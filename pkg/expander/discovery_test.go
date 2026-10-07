package expander

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/resource"
)

type generatedResourceExpander struct {
	calls    int
	freshIDs bool
}

func (e *generatedResourceExpander) Expand(context.Context, *render.Render) (*ExpandResult, error) {
	e.calls++
	name := "same"
	if e.freshIDs {
		name = fmt.Sprintf("generated-%d", e.calls)
	}
	output, err := resmap.NewFactory(resource.NewFactory(nil)).NewResMapFromBytes([]byte(
		fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata: {name: %s, annotations: {pass: '%d'}}\n", name, e.calls)))
	return &ExpandResult{Resources: output}, err
}

func TestDiscoveryProgressUsesNewIdentities(t *testing.T) {
	for _, freshIDs := range []bool{false, true} {
		e := &generatedResourceExpander{freshIDs: freshIDs}
		registry := NewRegistry(logr.Discard())
		registry.Register(e)
		r := DiscoveryRunner{Expanders: registry, MaxIterations: 3, RenderPath: func(DiscoveredPath) error { return nil }}
		_, err := r.Run(t.Context(), render.NewDefaultRender(logr.Discard()), []DiscoveredPath{{Path: "."}})
		if freshIDs {
			if err == nil || e.calls > 4 {
				t.Fatalf("Run(unbounded new identities) = %v, calls=%d, want bounded error", err, e.calls)
			}
		} else if err != nil || e.calls != 2 {
			t.Fatalf("Run(metadata churn) = %v, calls=%d, want one retry then stop", err, e.calls)
		}
	}
}
