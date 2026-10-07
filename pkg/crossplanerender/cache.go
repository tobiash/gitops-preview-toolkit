package crossplanerender

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	cprender "github.com/crossplane/cli/v2/cmd/crossplane/render"
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/kube-openapi/pkg/spec3"
)

func (s *Service) evaluate(
	ctx context.Context, sess *session, id string, in cprender.CompositionInputs, functions []pkgv1.Function,
) (*rendered, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	base, err := evaluationBase(in, functions, sess.config)
	if err != nil {
		return nil, fmt.Errorf("build evaluation key: %w", err)
	}
	if cached := sess.evaluations[id]; cached != nil && cached.base == base {
		witnesses, err := evaluationWitnesses(in, cached)
		if err != nil {
			return nil, fmt.Errorf("check evaluation dependencies: %w", err)
		}
		if witnesses == cached.witnesses {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return cached.result, cached.err
		}
	}
	delete(sess.evaluations, id)
	result, renderErr := s.execute(ctx, sess, in, functions)
	// Transport/startup failures must remain retryable. A real partial
	// response+error is the upstream pipeline-FATAL contract; it is cacheable
	// only together with all requested selectors and their current witnesses.
	usable := result != nil && result.response.GetComposite() != nil
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !usable {
		return result, renderErr
	}
	resources, schemas, err := evaluationSelectors(result)
	if err != nil {
		return result, fmt.Errorf("record evaluation dependencies: %w", err)
	}
	cached := &evaluation{base: base, resources: resources, schemas: schemas, result: result, err: renderErr}
	cached.witnesses, err = evaluationWitnesses(in, cached)
	if err != nil {
		return result, fmt.Errorf("record evaluation witnesses: %w", err)
	}
	sess.evaluations[id] = cached
	return result, renderErr
}

// One evaluation is retained per current producer, never a history of random
// executions. Dynamic witnesses include absence, so a pending dependency that
// appears later invalidates a cached fatal as well as a successful evaluation.
type evaluation struct {
	base      string
	witnesses string
	resources []*fnv1.ResourceSelector
	schemas   []*fnv1.SchemaSelector
	result    *rendered
	err       error
}

func evaluationBase(in cprender.CompositionInputs, functions []pkgv1.Function, cfg Config) (string, error) {
	in.RequiredResources = nil
	in.RequiredSchemas = nil
	in.FunctionAddrs = map[string]string{} // Proxy listen addresses are ephemeral.
	req, err := cprender.BuildCompositeRequest(in)
	if err != nil {
		return "", err
	}
	data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(req)
	if err != nil {
		return "", err
	}
	keys := make([]string, 0, len(functions))
	for _, fn := range functions {
		keys = append(keys, runtimeKey(fn))
	}
	return semanticHash(struct {
		Input     []byte
		Functions []string
		Config    Config
	}{Input: data, Functions: keys, Config: cfg})
}

func semanticHash(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

func evaluationSelectors(result *rendered) ([]*fnv1.ResourceSelector, []*fnv1.SchemaSelector, error) {
	resources := slices.Clone(result.resourceSelectors)
	schemas := slices.Clone(result.schemaSelectors)
	if out := result.response.GetComposite(); out != nil {
		for _, value := range out.GetRequiredResources() {
			selector := &fnv1.ResourceSelector{}
			data, err := value.MarshalJSON()
			if err != nil {
				return nil, nil, err
			}
			if err := protojson.Unmarshal(data, selector); err != nil {
				return nil, nil, fmt.Errorf("decode requested resource selector: %w", err)
			}
			resources = append(resources, selector)
		}
		for _, value := range out.GetRequiredSchemas() {
			selector := &fnv1.SchemaSelector{}
			data, err := value.MarshalJSON()
			if err != nil {
				return nil, nil, err
			}
			if err := protojson.Unmarshal(data, selector); err != nil {
				return nil, nil, fmt.Errorf("decode requested schema selector: %w", err)
			}
			schemas = append(schemas, selector)
		}
	}
	// Selectors from all negotiation iterations are dependencies, even if a
	// later response no longer requests an earlier input it already consumed.
	resources = uniqueSelectors(resources)
	schemas = uniqueSelectors(schemas)
	return resources, schemas, nil
}

func uniqueSelectors[T proto.Message](selectors []T) []T {
	byKey := map[string]T{}
	for _, selector := range selectors {
		data, _ := (proto.MarshalOptions{Deterministic: true}).Marshal(selector)
		byKey[string(data)] = selector
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := make([]T, 0, len(keys))
	for _, key := range keys {
		out = append(out, byKey[key])
	}
	return out
}

func evaluationWitnesses(in cprender.CompositionInputs, cached *evaluation) (string, error) {
	// Reproduce the externally seeded store, including the primary XR and
	// credential stringData conversion. Last-write wins for duplicate inputs.
	store := map[string]map[string]any{}
	put := func(o *unstructured.Unstructured) {
		key, _ := json.Marshal([]string{o.GetAPIVersion(), o.GetKind(), o.GetNamespace(), o.GetName()})
		store[string(key)] = o.Object
	}
	put(&in.CompositeResource.Unstructured)
	for _, resource := range in.RequiredResources {
		put(&resource)
	}
	for _, credential := range in.FunctionCredentials {
		data, err := json.Marshal(credential)
		if err != nil {
			return "", err
		}
		o := map[string]any{}
		if err := json.Unmarshal(data, &o); err != nil {
			return "", err
		}
		o["apiVersion"], o["kind"] = "v1", "Secret"
		put(&unstructured.Unstructured{Object: o})
	}
	keys := make([]string, 0, len(store))
	for key := range store {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	resourceWitnesses := make([][]map[string]any, 0, len(cached.resources))
	for _, selector := range cached.resources {
		matches := []map[string]any{} // Empty lists are explicit absence witnesses.
		for _, key := range keys {
			o := &unstructured.Unstructured{Object: store[key]}
			if resourceMatches(selector, o) {
				matches = append(matches, o.Object)
			}
		}
		resourceWitnesses = append(resourceWitnesses, matches)
	}
	schemaWitnesses := make([]any, 0, len(cached.schemas))
	for _, selector := range cached.schemas {
		witness, err := schemaWitness(selector, in.RequiredSchemas)
		if err != nil {
			return "", err
		}
		schemaWitnesses = append(schemaWitnesses, witness)
	}
	return semanticHash(struct {
		Resources [][]map[string]any
		Schemas   []any
	}{Resources: resourceWitnesses, Schemas: schemaWitnesses})
}

func resourceMatches(selector *fnv1.ResourceSelector, o *unstructured.Unstructured) bool {
	if selector.GetApiVersion() != o.GetAPIVersion() || selector.GetKind() != o.GetKind() {
		return false
	}
	if _, byName := selector.GetMatch().(*fnv1.ResourceSelector_MatchName); byName {
		// An omitted namespace on a named get means cluster scope, not an
		// all-namespace lookup. Label/list selectors can cross namespaces.
		return selector.GetMatchName() == o.GetName() && selector.GetNamespace() == o.GetNamespace()
	}
	if selector.GetNamespace() != "" && selector.GetNamespace() != o.GetNamespace() {
		return false
	}
	for key, value := range selector.GetMatchLabels().GetLabels() {
		if actual, present := o.GetLabels()[key]; !present || actual != value {
			return false
		}
	}
	return true
}

func schemaWitness(selector *fnv1.SchemaSelector, documents []spec3.OpenAPI) (any, error) {
	group, version, hasGroup := strings.Cut(selector.GetApiVersion(), "/")
	if !hasGroup {
		group, version = "", selector.GetApiVersion()
	}
	for _, document := range documents {
		if document.Components == nil {
			continue
		}
		keys := make([]string, 0, len(document.Components.Schemas))
		for key := range document.Components.Schemas {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			schema := document.Components.Schemas[key]
			if schema == nil {
				continue
			}
			gvks, _ := schema.Extensions["x-kubernetes-group-version-kind"].([]any)
			for _, value := range gvks {
				gvk, _ := value.(map[string]any)
				if gvk["group"] != group || gvk["version"] != version || gvk["kind"] != selector.GetKind() {
					continue
				}
				// Include only this schema and its referenced components, not
				// unrelated kinds that happen to share its group-version document.
				components := map[string]any{}
				var include func(string) error
				include = func(name string) error {
					if _, seen := components[name]; seen {
						return nil
					}
					data, err := json.Marshal(document.Components.Schemas[name])
					if err != nil {
						return err
					}
					var value any
					if err := json.Unmarshal(data, &value); err != nil {
						return err
					}
					components[name] = value
					return schemaReferences(value, include)
				}
				if err := include(key); err != nil {
					return nil, err
				}
				return components, nil
			}
		}
	}
	return nil, nil // A requested but absent schema is also a witness.
}

func schemaReferences(value any, include func(string) error) error {
	switch value := value.(type) {
	case map[string]any:
		if ref, ok := value["$ref"].(string); ok {
			if name, local := strings.CutPrefix(ref, "#/components/schemas/"); local {
				name = strings.ReplaceAll(strings.ReplaceAll(name, "~1", "/"), "~0", "~")
				if err := include(name); err != nil {
					return err
				}
			}
		}
		for _, child := range value {
			if err := schemaReferences(child, include); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := schemaReferences(child, include); err != nil {
				return err
			}
		}
	}
	return nil
}
