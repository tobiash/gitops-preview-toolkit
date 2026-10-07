package crossplanerender

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	cprender "github.com/crossplane/cli/v2/cmd/crossplane/render"
	xrpkg "github.com/crossplane/cli/v2/pkg/xr"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composite"
	"github.com/crossplane/crossplane-runtime/v2/pkg/xcrd"
	apiextensionsv1 "github.com/crossplane/crossplane/apis/v2/apiextensions/v1"
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/kube-openapi/pkg/spec3"
)

func effectiveFunction(fn *unstructured.Unstructured, cfg Config) (pkgv1.Function, error) {
	f := pkgv1.Function{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(fn.Object, &f); err != nil {
		return f, unresolved("invalid-function", "Function %q: %v", fn.GetName(), err)
	}
	if f.Spec.Package == "" {
		return f, unresolved("invalid-function", "Function %q has no spec.package", f.Name)
	}
	if f.Annotations == nil {
		f.Annotations = map[string]string{}
	}
	for key := range f.Annotations {
		// Also remove CLI-owned InProcess annotations and future runtime knobs.
		if strings.HasPrefix(key, "render.crossplane.io/") {
			delete(f.Annotations, key)
		}
	}
	mode := cfg.Runtime
	if target := cfg.DevelopmentTargets[f.Name]; target != "" {
		mode = "Development"
		f.Annotations[cprender.AnnotationKeyRuntimeDevelopmentTarget] = target
	}
	if mode == "Development" && cfg.DevelopmentTargets[f.Name] == "" {
		return f, unresolved("development-target-unapproved", "Function %q has no trusted development target", f.Name)
	}
	f.Annotations[cprender.AnnotationKeyRuntime] = mode
	if mode == "Docker" {
		f.Annotations[cprender.AnnotationKeyRuntimeDockerCleanup] = "Remove"
		keys := make([]string, 0, len(cfg.DockerEnv))
		for key := range cfg.DockerEnv {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		pairs := make([]string, 0, len(keys))
		for _, key := range keys {
			pairs = append(pairs, key+"="+cfg.DockerEnv[key])
		}
		if len(pairs) != 0 {
			f.Annotations[cprender.AnnotationKeyRuntimeEnvironmentVariables] = strings.Join(pairs, ",")
		}
	}
	return f, nil
}

func resolveFunctions(comp *apiextensionsv1.Composition, inventory []inventoryResource, cfg Config) ([]pkgv1.Function, error) {
	if comp.Spec.Mode != apiextensionsv1.CompositionModePipeline || len(comp.Spec.Pipeline) == 0 {
		return nil, unresolved("unsupported-composition", "Composition %q must contain a nonempty Pipeline", comp.Name)
	}
	functions := []pkgv1.Function{}
	seen := map[string]bool{}
	for _, step := range comp.Spec.Pipeline {
		name := step.FunctionRef.Name
		if seen[name] {
			continue
		}
		seen[name] = true
		var found *unstructured.Unstructured
		for _, item := range inventory {
			o := item.object
			validVersion := o.GetAPIVersion() == "pkg.crossplane.io/v1" || o.GetAPIVersion() == "pkg.crossplane.io/v1beta1"
			if validVersion && o.GetKind() == "Function" && o.GetName() == name {
				if found != nil {
					return nil, unresolved("function-ambiguous", "multiple Function manifests named %q", name)
				}
				found = o
			}
		}
		if found == nil {
			return nil, unresolved("function-unavailable", "pipeline step %q requires Function %q", step.Step, name)
		}
		fn, err := effectiveFunction(found, cfg)
		if err != nil {
			return nil, err
		}
		functions = append(functions, fn)
	}
	if len(functions) > cfg.MaxFunctions {
		return nil, unresolved("runtime-limit", "Composition requires more than %d functions", cfg.MaxFunctions)
	}
	return functions, nil
}

func compositionInputs(
	xr inventoryResource, d definition, selected *unstructured.Unstructured, inventory []inventoryResource,
) (cprender.CompositionInputs, error) {
	in := cprender.CompositionInputs{
		CompositeResource: composite.New(), Composition: &apiextensionsv1.Composition{},
		FunctionAddrs: map[string]string{}, ObservedResources: []composed.Unstructured{},
		RequiredResources: []unstructured.Unstructured{}, FunctionCredentials: []corev1.Secret{},
		RequiredSchemas: []spec3.OpenAPI{}, XRD: d.object.DeepCopy(),
	}
	if d.scope == "Namespaced" && xr.object.GetNamespace() == "" {
		return in, unresolved("xr-namespace-required", "namespaced XR %q must declare a namespace", xr.object.GetName())
	}
	if d.scope != "Namespaced" && xr.object.GetNamespace() != "" {
		return in, unresolved("invalid-xr-scope", "cluster scoped XR %q must not declare a namespace", xr.object.GetName())
	}
	if xr.object.GetName() == "" {
		return in, unresolved("unnamed-xr", "an unnamed XR has no cluster identity and cannot be reconciled")
	}
	in.CompositeResource.Unstructured = *xr.object.DeepCopy()
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(selected.Object, in.Composition); err != nil {
		return in, unresolved("invalid-composition", "%v", err)
	}
	typedXRD := &apiextensionsv1.CompositeResourceDefinition{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(d.object.Object, typedXRD); err != nil {
		return in, unresolved("invalid-xrd", "%v", err)
	}
	if err := xrpkg.ApplyXRDDefaults(in.CompositeResource.GetUnstructured(), typedXRD); err != nil {
		return in, unresolved("xrd-defaulting-failed", "%v", err)
	}
	for _, item := range inventory {
		// The engine seeds its primary XR itself. An older inventory copy must
		// not overwrite the defaulted primary object in its in-memory store.
		if item.resource.ID == xr.resource.ID || item.resource.Logical || item.object.GetName() == "" {
			continue
		}
		in.RequiredResources = append(in.RequiredResources, *item.object.DeepCopy())
	}
	for _, step := range in.Composition.Spec.Pipeline {
		for _, credential := range step.Credentials {
			if credential.SecretRef == nil || string(credential.Source) != "Secret" {
				return in, unresolved("unsupported-credentials", "step %q has unsupported credentials", step.Step)
			}
			found := false
			for _, item := range inventory {
				o := item.object
				if o.GetAPIVersion() != "v1" || o.GetKind() != "Secret" {
					continue
				}
				if o.GetName() != credential.SecretRef.Name || o.GetNamespace() != credential.SecretRef.Namespace {
					continue
				}
				secret := corev1.Secret{}
				if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, &secret); err != nil {
					return in, unresolved("invalid-credentials", "%v", err)
				}
				// Render has no API server to perform Secret stringData conversion.
				if secret.Data == nil {
					secret.Data = map[string][]byte{}
				}
				for key, value := range secret.StringData {
					secret.Data[key] = []byte(value)
				}
				secret.StringData = nil
				in.FunctionCredentials = append(in.FunctionCredentials, secret)
				found = true
				break
			}
			if !found {
				return in, unresolved("credentials-unavailable", "step %q requires Secret %s/%s",
					step.Step, credential.SecretRef.Namespace, credential.SecretRef.Name)
			}
		}
	}
	schemas, err := definitionSchemas(inventory)
	if err != nil {
		return in, err
	}
	in.RequiredSchemas = schemas
	return in, nil
}

func definitionSchemas(inventory []inventoryResource) ([]spec3.OpenAPI, error) {
	documents := map[string]map[string]any{}
	for _, item := range inventory {
		if item.object.GetKind() != "CompositeResourceDefinition" {
			continue
		}
		d, err := readDefinition(item.object)
		if err != nil {
			continue // Invalid definitions are diagnosed separately during discovery.
		}
		typed := &apiextensionsv1.CompositeResourceDefinition{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(d.object.Object, typed); err != nil {
			return nil, unresolved("invalid-xrd-schema", "%v", err)
		}
		crd, err := xcrd.ForCompositeResource(typed)
		if err != nil {
			return nil, unresolved("invalid-xrd-schema", "%v", err)
		}
		for _, version := range crd.Spec.Versions {
			if !version.Served || version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
				continue
			}
			key := d.group + "/" + version.Name
			if documents[key] == nil {
				documents[key] = map[string]any{}
			}
			data, err := json.Marshal(version.Schema.OpenAPIV3Schema)
			if err != nil {
				return nil, err
			}
			schema := map[string]any{}
			if err := json.Unmarshal(data, &schema); err != nil {
				return nil, err
			}
			schema["x-kubernetes-group-version-kind"] = []any{
				map[string]any{"group": d.group, "version": version.Name, "kind": d.kind},
			}
			documents[key][d.group+"."+version.Name+"."+d.kind] = schema
		}
	}
	keys := make([]string, 0, len(documents))
	for key := range documents {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := make([]spec3.OpenAPI, 0, len(keys))
	for _, key := range keys {
		data, err := json.Marshal(map[string]any{
			"openapi": "3.0.0", "info": map[string]any{"title": key, "version": "v1"},
			"paths": map[string]any{}, "components": map[string]any{"schemas": documents[key]},
		})
		if err != nil {
			return nil, fmt.Errorf("marshal XRD schema: %w", err)
		}
		doc := spec3.OpenAPI{}
		if err := json.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("decode XRD OpenAPI schema: %w", err)
		}
		out = append(out, doc)
	}
	return out, nil
}
