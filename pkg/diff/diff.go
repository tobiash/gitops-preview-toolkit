package diff

import (
	"fmt"
	"io"

	"github.com/hexops/gotextdiff"
	"github.com/hexops/gotextdiff/myers"
	"github.com/hexops/gotextdiff/span"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	k8qdiff "github.com/tobiash/k8q/pkg/diff"
	"sigs.k8s.io/kustomize/kyaml/resid"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

// renderToRNodes converts a render.Render to a slice of *yaml.RNode for
// consumption by the k8q diff engine. Copies keep render ownership isolated
// from downstream analysis.
func renderToRNodes(r *render.Render) []*yaml.RNode {
	resources := r.Resources()
	nodes := make([]*yaml.RNode, len(resources))
	for i, res := range resources {
		nodes[i] = res.Copy()
	}
	return nodes
}

// objectRefToResID converts a k8q ObjectRef to a kustomize resid.ResId.
func objectRefToResID(ref k8qdiff.ObjectRef) resid.ResId {
	return resid.NewResIdWithNamespace(
		resid.NewGvk(gvkGroup(ref.APIVersion), gvkVersion(ref.APIVersion), ref.Kind),
		ref.Name,
		ref.Namespace,
	)
}

func gvkGroup(apiVersion string) string {
	if i := len(apiVersion) - 1; i >= 0 {
		for j := i; j >= 0; j-- {
			if apiVersion[j] == '/' {
				return apiVersion[:j]
			}
		}
	}
	return ""
}

func gvkVersion(apiVersion string) string {
	for i := len(apiVersion) - 1; i >= 0; i-- {
		if apiVersion[i] == '/' {
			return apiVersion[i+1:]
		}
	}
	return apiVersion
}

func computeDiff(name, before, after string) gotextdiff.Unified {
	edits := myers.ComputeEdits(span.URIFromPath(name), before, after)
	return gotextdiff.ToUnified(name, name, before, edits)
}

// UnifiedDiff computes a unified diff between before and after strings.
func UnifiedDiff(name, before, after string) string {
	return fmt.Sprintf("%v", computeDiff(name, before, after))
}

func formatUnified(w io.Writer, u gotextdiff.Unified) {
	_, _ = fmt.Fprintf(w, "%v", u)
}

// Diff computes a unified diff between two Renders and writes the result to w.
func Diff(a, b *render.Render, w io.Writer) error {
	_, err := DiffWithResult(a, b, w)
	return err
}
