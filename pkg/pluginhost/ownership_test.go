package pluginhost_test

import (
	"context"
	"strings"
	"testing"

	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
)

func hasResource(inventory []plugin.Resource, id string) bool {
	for _, resource := range inventory {
		if resource.ID == id {
			return true
		}
	}
	return false
}

func TestHostGeneratedOutputHandsOffToDirectRoot(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"named", "cross-version named", "logical"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			xr, err := plugin.ParseResource([]byte(
				"apiVersion: example.org/v1\nkind: ExampleXR\nmetadata:\n  name: example\n  namespace: default\n"), plugin.Provenance{})
			if err != nil {
				t.Fatal(err)
			}
			generated := resource(t, "child", "generated")
			direct := resource(t, "child", "direct")
			switch kind {
			case "cross-version named":
				direct, err = plugin.ParseResource([]byte(strings.Replace(direct.YAML, "apiVersion: v1", "apiVersion: v2", 1)),
					plugin.Provenance{})
				if err != nil {
					t.Fatal(err)
				}
			case "logical":
				generated = plugin.Resource{ID: "logical:child", Logical: true,
					YAML: "apiVersion: v1\nkind: ConfigMap\ndata:\n  value: generated\n"}
				direct = plugin.Resource{ID: generated.ID, Logical: true,
					YAML: "apiVersion: v1\nkind: ConfigMap\ndata:\n  value: direct\n"}
			}
			flux := &fakeEngine{name: "flux", expand: func(_ context.Context, request *plugin.ExpandRequest, _ int) (*plugin.ExpandResponse, error) {
				// Model a source update that removes the XR and supplies its former
				// child directly once the generated child has first been observed.
				updated := hasResource(request.Resources, generated.ID) || hasResource(request.Resources, direct.ID)
				if updated {
					return expansion("source", "root", direct), nil
				}
				return expansion("source", "root", xr), nil
			}}
			crossplane := &fakeEngine{name: "crossplane", expand: func(_ context.Context, request *plugin.ExpandRequest, _ int) (*plugin.ExpandResponse, error) {
				if !hasResource(request.Resources, xr.ID) {
					return &plugin.ExpandResponse{}, nil
				}
				return expansion("composed", xr.ID, generated), nil
			}}
			result, err := host(t, flux, crossplane).Render(t.Context(), plugin.OpenRequest{})
			complete(t, result, err)
			if len(result.Resources) != 1 || result.Resources[0].ID != direct.ID ||
				value(t, result.Resources, direct.ID) != "direct" {
				t.Fatalf("ownership handoff lost direct root: %+v", result.Resources)
			}
			if len(crossplane.inputs) != 4 || !hasResource(crossplane.inputs[2], xr.ID) {
				t.Fatal("test did not exercise a stale generated output during source replacement")
			}
		})
	}
}

func TestHostOrphanDuplicateIsPrunedWithoutOwnershipConflict(t *testing.T) {
	t.Parallel()
	root := resource(t, "child", "direct")
	orphan := resource(t, "child", "stale")
	response := expansion("orphan", "nonexistent", orphan)
	// Pruning ownership must not erase an engine's pending error diagnostics.
	response.Diagnostics = []plugin.Diagnostic{{Code: "pending", Severity: "error", Message: "unresolved input"}}
	result, err := host(t, fixed("flux", expansion("source", "root", root)),
		fixed("crossplane", response)).Render(t.Context(), plugin.OpenRequest{})
	if err != nil {
		t.Fatalf("inactive duplicate caused ownership conflict: %v", err)
	}
	incomplete(t, result)
	codes := map[string]bool{}
	for _, diagnostic := range result.Diagnostics {
		codes[diagnostic.Code] = true
	}
	if !codes["missing-trigger"] || !codes["pending"] {
		t.Fatalf("pruning hid diagnostics: %+v", result.Diagnostics)
	}
}

func TestHostDuplicateInInactiveBatchRemainsMalformed(t *testing.T) {
	t.Parallel()
	r := resource(t, "child", "stale")
	result, err := host(t, fixed("crossplane", expansion("orphan", "nonexistent", r, r))).Render(
		t.Context(), plugin.OpenRequest{})
	if err == nil || !strings.Contains(err.Error(), "duplicate resource identity") {
		t.Fatalf("malformed inactive batch accepted: %v", err)
	}
	incomplete(t, result)
}

func TestHostSharedIdentityActivatesEveryProducerBeforeCollisionCheck(t *testing.T) {
	t.Parallel()
	root := resource(t, "root", "present")
	one := resource(t, "shared", "one")
	two := resource(t, "shared", "two")
	// A shared identity must activate consumers even if an inactive producer
	// claims it first. Both child producers become reachable from the root.
	flux := fixed("flux", expansion("source", "root", root))
	other := fixed("crossplane", &plugin.ExpandResponse{Expansions: []plugin.Expansion{
		{ID: "inactive", Trigger: "missing", Resources: []plugin.Resource{one}},
		{ID: "two", Trigger: one.ID, Resources: []plugin.Resource{two}},
		{ID: "one", Trigger: root.ID, Resources: []plugin.Resource{one}},
	}})
	result, err := host(t, flux, other).Render(t.Context(), plugin.OpenRequest{})
	if err == nil || !strings.Contains(err.Error(), "duplicate resource identity") {
		t.Fatalf("reachable producers escaped ownership validation: %v", err)
	}
	incomplete(t, result)
}
