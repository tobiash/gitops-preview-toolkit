package crossplanerender

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	cprender "github.com/crossplane/cli/v2/cmd/crossplane/render"
	renderproto "github.com/crossplane/cli/v2/proto/render/v1alpha1"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
	"github.com/go-logr/logr"
	"google.golang.org/protobuf/types/known/structpb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
)

func testResource(t *testing.T, text string) inventoryResource {
	t.Helper()
	resource, err := plugin.ParseResource([]byte(text), plugin.Provenance{})
	if err != nil {
		t.Fatal(err)
	}
	object, err := plugin.Object(resource)
	if err != nil {
		t.Fatal(err)
	}
	return inventoryResource{resource: resource, object: &unstructured.Unstructured{Object: object}}
}

const testXRD = `apiVersion: apiextensions.crossplane.io/v2
kind: CompositeResourceDefinition
metadata:
  name: apps.example.org
spec:
  group: example.org
  names:
    kind: App
    plural: apps
  versions:
  - name: v1
    served: true
    referenceable: true
    schema:
      openAPIV3Schema:
        type: object
        properties:
          spec:
            type: object
            properties:
              replicas:
                type: integer
                default: 2
          status:
            type: object
`

const testXR = `apiVersion: example.org/v1
kind: App
metadata:
  name: demo
  namespace: apps
spec: {}
`

const testComposition = `apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: small
  labels:
    size: small
spec:
  compositeTypeRef:
    apiVersion: example.org/v1
    kind: App
  mode: Pipeline
  pipeline:
  - step: template
    functionRef:
      name: templating
`

const testFunction = `apiVersion: pkg.crossplane.io/v1
kind: Function
metadata:
  name: templating
  annotations:
    render.crossplane.io/runtime: Development
    render.crossplane.io/runtime-development-target: malicious.example:9443
    render.crossplane.io/runtime-docker-image: attacker/image
    render.crossplane.io/runtime-docker-env: TOKEN=untrusted
    render.crossplane.io/runtime-docker-name: shared-across-permadiffs
spec:
  package: xpkg.crossplane.io/crossplane-contrib/function-go-templating:v0.12.0
`

func fixtureInventory(t *testing.T) []inventoryResource {
	t.Helper()
	return []inventoryResource{
		testResource(t, testXRD), testResource(t, testXR),
		testResource(t, testComposition), testResource(t, testFunction),
	}
}

func setField(t *testing.T, o *unstructured.Unstructured, value any, fields ...string) {
	t.Helper()
	if err := unstructured.SetNestedField(o.Object, value, fields...); err != nil {
		t.Fatal(err)
	}
}

func TestCompositionSelection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		configure func(*testing.T, []inventoryResource)
		want      string
		code      string
	}{
		{name: "unique compatible", want: "small"},
		{name: "explicit reference", want: "large", configure: func(t *testing.T, inv []inventoryResource) {
			setField(t, inv[1].object, "large", "spec", "crossplane", "compositionRef", "name")
		}},
		{name: "default", want: "small", configure: func(t *testing.T, inv []inventoryResource) {
			setField(t, inv[0].object, "small", "spec", "defaultCompositionRef", "name")
		}},
		{name: "enforced overrides explicit", want: "small", configure: func(t *testing.T, inv []inventoryResource) {
			setField(t, inv[0].object, "small", "spec", "enforcedCompositionRef", "name")
			setField(t, inv[1].object, "large", "spec", "crossplane", "compositionRef", "name")
		}},
		{name: "selector", want: "small", configure: func(t *testing.T, inv []inventoryResource) {
			setField(t, inv[1].object, "small", "spec", "crossplane", "compositionSelector", "matchLabels", "size")
		}},
		{name: "selector expressions", want: "small", configure: func(t *testing.T, inv []inventoryResource) {
			setField(t, inv[1].object, []any{map[string]any{
				"key": "size", "operator": "In", "values": []any{"small"},
			}}, "spec", "crossplane", "compositionSelector", "matchExpressions")
		}},
		{name: "ambiguous", code: "composition-ambiguous"},
		{name: "missing reference", code: "composition-unavailable", configure: func(t *testing.T, inv []inventoryResource) {
			setField(t, inv[1].object, "missing", "spec", "crossplane", "compositionRef", "name")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			inv := fixtureInventory(t)
			if tt.name != "unique compatible" {
				large := testResource(t, testComposition)
				large.resource.ID += "-large"
				large.object.SetName("large")
				large.object.SetLabels(map[string]string{"size": "large"})
				inv = append(inv, large)
			}
			if tt.configure != nil {
				tt.configure(t, inv)
			}
			d, err := readDefinition(inv[0].object)
			if err != nil {
				t.Fatal(err)
			}
			comp, err := selectComposition(inv[1].object, d, inv)
			if tt.code != "" {
				assertCode(t, err, tt.code)
				return
			}
			if err != nil || comp.GetName() != tt.want {
				t.Fatalf("selection = %v, %v; want %s", comp, err, tt.want)
			}
		})
	}
}

func assertCode(t *testing.T, err error, code string) {
	t.Helper()
	var resolution *resolutionError
	if !errors.As(err, &resolution) || resolution.code != code {
		t.Fatalf("error = %v; want code %s", err, code)
	}
}

func TestDefinitionScopesAndServedVersions(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		version string
		scope   string
		want    string
	}{
		{name: "v1 default", version: "v1", want: "LegacyCluster"},
		{name: "v2 default", version: "v2", want: "Namespaced"},
		{name: "v2 cluster", version: "v2", scope: "Cluster", want: "Cluster"},
		{name: "v1 modern", version: "v1", scope: "Namespaced", want: "Namespaced"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			inv := fixtureInventory(t)
			inv[0].object.SetAPIVersion("apiextensions.crossplane.io/" + tt.version)
			if tt.scope != "" {
				setField(t, inv[0].object, tt.scope, "spec", "scope")
			}
			d, err := readDefinition(inv[0].object)
			if err != nil || d.scope != tt.want {
				t.Fatalf("scope = %s, %v; want %s", d.scope, err, tt.want)
			}
			if !d.matches(inv[1].object) {
				t.Fatal("served XR not discovered")
			}
			inv[1].object.SetAPIVersion("example.org/v2")
			if d.matches(inv[1].object) {
				t.Fatal("unserved XR discovered")
			}
			if tt.want == "LegacyCluster" {
				setField(t, inv[1].object, "legacy", "spec", "compositionRef", "name")
				if stringField(controls(inv[1].object, d), "compositionRef", "name") != "legacy" {
					t.Fatal("legacy controls not read")
				}
			}
		})
	}
}

func TestPinnedRevision(t *testing.T) {
	t.Parallel()
	inv := fixtureInventory(t)
	rev := testResource(t, testComposition)
	rev.object.SetKind("CompositionRevision")
	rev.object.SetName("small-42")
	rev.object.SetLabels(map[string]string{"crossplane.io/composition-name": "small"})
	setField(t, rev.object, int64(42), "spec", "revision")
	setField(t, inv[1].object, "small-42", "spec", "crossplane", "compositionRevisionRef", "name")
	d, err := readDefinition(inv[0].object)
	if err != nil {
		t.Fatal(err)
	}
	_, err = selectComposition(inv[1].object, d, inv)
	assertCode(t, err, "unsupported-pinned-revision")
	inv = append(inv, rev)
	comp, err := selectComposition(inv[1].object, d, inv)
	if err != nil || comp.GetKind() != "Composition" || comp.GetName() != "small" {
		t.Fatalf("revision selection = %v, %v", comp, err)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(comp.Object, "spec", "revision"); found {
		t.Fatal("revision-only field leaked to Composition")
	}
	setField(t, d.object, "other", "spec", "enforcedCompositionRef", "name")
	_, err = selectComposition(inv[1].object, d, inv)
	assertCode(t, err, "revision-enforcement-conflict")
}

func TestRuntimeConfigurationTrust(t *testing.T) {
	t.Parallel()
	inv := fixtureInventory(t)
	cfg, _, err := parseConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	fn, err := effectiveFunction(inv[3].object, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if fn.Annotations[cprender.AnnotationKeyRuntime] != "Docker" {
		t.Fatal("untrusted runtime annotation changed runtime")
	}
	for _, key := range []string{
		cprender.AnnotationKeyRuntimeDevelopmentTarget, cprender.AnnotationKeyRuntimeDockerImage,
		cprender.AnnotationKeyRuntimeEnvironmentVariables, cprender.AnnotationKeyRuntimeNamedContainer,
	} {
		if _, present := fn.Annotations[key]; present {
			t.Fatalf("untrusted annotation survived: %s", key)
		}
	}
	cfg.Runtime = "Development"
	_, err = effectiveFunction(inv[3].object, cfg)
	assertCode(t, err, "development-target-unapproved")
	cfg.DevelopmentTargets["templating"] = "127.0.0.1:9443"
	fn, err = effectiveFunction(inv[3].object, cfg)
	if err != nil || fn.Annotations[cprender.AnnotationKeyRuntimeDevelopmentTarget] != "127.0.0.1:9443" {
		t.Fatalf("trusted target not applied: %v", err)
	}
	for _, raw := range []string{
		`{"runtime":"InProcess"}`, `{"runtime":"bad"}`, `{"timeout":"0s"}`,
		`{"developmentTargets":{"f":"localhost"}}`, `{"maxFunctions":0}`, `{"unknown":true}`, `null`,
	} {
		if _, _, err := parseConfig(json.RawMessage(raw)); err == nil {
			t.Fatalf("invalid configuration accepted: %s", raw)
		}
	}
	service := New(logr.Discard())
	if _, err := service.OpenRender(t.Context(), &plugin.OpenRequest{LocalOnly: true}); err == nil {
		t.Fatal("localOnly execution accepted")
	}
}

func openTestSession(t *testing.T, s *Service) string {
	t.Helper()
	opened, err := s.OpenRender(t.Context(), &plugin.OpenRequest{Fresh: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return opened.Session
}

func resourcesOf(inv []inventoryResource) []plugin.Resource {
	out := make([]plugin.Resource, 0, len(inv))
	for _, item := range inv {
		out = append(out, item.resource)
	}
	return out
}

func outputResponse(t *testing.T, objects ...map[string]any) *rendered {
	t.Helper()
	outputs := []*structpb.Struct{}
	for _, object := range objects {
		s, err := structpb.NewStruct(object)
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, s)
	}
	xr, err := structpb.NewStruct(map[string]any{
		"apiVersion": "example.org/v1", "kind": "App",
		"metadata": map[string]any{"name": "demo", "namespace": "apps"},
		"status":   map[string]any{"answer": "evidence only"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &rendered{
		response: &renderproto.RenderResponse{Output: &renderproto.RenderResponse_Composite{
			Composite: &renderproto.CompositeOutput{CompositeResource: xr, ComposedResources: outputs},
		}}, declaredNames: map[string]string{}, functionTrace: []json.RawMessage{},
	}
}

func TestExpandCachesUnchangedInputsAndNeverObservesDesired(t *testing.T) {
	t.Parallel()
	service := New(logr.Discard())
	sessionID := openTestSession(t, service)
	inv := fixtureInventory(t)
	var calls int
	service.execute = func(
		ctx context.Context, sess *session, in cprender.CompositionInputs, fns []pkgv1.Function,
	) (*rendered, error) {
		calls++
		if len(in.ObservedResources) != 0 {
			t.Fatal("desired resources used as observed state")
		}
		if stringField(&in.CompositeResource.Unstructured, "spec", "replicas") != "" {
			t.Fatal("integer default decoded as a string")
		}
		value, _, _ := unstructured.NestedFieldNoCopy(in.CompositeResource.Object, "spec", "replicas")
		if value != int64(2) && value != float64(2) {
			t.Fatalf("XRD default not applied: %#v", value)
		}
		if len(in.RequiredSchemas) == 0 || len(in.RequiredResources) < 2 {
			t.Fatal("inventory or XRD schemas not supplied")
		}
		for _, resource := range in.RequiredResources {
			if resource.GetKind() == "App" && resource.GetName() == "demo" {
				t.Fatal("original inventory XR would overwrite defaulted XR")
			}
		}
		result := outputResponse(t, map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{
				"name": "values", "namespace": "apps",
				"annotations": map[string]any{"crossplane.io/composition-resource-name": "values"},
			}, "data": map[string]any{"answer": "42"},
		})
		result.declaredNames["values"] = "values"
		return result, nil
	}
	// Missing Functions produce current diagnostics rather than executing.
	request := &plugin.ExpandRequest{Session: sessionID, Resources: resourcesOf(inv[:3])}
	response, err := service.Expand(t.Context(), request)
	if err != nil || len(response.Diagnostics) != 1 || response.Diagnostics[0].Code != "function-unavailable" {
		t.Fatalf("missing function diagnostics: %+v, %v", response, err)
	}
	request.Resources = resourcesOf(inv)
	for range 2 {
		response, err = service.Expand(t.Context(), request)
		if err != nil || len(response.Diagnostics) != 0 || len(response.Expansions) != 1 {
			t.Fatalf("resolved expansion: %+v, %v", response, err)
		}
		if len(response.Expansions[0].Resources) != 1 || len(response.Evidence) == 0 {
			t.Fatal("missing desired output or status evidence")
		}
		request.Resources = append(resourcesOf(inv), response.Expansions[0].Resources...)
	}
	if calls != 1 {
		t.Fatalf("unchanged pipeline reran: %d calls", calls)
	}
}

func TestFatalPublishesEvidenceNotDesired(t *testing.T) {
	t.Parallel()
	service := New(logr.Discard())
	sessionID := openTestSession(t, service)
	service.execute = func(
		ctx context.Context, sess *session, in cprender.CompositionInputs, fns []pkgv1.Function,
	) (*rendered, error) {
		return outputResponse(t), errors.New("pipeline step returned a fatal result")
	}
	response, err := service.Expand(t.Context(), &plugin.ExpandRequest{
		Session: sessionID, Resources: resourcesOf(fixtureInventory(t)),
	})
	if err != nil || len(response.Diagnostics) != 1 || len(response.Expansions[0].Resources) != 0 {
		t.Fatalf("fatal expansion: %+v, %v", response, err)
	}
	if len(response.Evidence) == 0 {
		t.Fatal("partial evidence lost")
	}
}

func TestUnnamedCompositeOutputIsTerminalDesiredInventory(t *testing.T) {
	t.Parallel()
	service := New(logr.Discard())
	id := openTestSession(t, service)
	inv := fixtureInventory(t)
	calls := 0
	service.execute = func(_ context.Context, _ *session, in cprender.CompositionInputs, _ []pkgv1.Function) (*rendered, error) {
		calls++
		if in.CompositeResource.GetName() != "demo" {
			t.Fatalf("unnamed desired XR became an active composite: %s", in.CompositeResource.GetName())
		}
		for _, required := range in.RequiredResources {
			if required.GetName() == "" {
				t.Fatal("terminal logical output was provided as a named required resource")
			}
		}
		result := outputResponse(t, map[string]any{
			"apiVersion": "example.org/v1", "kind": "App",
			"metadata": map[string]any{"name": "engine-synthetic", "generateName": "child-", "namespace": "apps",
				"annotations": map[string]any{"crossplane.io/composition-resource-name": "child"}},
			"spec": map[string]any{},
		})
		result.declaredNames["child"] = ""
		return result, nil
	}
	request := &plugin.ExpandRequest{Session: id, Resources: resourcesOf(inv)}
	first, err := service.Expand(t.Context(), request)
	if err != nil || len(first.Diagnostics) != 0 || len(first.Expansions) != 1 || len(first.Expansions[0].Resources) != 1 {
		t.Fatalf("initial composite render: %+v, %v", first, err)
	}
	child := first.Expansions[0].Resources[0]
	if !child.Logical {
		t.Fatal("unnamed XR lost logical identity")
	}
	object, err := plugin.Object(child)
	if err != nil {
		t.Fatal(err)
	}
	metadata := object["metadata"].(map[string]any)
	if metadata["name"] != nil && metadata["name"] != "" {
		t.Fatalf("unnamed XR retained fabricated engine name: %+v", metadata)
	}
	request.Resources = append(resourcesOf(inv), child)
	second, err := service.Expand(t.Context(), request)
	if err != nil || len(second.Diagnostics) != 0 || len(second.Expansions) != 1 || second.Expansions[0].Trigger != inv[1].resource.ID || !slices.Equal(second.Expansions[0].Resources, first.Expansions[0].Resources) || calls != 1 {
		t.Fatalf("terminal XR activated or changed the parent evaluation: %+v, calls=%d, err=%v", second, calls, err)
	}
	// Removing the named parent leaves only an unnamed desired XR. It must not
	// execute a pipeline or retain the parent's producer.
	request.Resources = append(resourcesOf([]inventoryResource{inv[0], inv[2], inv[3]}), child)
	removed, err := service.Expand(t.Context(), request)
	if err != nil || len(removed.Diagnostics) != 0 || len(removed.Expansions) != 0 || calls != 1 {
		t.Fatalf("terminal XR kept an active producer after parent removal: %+v, calls=%d, err=%v", removed, calls, err)
	}
}

func TestNamedAndUnnamedOutputs(t *testing.T) {
	t.Parallel()
	inv := fixtureInventory(t)
	outputs := []composed.Unstructured{}
	for _, tt := range []struct {
		key         string
		name        string
		generate    string
		declared    string
		wantLogical bool
	}{
		{key: "unnamed", name: "demo-synthetic", generate: "demo-", wantLogical: true},
		{key: "flux", name: "real-flux-name", declared: "real-flux-name"},
	} {
		o := composed.New()
		o.SetAPIVersion("kustomize.toolkit.fluxcd.io/v1")
		o.SetKind("Kustomization")
		o.SetNamespace("apps")
		o.SetName(tt.name)
		o.SetGenerateName(tt.generate)
		o.SetAnnotations(map[string]string{"crossplane.io/composition-resource-name": tt.key})
		outputs = append(outputs, *o)
	}
	resources, err := desiredResources(inv[1], outputs, map[string]string{"unnamed": "", "flux": "real-flux-name"})
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range resources {
		o, err := plugin.Object(resource)
		if err != nil {
			t.Fatal(err)
		}
		object := &unstructured.Unstructured{Object: o}
		if resource.Logical {
			if object.GetName() != "" || object.GetGenerateName() != "demo-" {
				t.Fatal("synthetic name retained or generateName lost")
			}
		} else if object.GetName() != "real-flux-name" {
			t.Fatal("explicit Flux name changed")
		}
	}
	outputs[0].SetName("another-synthetic-name")
	other, err := desiredResources(inv[1], outputs, map[string]string{"unnamed": "", "flux": "real-flux-name"})
	if err != nil || !slices.EqualFunc(resources, other, func(a, b plugin.Resource) bool { return a.ID == b.ID && a.YAML == b.YAML }) {
		t.Fatalf("logical output changed with engine-generated name: %v", err)
	}
	_, err = desiredResources(inv[1], outputs, map[string]string{})
	assertCode(t, err, "output-name-evidence-missing")
	// A user may explicitly choose a name resembling the controller's name
	// format. The actual response, never a prefix/suffix heuristic, decides.
	outputs[0].SetName("demo-123456abcdef")
	explicit, err := desiredResources(inv[1], outputs[:1], map[string]string{"unnamed": "demo-123456abcdef"})
	if err != nil || explicit[0].Logical {
		t.Fatalf("explicit generated-looking name misclassified: %v", err)
	}
}

func TestSessionCloseCleansRuntimesAndIsolatesFreshSessions(t *testing.T) {
	t.Parallel()
	service := New(logr.Discard())
	id := openTestSession(t, service)
	second := openTestSession(t, service)
	if id == second || service.sessions[id] == service.sessions[second] {
		t.Fatal("Fresh sessions shared state")
	}
	var stops atomic.Int32
	service.sessions[id].runtimes["owned"] = cprender.RuntimeContext{Stop: func(ctx context.Context) error {
		if ctx.Err() != nil {
			t.Fatal("cleanup used canceled caller context")
		}
		stops.Add(1)
		return nil
	}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := service.CloseRender(ctx, &plugin.CloseRequest{Session: id}); err != nil {
		t.Fatal(err)
	}
	if stops.Load() != 1 {
		t.Fatal("runtime not cleaned exactly once")
	}
	if _, err := service.Expand(t.Context(), &plugin.ExpandRequest{Session: id}); err == nil {
		t.Fatal("closed session accepted")
	}
}

func TestCloseCancelsActiveRender(t *testing.T) {
	t.Parallel()
	service := New(logr.Discard())
	id := openTestSession(t, service)
	started := make(chan struct{})
	service.execute = func(
		ctx context.Context, sess *session, in cprender.CompositionInputs, fns []pkgv1.Function,
	) (*rendered, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	done := make(chan error, 1)
	request := &plugin.ExpandRequest{Session: id, Resources: resourcesOf(fixtureInventory(t))}
	go func() {
		_, err := service.Expand(t.Context(), request)
		done <- err
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("render did not start")
	}
	if _, err := service.CloseRender(ctx, &plugin.CloseRequest{Session: id}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("active render cancellation = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("render survived session close")
	}
}

func TestMissingDefinitionAndUnsupportedMode(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		code string
	}{
		{name: "missing XRD", code: "xrd-unavailable"},
		{name: "nonpipeline", code: "unsupported-composition"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			service := New(logr.Discard())
			id := openTestSession(t, service)
			inv := fixtureInventory(t)
			if tt.name == "missing XRD" {
				inv = inv[1:]
			} else {
				setField(t, inv[2].object, "Resources", "spec", "mode")
				inv[2].resource = marshalResource(t, inv[2])
			}
			service.execute = func(
				ctx context.Context, sess *session, in cprender.CompositionInputs, fns []pkgv1.Function,
			) (*rendered, error) {
				t.Fatal("unsupported inputs executed an engine")
				return nil, nil
			}
			response, err := service.Expand(t.Context(), &plugin.ExpandRequest{Session: id, Resources: resourcesOf(inv)})
			if err != nil || len(response.Diagnostics) != 1 || response.Diagnostics[0].Code != tt.code {
				t.Fatalf("unsupported inputs: %+v, %v", response, err)
			}
		})
	}
}

// marshalResource updates a fixture's wire YAML after selection mutations.
func marshalResource(t *testing.T, item inventoryResource) plugin.Resource {
	t.Helper()
	data, err := yaml.Marshal(item.object.Object)
	if err != nil {
		t.Fatal(err)
	}
	resource, err := plugin.ParseResource(data, item.resource.Provenance)
	if err != nil {
		t.Fatal(err)
	}
	return resource
}
