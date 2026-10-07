package crossplanerender

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	cprender "github.com/crossplane/cli/v2/cmd/crossplane/render"
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/go-logr/logr"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
)

func addEngineSelector(t *testing.T, result *rendered, selector *fnv1.ResourceSelector) {
	t.Helper()
	data, err := protojson.Marshal(selector)
	if err != nil {
		t.Fatal(err)
	}
	value := &structpb.Struct{}
	if err := value.UnmarshalJSON(data); err != nil {
		t.Fatal(err)
	}
	result.response.GetComposite().RequiredResources = append(result.response.GetComposite().RequiredResources, value)
}

func changingOutput(t *testing.T, call int) *rendered {
	t.Helper()
	result := outputResponse(t, map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{
			"name": "random-output", "namespace": "apps",
			"annotations": map[string]any{"crossplane.io/composition-resource-name": "random"},
		}, "data": map[string]any{"randomValue": fmt.Sprintf("execution-%d", call)},
	})
	result.declaredNames["random"] = "random-output"
	return result
}

func mustExpand(t *testing.T, service *Service, request *plugin.ExpandRequest) *plugin.ExpandResponse {
	t.Helper()
	response, err := service.Expand(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestEvaluationCachesRandomOutputsWithinSession(t *testing.T) {
	t.Parallel()
	service := New(logr.Discard())
	id := openTestSession(t, service)
	inv := fixtureInventory(t)
	calls := 0
	service.execute = func(
		ctx context.Context, sess *session, in cprender.CompositionInputs, fns []pkgv1.Function,
	) (*rendered, error) {
		calls++
		if len(in.ObservedResources) != 0 {
			t.Fatal("desired outputs became observed state")
		}
		return changingOutput(t, calls), nil
	}
	request := &plugin.ExpandRequest{Session: id, Resources: resourcesOf(inv)}
	first := mustExpand(t, service, request)
	if len(first.Diagnostics) != 0 {
		t.Fatal(first.Diagnostics)
	}
	for range 3 {
		// Simulate growing Flux discovery, including this XR's prior output.
		request.Resources = append(resourcesOf(inv), first.Expansions[0].Resources...)
		unrelated := testResource(t, `apiVersion: v1
kind: ConfigMap
metadata: {name: unrelated, namespace: apps}
data: {value: unrelated}
`)
		request.Resources = append(request.Resources, unrelated.resource)
		response := mustExpand(t, service, request)
		before, _ := json.Marshal(first)
		after, _ := json.Marshal(response)
		if string(before) != string(after) {
			t.Fatal("cached output/evidence changed or evidence accumulated")
		}
	}
	if calls != 1 {
		t.Fatalf("unrelated inventory invalidated random evaluation: %d calls", calls)
	}
	request.Session = openTestSession(t, service)
	second := mustExpand(t, service, request)
	if calls != 2 || first.Expansions[0].Resources[0].YAML == second.Expansions[0].Resources[0].YAML {
		t.Fatal("independent session reused prior evaluation")
	}
}

func TestRequiredResourceWitnessesInvalidateEvaluation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		selector *fnv1.ResourceSelector
	}{
		{name: "named namespace", selector: &fnv1.ResourceSelector{
			ApiVersion: "v1", Kind: "ConfigMap", Namespace: proto.String("apps"),
			Match: &fnv1.ResourceSelector_MatchName{MatchName: "dependency"},
		}},
		{name: "labels namespace", selector: &fnv1.ResourceSelector{
			ApiVersion: "v1", Kind: "ConfigMap", Namespace: proto.String("apps"),
			Match: &fnv1.ResourceSelector_MatchLabels{MatchLabels: &fnv1.MatchLabels{Labels: map[string]string{"role": "dependency"}}},
		}},
		{name: "labels all namespaces", selector: &fnv1.ResourceSelector{
			ApiVersion: "v1", Kind: "ConfigMap",
			Match: &fnv1.ResourceSelector_MatchLabels{MatchLabels: &fnv1.MatchLabels{Labels: map[string]string{"role": "dependency"}}},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			service := New(logr.Discard())
			id := openTestSession(t, service)
			inv := fixtureInventory(t)
			calls := 0
			service.execute = func(
				ctx context.Context, sess *session, in cprender.CompositionInputs, fns []pkgv1.Function,
			) (*rendered, error) {
				calls++
				if len(in.ObservedResources) != 0 {
					t.Fatal("cache miss fed desired outputs into observed state")
				}
				result := changingOutput(t, calls)
				addEngineSelector(t, result, tt.selector)
				return result, nil
			}
			request := &plugin.ExpandRequest{Session: id, Resources: resourcesOf(inv)}
			mustExpand(t, service, request) // Zero-match witness.
			wrongNamespace := testResource(t, `apiVersion: v1
kind: ConfigMap
metadata: {name: dependency, namespace: other, labels: {role: dependency}}
data: {value: initial}
`)
			request.Resources = append(resourcesOf(inv), wrongNamespace.resource)
			mustExpand(t, service, request)
			want := 1
			if tt.name == "labels all namespaces" {
				want++
			}
			if calls != want {
				t.Fatalf("namespace witness: %d calls, want %d", calls, want)
			}
			dependency := wrongNamespace
			dependency.object = dependency.object.DeepCopy()
			dependency.object.SetNamespace("apps")
			request.Resources = append(resourcesOf(inv), marshalResource(t, dependency))
			mustExpand(t, service, request)
			want++
			if calls != want {
				t.Fatal("zero match becoming available did not invalidate")
			}
			mustExpand(t, service, request)
			if calls != want {
				t.Fatal("unchanged requested resource invalidated evaluation")
			}
			setField(t, dependency.object, "changed", "data", "value")
			request.Resources = append(resourcesOf(inv), marshalResource(t, dependency))
			mustExpand(t, service, request)
			if calls != want+1 {
				t.Fatal("requested resource content change did not invalidate")
			}
			request.Resources = resourcesOf(inv)
			mustExpand(t, service, request)
			if calls != want+2 {
				t.Fatal("requested resource deletion did not invalidate")
			}
		})
	}
}

func TestFatalPendingDependencyIsCachedThenResolved(t *testing.T) {
	t.Parallel()
	service := New(logr.Discard())
	id := openTestSession(t, service)
	inv := fixtureInventory(t)
	selector := &fnv1.ResourceSelector{
		ApiVersion: "v1", Kind: "ConfigMap", Namespace: proto.String("apps"),
		Match: &fnv1.ResourceSelector_MatchName{MatchName: "pending"},
	}
	calls := 0
	service.execute = func(
		ctx context.Context, sess *session, in cprender.CompositionInputs, fns []pkgv1.Function,
	) (*rendered, error) {
		calls++
		result := changingOutput(t, calls)
		// A FATAL response's requirements may not reach the engine fetcher.
		// The transparent function proxy supplies these selectors instead.
		result.resourceSelectors = []*fnv1.ResourceSelector{selector}
		for _, resource := range in.RequiredResources {
			if resourceMatches(selector, &resource) {
				return result, nil
			}
		}
		return result, errors.New("pipeline fatal: pending input absent")
	}
	request := &plugin.ExpandRequest{Session: id, Resources: resourcesOf(inv)}
	first := mustExpand(t, service, request)
	second := mustExpand(t, service, request)
	if calls != 1 || len(second.Expansions[0].Resources) != 0 || second.Diagnostics[0].Code != "pipeline-fatal" {
		t.Fatal("unchanged fatal was rerun or published desired state")
	}
	a, _ := json.Marshal(first.Evidence)
	b, _ := json.Marshal(second.Evidence)
	if string(a) != string(b) {
		t.Fatal("cached fatal evidence changed")
	}
	pending := testResource(t, `apiVersion: v1
kind: ConfigMap
metadata: {name: pending, namespace: apps}
data: {value: available}
`)
	request.Resources = append(resourcesOf(inv), pending.resource)
	response := mustExpand(t, service, request)
	if calls != 2 || len(response.Diagnostics) != 0 || len(response.Expansions[0].Resources) != 1 {
		t.Fatal("cached fatal did not resolve when its input appeared")
	}
}

func TestRequestedSchemaWitnessDoesNotHashOtherDefinitions(t *testing.T) {
	t.Parallel()
	service := New(logr.Discard())
	id := openTestSession(t, service)
	inv := fixtureInventory(t)
	selector := &fnv1.SchemaSelector{ApiVersion: "example.org/v1", Kind: "Other"}
	calls := 0
	service.execute = func(
		ctx context.Context, sess *session, in cprender.CompositionInputs, fns []pkgv1.Function,
	) (*rendered, error) {
		calls++
		result := changingOutput(t, calls)
		result.schemaSelectors = []*fnv1.SchemaSelector{selector}
		return result, nil
	}
	request := &plugin.ExpandRequest{Session: id, Resources: resourcesOf(inv)}
	mustExpand(t, service, request) // Missing schema witness.
	other := testResource(t, testXRD)
	other.object.SetName("others.example.org")
	setField(t, other.object, "Other", "spec", "names", "kind")
	setField(t, other.object, "others", "spec", "names", "plural")
	request.Resources = append(resourcesOf(inv), marshalResource(t, other))
	mustExpand(t, service, request)
	if calls != 2 {
		t.Fatal("newly available requested schema did not invalidate")
	}
	unrelated := testResource(t, testXRD)
	unrelated.object.SetName("unrelateds.example.org")
	setField(t, unrelated.object, "Unrelated", "spec", "names", "kind")
	setField(t, unrelated.object, "unrelateds", "spec", "names", "plural")
	request.Resources = append(request.Resources, marshalResource(t, unrelated))
	mustExpand(t, service, request)
	if calls != 2 {
		t.Fatal("unrelated schema in same OpenAPI document invalidated evaluation")
	}
	versions, _, _ := unstructured.NestedSlice(other.object.Object, "spec", "versions")
	version, _ := versions[0].(map[string]any)
	schema, _ := version["schema"].(map[string]any)
	openAPI, _ := schema["openAPIV3Schema"].(map[string]any)
	openAPI["description"] = "changed requested schema"
	setField(t, other.object, versions, "spec", "versions")
	request.Resources = append(resourcesOf(inv), marshalResource(t, other), marshalResource(t, unrelated))
	mustExpand(t, service, request)
	if calls != 3 {
		t.Fatal("requested schema change did not invalidate")
	}
}

func TestDirectEvaluationDependenciesInvalidate(t *testing.T) {
	t.Parallel()
	for _, dependency := range []string{"XR", "XRD", "Composition", "Function", "credentials"} {
		t.Run(dependency, func(t *testing.T) {
			t.Parallel()
			service := New(logr.Discard())
			id := openTestSession(t, service)
			inv := fixtureInventory(t)
			if dependency == "credentials" {
				steps, _, _ := unstructured.NestedSlice(inv[2].object.Object, "spec", "pipeline")
				step, _ := steps[0].(map[string]any)
				step["credentials"] = []any{map[string]any{
					"name": "auth", "source": "Secret",
					"secretRef": map[string]any{"name": "credentials", "namespace": "apps"},
				}}
				setField(t, inv[2].object, steps, "spec", "pipeline")
				inv[2].resource = marshalResource(t, inv[2])
				inv = append(inv, testResource(t, `apiVersion: v1
kind: Secret
metadata: {name: credentials, namespace: apps}
stringData: {token: first}
`))
			}
			calls := 0
			service.execute = func(
				ctx context.Context, sess *session, in cprender.CompositionInputs, fns []pkgv1.Function,
			) (*rendered, error) {
				calls++
				return changingOutput(t, calls), nil
			}
			request := &plugin.ExpandRequest{Session: id, Resources: resourcesOf(inv)}
			mustExpand(t, service, request)
			mustExpand(t, service, request)
			switch dependency {
			case "XR":
				setField(t, inv[1].object, int64(3), "spec", "replicas")
				inv[1].resource = marshalResource(t, inv[1])
			case "XRD":
				setField(t, inv[0].object, "app", "spec", "names", "singular")
				inv[0].resource = marshalResource(t, inv[0])
			case "Composition":
				setField(t, inv[2].object, "changed", "metadata", "annotations", "test-input")
				inv[2].resource = marshalResource(t, inv[2])
			case "Function":
				setField(t, inv[3].object, "example.invalid/function:v2", "spec", "package")
				inv[3].resource = marshalResource(t, inv[3])
			case "credentials":
				setField(t, inv[4].object, "changed", "stringData", "token")
				inv[4].resource = marshalResource(t, inv[4])
			}
			request.Resources = resourcesOf(inv)
			mustExpand(t, service, request)
			mustExpand(t, service, request)
			if calls != 2 {
				t.Fatalf("%s dependency change caused %d evaluations, want 2", dependency, calls)
			}
		})
	}
}
