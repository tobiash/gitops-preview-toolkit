package preview

import (
	"context"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/kustomize/kyaml/resid"
)

// ObjectRef mirrors the Kubernetes ObjectReference shape.
type ObjectRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace,omitempty"`
}

// KustomizationItem is the JSON representation of a Flux Kustomization.
type KustomizationItem struct {
	ObjectRef ObjectRef  `json:"objectRef"`
	Path      string     `json:"path,omitempty"`
	SourceRef *ObjectRef `json:"sourceRef,omitempty"`
}

// HelmReleaseItem is the JSON representation of a HelmRelease.
type HelmReleaseItem struct {
	ObjectRef ObjectRef  `json:"objectRef"`
	Chart     string     `json:"chart,omitempty"`
	Version   string     `json:"version,omitempty"`
	SourceRef *ObjectRef `json:"sourceRef,omitempty"`
}

// GetResourcesOutput is the JSON envelope for listing Flux resources.
type GetResourcesOutput struct {
	Items any `json:"items"`
}

// KustomizationInfo holds extracted fields from a Flux Kustomization CR.
type KustomizationInfo struct {
	Name       string
	Namespace  string
	Path       string
	SourceKind string
	SourceName string
}

// HelmReleaseInfo holds extracted fields from a HelmRelease CR.
type HelmReleaseInfo struct {
	Name       string
	Namespace  string
	Chart      string
	Version    string
	SourceKind string
	SourceName string
}

// ListKustomizations discovers and lists Flux Kustomizations from the repo at path.
func (p *Preview) ListKustomizations(ctx context.Context, path string) ([]KustomizationInfo, error) {
	results, err := p.loadRepo(ctx, path)
	if err != nil {
		return nil, err
	}
	var all []KustomizationInfo
	for _, r := range results {
		if len(r.errors) > 0 {
			return nil, &ExpansionError{Errors: r.errors, Warnings: r.warnings}
		}
		all = append(all, extractKustomizations(r.render)...)
	}
	return all, nil
}

// ListHelmReleases discovers and lists HelmReleases from the repo at path.
func (p *Preview) ListHelmReleases(ctx context.Context, path string) ([]HelmReleaseInfo, error) {
	results, err := p.loadRepo(ctx, path)
	if err != nil {
		return nil, err
	}
	var all []HelmReleaseInfo
	for _, r := range results {
		if len(r.errors) > 0 {
			return nil, &ExpansionError{Errors: r.errors, Warnings: r.warnings}
		}
		all = append(all, extractHelmReleases(r.render)...)
	}
	return all, nil
}

func extractKustomizations(r *render.Render) []KustomizationInfo {
	target := resid.NewGvk("kustomize.toolkit.fluxcd.io", "v1", "Kustomization")
	var result []KustomizationInfo
	for _, res := range r.Resources() {
		if !matchListGVK(res.GetGvk(), target) {
			continue
		}
		m, err := res.Map()
		if err != nil {
			continue
		}
		spec, _ := m["spec"].(map[string]any)
		path, _, _ := unstructured.NestedString(spec, "path")
		srcRef, _ := spec["sourceRef"].(map[string]any)
		srcKind, _, _ := unstructured.NestedString(srcRef, "kind")
		srcName, _, _ := unstructured.NestedString(srcRef, "name")
		result = append(result, KustomizationInfo{
			Name:       res.GetName(),
			Namespace:  res.GetNamespace(),
			Path:       path,
			SourceKind: srcKind,
			SourceName: srcName,
		})
	}
	return result
}

func extractHelmReleases(r *render.Render) []HelmReleaseInfo {
	target := resid.NewGvk("helm.toolkit.fluxcd.io", "v2", "HelmRelease")
	var result []HelmReleaseInfo
	for _, res := range r.Resources() {
		if !matchListGVK(res.GetGvk(), target) {
			continue
		}
		m, err := res.Map()
		if err != nil {
			continue
		}
		spec, _ := m["spec"].(map[string]any)
		chart := listNestedStr(spec, "chart", "spec", "chart")
		version := listNestedStr(spec, "chart", "spec", "version")
		srcRefRaw, _ := listNestedMap(spec, "chart", "spec", "sourceRef")
		srcRef, _ := srcRefRaw.(map[string]any)
		srcKind, _, _ := unstructured.NestedString(srcRef, "kind")
		srcName, _, _ := unstructured.NestedString(srcRef, "name")
		result = append(result, HelmReleaseInfo{
			Name:       res.GetName(),
			Namespace:  res.GetNamespace(),
			Chart:      chart,
			Version:    version,
			SourceKind: srcKind,
			SourceName: srcName,
		})
	}
	return result
}

func matchListGVK(a, b resid.Gvk) bool {
	return render.MatchGVK(a, b)
}

// KustomizationsToJSON converts KustomizationInfo slices to a JSON-serializable envelope.
func KustomizationsToJSON(ks []KustomizationInfo) *GetResourcesOutput {
	items := make([]KustomizationItem, 0, len(ks))
	for _, k := range ks {
		item := KustomizationItem{
			ObjectRef: ObjectRef{
				APIVersion: "kustomize.toolkit.fluxcd.io/v1",
				Kind:       "Kustomization",
				Name:       k.Name,
				Namespace:  k.Namespace,
			},
			Path: k.Path,
		}
		if k.SourceKind != "" && k.SourceName != "" {
			item.SourceRef = &ObjectRef{
				Kind:      k.SourceKind,
				Name:      k.SourceName,
				Namespace: k.Namespace,
			}
		}
		items = append(items, item)
	}
	return &GetResourcesOutput{Items: items}
}

// HelmReleasesToJSON converts HelmReleaseInfo slices to a JSON-serializable envelope.
func HelmReleasesToJSON(hrs []HelmReleaseInfo) *GetResourcesOutput {
	items := make([]HelmReleaseItem, 0, len(hrs))
	for _, hr := range hrs {
		item := HelmReleaseItem{
			ObjectRef: ObjectRef{
				APIVersion: "helm.toolkit.fluxcd.io/v2",
				Kind:       "HelmRelease",
				Name:       hr.Name,
				Namespace:  hr.Namespace,
			},
			Chart:   hr.Chart,
			Version: hr.Version,
		}
		if hr.SourceKind != "" && hr.SourceName != "" {
			item.SourceRef = &ObjectRef{
				Kind:      hr.SourceKind,
				Name:      hr.SourceName,
				Namespace: hr.Namespace,
			}
		}
		items = append(items, item)
	}
	return &GetResourcesOutput{Items: items}
}

// PrintKustomizations writes a table of Flux Kustomizations to out.
func PrintKustomizations(ks []KustomizationInfo, out io.Writer) {
	if len(ks) == 0 {
		_, _ = fmt.Fprintln(out, "No Kustomizations found.")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tNAMESPACE\tPATH\tSOURCE")
	for _, k := range ks {
		src := k.SourceKind + "/" + k.SourceName
		if src == "/" {
			src = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", k.Name, k.Namespace, k.Path, src)
	}
	_ = tw.Flush()
}

// PrintHelmReleases writes a table of HelmReleases to out.
func PrintHelmReleases(hrs []HelmReleaseInfo, out io.Writer) {
	if len(hrs) == 0 {
		_, _ = fmt.Fprintln(out, "No HelmReleases found.")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tNAMESPACE\tCHART\tVERSION\tSOURCE")
	for _, hr := range hrs {
		src := hr.SourceKind + "/" + hr.SourceName
		if src == "/" {
			src = "-"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", hr.Name, hr.Namespace, hr.Chart, hr.Version, src)
	}
	_ = tw.Flush()
}

func listNestedStr(m map[string]any, keys ...string) string {
	val, _ := listNestedMap(m, keys...)
	if s, ok := val.(string); ok {
		return s
	}
	return ""
}

func listNestedMap(m map[string]any, keys ...string) (any, bool) {
	for i, key := range keys {
		val, ok := m[key]
		if !ok {
			return nil, false
		}
		if i == len(keys)-1 {
			return val, true
		}
		next, ok := val.(map[string]any)
		if !ok {
			return nil, false
		}
		m = next
	}
	return nil, false
}
