package fluxrender

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"text/template"
	"text/template/parse"

	"github.com/Masterminds/sprig/v3"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
	kubejson "sigs.k8s.io/json"
	"sigs.k8s.io/kustomize/api/resource"
	"sigs.k8s.io/yaml"
)

const (
	maxResourceSetOutputs    = 10000
	maxResourceSetBytes      = 16 << 20
	maxResourceSetProviders  = 128
	maxResourceSetOperations = 65536
)

func renderResourceSet(ctx context.Context, control *resource.Resource, inventory *render.Render) ([]plugin.Resource, error) {
	if len(control.MustYaml()) > maxResourceSetBytes {
		return nil, fmt.Errorf("ResourceSet exceeds %d input bytes", maxResourceSetBytes)
	}
	object, err := control.Map()
	if err != nil {
		return nil, err
	}
	if control.GetApiVersion() != resourceSetAPIVersion {
		return nil, fmt.Errorf("unsupported ResourceSet apiVersion %q", control.GetApiVersion())
	}
	if err := validateResourceSetSpec(object); err != nil {
		return nil, err
	}
	if strings.EqualFold(control.GetAnnotations()["fluxcd.controlplane.io/reconcile"], "disabled") {
		return nil, fmt.Errorf("disabled ResourceSet requires retained live state; offline rendering is unsupported")
	}
	var metadata resourceSetMetadata
	if err := decodeResourceSetField(object, &metadata, "spec", "commonMetadata"); err != nil {
		return nil, err
	}
	inputs, hasProviders, err := resourceSetInputs(ctx, object, control, inventory)
	if err != nil {
		return nil, err
	}
	if len(inputs) == 0 && hasProviders {
		return []plugin.Resource{}, nil
	}
	noInputs := len(inputs) == 0
	if len(inputs) == 0 {
		inputs = []map[string]any{{}}
	}
	var templates []map[string]any
	if err := decodeResourceSetField(object, &templates, "spec", "resources"); err != nil {
		return nil, err
	}
	outputs := []plugin.Resource{}
	seen := map[string]bool{}
	texts := []string{}
	for _, raw := range templates {
		if raw == nil {
			return nil, fmt.Errorf("ResourceSet resources must contain objects")
		}
		data, err := yaml.Marshal(raw)
		if err != nil {
			return nil, err
		}
		texts = append(texts, string(data))
	}
	text, _, err := unstructured.NestedString(object, "spec", "resourcesTemplate")
	if err != nil {
		return nil, err
	}
	if text != "" {
		texts = append(texts, text)
	}
	budget := maxResourceSetBytes
	outputBytes := 0
	operations := maxResourceSetOperations
	check := func() (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		operations--
		if operations < 0 {
			return "", fmt.Errorf("ResourceSet template operation limit exceeded (%d)", maxResourceSetOperations)
		}
		return "", nil
	}
	for templateIndex, text := range texts {
		// Upstream's no-input object-form branch adds the object without
		// conditional exclusion or deduplication. YAML templates always filter.
		noInputObject := noInputs && templateIndex < len(templates)
		for _, in := range inputs {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			tp, err := template.New("resourceset").Delims("<<", ">>").Funcs(resourceSetTemplateFunctions()).Funcs(template.FuncMap{
				"inputs":        func() any { return in },
				"_previewCheck": check,
			}).Option("missingkey=error").Parse(text)
			if err != nil {
				return nil, err
			}
			if err := guardResourceSetTemplate(tp); err != nil {
				return nil, err
			}
			var buf bytes.Buffer
			if err := tp.Execute(&resourceSetWriter{ctx: ctx, buffer: &buf, remaining: &budget}, nil); err != nil {
				return nil, err
			}
			decoder := kubeyaml.NewYAMLOrJSONDecoder(&buf, 4096)
			for {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				// Kubernetes YAML conversion keeps dates as strings and resolves
				// YAML 1.1 booleans. Raw JSON avoids its default float64 decoding.
				var raw json.RawMessage
				if err := decoder.Decode(&raw); err == io.EOF {
					break
				} else if err != nil {
					return nil, fmt.Errorf("ResourceSet output YAML: %w", err)
				}
				var object map[string]any
				if err := kubejson.UnmarshalCaseSensitivePreserveInts(raw, &object); err != nil {
					return nil, fmt.Errorf("ResourceSet output JSON: %w", err)
				}
				if len(object) == 0 {
					continue
				}
				u := &unstructured.Unstructured{Object: object}
				if !noInputObject && u.GetAnnotations()["fluxcd.controlplane.io/reconcile"] == "disabled" {
					continue
				}
				_, err := plugin.ObjectID(object)
				if err != nil {
					return nil, fmt.Errorf("ResourceSet output: %w", err)
				}
				// ResourceSet deduplication uses cluster identity, excluding the
				// served API version; the public Resource ID still retains it.
				id := strings.Join([]string{u.GroupVersionKind().Group, u.GetKind(), u.GetNamespace(), u.GetName()}, "/")
				if !noInputObject && seen[id] {
					continue
				}
				if len(outputs) >= maxResourceSetOutputs {
					return nil, fmt.Errorf("ResourceSet exceeds %d output resources", maxResourceSetOutputs)
				}
				seen[id] = true
				if err := validateResourceSetTransforms(object); err != nil {
					return nil, err
				}
				ownerLabels := u.GetLabels()
				if ownerLabels == nil {
					ownerLabels = map[string]string{}
				}
				ownerLabels["resourceset.fluxcd.controlplane.io/name"] = control.GetName()
				ownerLabels["resourceset.fluxcd.controlplane.io/namespace"] = control.GetNamespace()
				for key, value := range metadata.Labels {
					ownerLabels[key] = value
				}
				u.SetLabels(ownerLabels)
				annotations := u.GetAnnotations()
				if annotations == nil {
					annotations = map[string]string{}
				}
				for key, value := range metadata.Annotations {
					annotations[key] = value
				}
				if len(annotations) > 0 {
					u.SetAnnotations(annotations)
				}
				data, err := yaml.Marshal(object)
				if err != nil {
					return nil, err
				}
				if len(data) > maxResourceSetBytes-outputBytes {
					return nil, fmt.Errorf("ResourceSet exceeds %d final output bytes", maxResourceSetBytes)
				}
				outputBytes += len(data)
				out, err := plugin.ParseResource(data, plugin.Provenance{Kind: "ResourceSet", Name: control.GetName(), Namespace: control.GetNamespace()})
				if err != nil {
					return nil, err
				}
				outputs = append(outputs, out)
			}
		}
	}
	return outputs, nil
}

// Check cancellation and work budgets even inside output-free ranges or
// recursive named templates, where a bounded writer alone cannot intervene.
func guardResourceSetTemplate(tp *template.Template) error {
	if tp.Lookup("_previewGuard") != nil {
		return fmt.Errorf("ResourceSet template name _previewGuard is reserved")
	}
	guard, err := tp.New("_previewGuard").Parse("<< _previewCheck >>")
	if err != nil {
		return err
	}
	action := guard.Root.Nodes[0]
	var visit func(*parse.ListNode)
	visit = func(list *parse.ListNode) {
		if list == nil {
			return
		}
		for _, node := range list.Nodes {
			switch node := node.(type) {
			case *parse.RangeNode:
				visit(node.List)
				visit(node.ElseList)
			case *parse.IfNode:
				visit(node.List)
				visit(node.ElseList)
			case *parse.WithNode:
				visit(node.List)
				visit(node.ElseList)
			}
		}
		list.Nodes = append([]parse.Node{action.Copy()}, list.Nodes...)
	}
	for _, entry := range tp.Templates() {
		if entry != guard && entry.Tree != nil {
			visit(entry.Root)
		}
	}
	return nil
}

func resourceSetToYAML(value any) (string, error) {
	data, err := yaml.Marshal(value)
	return string(data), err
}

// A deliberately bounded subset of upstream's hermetic template functions.
// Allocating generators (until, seq, repeat), random/crypto functions and
// environment/network functions are unavailable and fail at parse time.
func resourceSetTemplateFunctions() template.FuncMap {
	source := sprig.HermeticTxtFuncMap()
	functions := template.FuncMap{}
	for _, name := range strings.Fields(`default empty coalesce all any ternary
		quote squote toString toStrings split splitList splitn
		trim trimAll trimSuffix trimPrefix upper lower title untitle
		contains hasPrefix hasSuffix cat nospace trunc substr initials
		snakecase camelcase kebabcase swapcase
		b64enc b64dec b32enc b32dec toJson mustToJson fromJson mustFromJson
		toRawJson mustToRawJson
		list dict get hasKey keys values pluck pick omit dig
		first mustFirst last mustLast rest mustRest initial mustInitial
		reverse mustReverse uniq mustUniq compact mustCompact
		without mustWithout has mustHas append mustAppend prepend mustPrepend
		concat slice mustSlice sortAlpha len
		add add1 sub div mod mul max min floor ceil round int int64 atoi float64
		addf add1f subf divf mulf maxf minf
		typeOf typeIs typeIsLike kindOf kindIs deepEqual fail`) {
		if function, ok := source[name]; ok {
			functions[name] = function
		}
	}
	functions["indent"] = func(count int, value string) (string, error) {
		if count < 0 || count > 4096 || len(value) > maxResourceSetBytes/(count+1) {
			return "", fmt.Errorf("ResourceSet indent exceeds output limit")
		}
		padding := strings.Repeat(" ", count)
		return padding + strings.ReplaceAll(value, "\n", "\n"+padding), nil
	}
	functions["nindent"] = func(count int, value string) (string, error) {
		text, err := functions["indent"].(func(int, string) (string, error))(count, value)
		return "\n" + text, err
	}
	functions["toYaml"], functions["mustToYaml"] = resourceSetToYAML, resourceSetToYAML
	functions["printf"] = func(format string, values ...any) (string, error) {
		// Widths, precision and argument indexing can multiply small inputs
		// into arbitrarily large allocations inside fmt before any Write.
		for i := 0; i < len(format); i++ {
			if format[i] != '%' {
				continue
			}
			i++
			if i == len(format) || !strings.ContainsRune("%svdqtfgxXobTceEGU", rune(format[i])) {
				return "", fmt.Errorf("ResourceSet printf supports only bare format verbs (no widths, precision or indexing)")
			}
		}
		return fmt.Sprintf(format, values...), nil
	}
	functions["print"], functions["println"] = fmt.Sprint, fmt.Sprintln
	functions["html"], functions["js"], functions["urlquery"] = template.HTMLEscaper, template.JSEscaper, template.URLQueryEscaper
	for name, function := range functions {
		functions[name] = boundedResourceSetFunction(function)
	}
	return functions
}

// Bound the expanded value graph, not merely its shallow size: lists can share
// sublists and grow exponentially without allocating their contents yet.
func boundedResourceSetFunction(function any) any {
	value := reflect.ValueOf(function)
	typ := value.Type()
	arguments := make([]reflect.Type, typ.NumIn())
	for i := range arguments {
		arguments[i] = typ.In(i)
	}
	results := make([]reflect.Type, typ.NumOut())
	for i := range results {
		results[i] = typ.Out(i)
	}
	errorType := reflect.TypeFor[error]()
	hasError := len(results) > 0 && results[len(results)-1] == errorType
	if !hasError {
		results = append(results, errorType)
	}
	boundedType := reflect.FuncOf(arguments, results, typ.IsVariadic())
	return reflect.MakeFunc(boundedType, func(args []reflect.Value) []reflect.Value {
		fail := func(err error) []reflect.Value {
			out := make([]reflect.Value, len(results))
			for i, result := range results {
				out[i] = reflect.Zero(result)
			}
			out[len(out)-1] = reflect.ValueOf(&err).Elem()
			return out
		}
		if err := checkResourceSetMaterialization(args); err != nil {
			return fail(err)
		}
		var out []reflect.Value
		if typ.IsVariadic() {
			out = value.CallSlice(args)
		} else {
			out = value.Call(args)
		}
		values := out
		if hasError {
			if !out[len(out)-1].IsNil() {
				return out
			}
			values = out[:len(out)-1]
		}
		if err := checkResourceSetMaterialization(values); err != nil {
			return fail(err)
		}
		if !hasError {
			out = append(out, reflect.Zero(errorType))
		}
		return out
	}).Interface()
}

func checkResourceSetMaterialization(values []reflect.Value) error {
	bytesLeft, nodesLeft := 1<<20, 65536
	var visit func(reflect.Value, int) error
	visit = func(value reflect.Value, depth int) error {
		nodesLeft--
		if nodesLeft < 0 || depth > 64 || bytesLeft < 0 {
			return fmt.Errorf("ResourceSet function materialization limit exceeded (1 MiB, 65536 values, depth 64)")
		}
		if !value.IsValid() {
			return nil
		}
		switch value.Kind() {
		case reflect.Interface, reflect.Pointer:
			if !value.IsNil() {
				return visit(value.Elem(), depth+1)
			}
		case reflect.String:
			bytesLeft -= value.Len()
			if bytesLeft < 0 {
				return fmt.Errorf("ResourceSet function materialization limit exceeded (1 MiB)")
			}
		case reflect.Slice, reflect.Array:
			if value.Len() > nodesLeft {
				return fmt.Errorf("ResourceSet function materialization limit exceeded (65536 values)")
			}
			for i := 0; i < value.Len(); i++ {
				if err := visit(value.Index(i), depth+1); err != nil {
					return err
				}
			}
		case reflect.Map:
			iterator := value.MapRange()
			for iterator.Next() {
				if err := visit(iterator.Key(), depth+1); err != nil {
					return err
				}
				if err := visit(iterator.Value(), depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, value := range values {
		if err := visit(value, 0); err != nil {
			return err
		}
	}
	return nil
}

type resourceSetWriter struct {
	ctx       context.Context
	buffer    *bytes.Buffer
	remaining *int
}

func (w *resourceSetWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if len(data) > *w.remaining {
		return 0, fmt.Errorf("ResourceSet exceeds %d rendered bytes", maxResourceSetBytes)
	}
	*w.remaining -= len(data)
	return w.buffer.Write(data)
}

func validateResourceSetSpec(object map[string]any) error {
	value, found, err := unstructured.NestedFieldNoCopy(object, "spec")
	if err != nil {
		return err
	}
	spec, ok := value.(map[string]any)
	if !found || !ok {
		return fmt.Errorf("ResourceSet spec is required")
	}
	for field := range spec {
		switch field {
		case "inputs", "inputsFrom", "inputStrategy", "resources", "resourcesTemplate", "commonMetadata", "dependsOn", "wait", "serviceAccountName":
		default:
			return fmt.Errorf("unsupported ResourceSet spec.%s", field)
		}
	}
	if _, hasResources := spec["resources"]; !hasResources && spec["resourcesTemplate"] == nil {
		return fmt.Errorf("ResourceSet requires resources or resourcesTemplate")
	}
	var metadata resourceSetMetadata
	if err := decodeResourceSetField(object, &metadata, "spec", "commonMetadata"); err != nil {
		return err
	}
	return validateResourceSetAnnotations(metadata.Annotations)
}

type resourceSetMetadata struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

func validateResourceSetAnnotations(annotations map[string]string) error {
	for key := range annotations {
		if strings.HasPrefix(key, "fluxcd.controlplane.io/") {
			switch strings.TrimPrefix(key, "fluxcd.controlplane.io/") {
			case "copyFrom", "convertKubeConfigFrom", "checksumFrom":
				return fmt.Errorf("unsupported ResourceSet render-affecting annotation %q", key)
			}
		}
	}
	return nil
}

func validateResourceSetTransforms(value any) error {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "metadata" {
				if metadata, ok := child.(map[string]any); ok {
					for _, field := range []string{"labels", "annotations"} {
						values, _, err := unstructured.NestedStringMap(metadata, field)
						if err != nil {
							return fmt.Errorf("ResourceSet output metadata.%s: %w", field, err)
						}
						if field == "annotations" {
							if err := validateResourceSetAnnotations(values); err != nil {
								return err
							}
						}
					}
				}
			}
			if err := validateResourceSetTransforms(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := validateResourceSetTransforms(child); err != nil {
				return err
			}
		}
	}
	return nil
}
