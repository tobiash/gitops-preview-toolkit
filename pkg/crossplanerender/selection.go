package crossplanerender

import (
	"fmt"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
)

type inventoryResource struct {
	resource plugin.Resource
	object   *unstructured.Unstructured
}

type definition struct {
	object   *unstructured.Unstructured
	group    string
	kind     string
	scope    string
	versions []string
}

type resolutionError struct {
	code    string
	message string
}

func (e *resolutionError) Error() string { return e.message }

func unresolved(code, format string, args ...any) error {
	return &resolutionError{code: code, message: fmt.Sprintf(format, args...)}
}

func stringField(o *unstructured.Unstructured, fields ...string) string {
	s, _, _ := unstructured.NestedString(o.Object, fields...)
	return s
}

func readDefinition(o *unstructured.Unstructured) (definition, error) {
	d := definition{object: o.DeepCopy(), versions: []string{}}
	if o.GetAPIVersion() != "apiextensions.crossplane.io/v1" && o.GetAPIVersion() != "apiextensions.crossplane.io/v2" {
		return d, unresolved("unsupported-xrd", "XRD %q has unsupported apiVersion %q", o.GetName(), o.GetAPIVersion())
	}
	d.group = stringField(o, "spec", "group")
	d.kind = stringField(o, "spec", "names", "kind")
	d.scope = stringField(o, "spec", "scope")
	if d.scope == "" {
		d.scope = "LegacyCluster"
		if o.GetAPIVersion() == "apiextensions.crossplane.io/v2" {
			d.scope = "Namespaced"
		}
	}
	if d.group == "" || d.kind == "" {
		return d, unresolved("invalid-xrd", "XRD %q must define spec.group and spec.names.kind", o.GetName())
	}
	switch d.scope {
	case "Namespaced", "Cluster", "LegacyCluster":
	default:
		return d, unresolved("unsupported-xrd-scope", "XRD %q has unsupported scope %q", o.GetName(), d.scope)
	}
	versions, _, err := unstructured.NestedSlice(o.Object, "spec", "versions")
	if err != nil {
		return d, unresolved("invalid-xrd", "XRD %q versions: %v", o.GetName(), err)
	}
	for _, value := range versions {
		version, ok := value.(map[string]any)
		if !ok {
			return d, unresolved("invalid-xrd", "XRD %q has a malformed version", o.GetName())
		}
		served, _ := version["served"].(bool)
		name, _ := version["name"].(string)
		if served && name != "" {
			d.versions = append(d.versions, name)
		}
	}
	if len(d.versions) == 0 {
		return d, unresolved("invalid-xrd", "XRD %q has no served versions", o.GetName())
	}
	if err := unstructured.SetNestedField(d.object.Object, d.scope, "spec", "scope"); err != nil {
		return d, err
	}
	return d, nil
}

func (d definition) matches(o *unstructured.Unstructured) bool {
	group, version, ok := strings.Cut(o.GetAPIVersion(), "/")
	return ok && group == d.group && o.GetKind() == d.kind && slices.Contains(d.versions, version)
}

func controls(xr *unstructured.Unstructured, d definition) *unstructured.Unstructured {
	path := []string{"spec"}
	if d.scope != "LegacyCluster" {
		path = append(path, "crossplane")
	}
	m, _, _ := unstructured.NestedMap(xr.Object, path...)
	if m == nil {
		m = map[string]any{}
	}
	return &unstructured.Unstructured{Object: m}
}

func compatible(comp, xr *unstructured.Unstructured) bool {
	return stringField(comp, "spec", "compositeTypeRef", "apiVersion") == xr.GetAPIVersion() &&
		stringField(comp, "spec", "compositeTypeRef", "kind") == xr.GetKind()
}

func selectComposition(xr *unstructured.Unstructured, d definition, inventory []inventoryResource) (
	*unstructured.Unstructured, error,
) {
	c := controls(xr, d)
	if _, found, _ := unstructured.NestedFieldNoCopy(c.Object, "compositionRevisionSelector"); found {
		return nil, unresolved("unsupported-revision-selector", "compositionRevisionSelector requires a pinned revision manifest")
	}
	enforced := stringField(d.object, "spec", "enforcedCompositionRef", "name")
	explicit := stringField(c, "compositionRef", "name")
	revisionName := stringField(c, "compositionRevisionRef", "name")
	if revisionName != "" {
		return selectRevision(xr, inventory, revisionName, enforced, explicit)
	}
	name := enforced
	if name == "" {
		name = explicit
	}
	candidates := []*unstructured.Unstructured{}
	for _, item := range inventory {
		o := item.object
		if o.GetAPIVersion() == "apiextensions.crossplane.io/v1" && o.GetKind() == "Composition" && compatible(o, xr) {
			candidates = append(candidates, o)
		}
	}
	if name != "" {
		for _, comp := range candidates {
			if comp.GetName() == name {
				return comp.DeepCopy(), nil
			}
		}
		return nil, unresolved("composition-unavailable", "compatible Composition %q is not in the inventory", name)
	}
	selector, hasSelector, err := unstructured.NestedMap(c.Object, "compositionSelector")
	if err != nil {
		return nil, unresolved("invalid-composition-selector", "%v", err)
	}
	if hasSelector {
		ls := &metav1.LabelSelector{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(selector, ls); err != nil {
			return nil, unresolved("invalid-composition-selector", "%v", err)
		}
		match, err := metav1.LabelSelectorAsSelector(ls)
		if err != nil {
			return nil, unresolved("invalid-composition-selector", "%v", err)
		}
		candidates = slices.DeleteFunc(candidates, func(o *unstructured.Unstructured) bool {
			return !match.Matches(labels.Set(o.GetLabels()))
		})
	} else if name = stringField(d.object, "spec", "defaultCompositionRef", "name"); name != "" {
		for _, comp := range candidates {
			if comp.GetName() == name {
				return comp.DeepCopy(), nil
			}
		}
		return nil, unresolved("composition-unavailable", "default Composition %q is not in the inventory", name)
	}
	if len(candidates) == 0 {
		return nil, unresolved("composition-unavailable", "no compatible Composition for %s %q", xr.GetKind(), xr.GetName())
	}
	slices.SortFunc(candidates, func(a, b *unstructured.Unstructured) int {
		return strings.Compare(a.GetName(), b.GetName())
	})
	if len(candidates) != 1 {
		names := make([]string, 0, len(candidates))
		for _, comp := range candidates {
			names = append(names, comp.GetName())
		}
		return nil, unresolved("composition-ambiguous", "multiple compatible Compositions: %s", strings.Join(names, ", "))
	}
	return candidates[0].DeepCopy(), nil
}

func selectRevision(
	xr *unstructured.Unstructured, inventory []inventoryResource, name, enforced, explicit string,
) (*unstructured.Unstructured, error) {
	for _, item := range inventory {
		rev := item.object
		if rev.GetAPIVersion() != "apiextensions.crossplane.io/v1" || rev.GetKind() != "CompositionRevision" || rev.GetName() != name {
			continue
		}
		parent := rev.GetLabels()["crossplane.io/composition-name"]
		if parent == "" {
			for _, owner := range rev.GetOwnerReferences() {
				if owner.Kind == "Composition" && owner.APIVersion == "apiextensions.crossplane.io/v1" {
					parent = owner.Name
				}
			}
		}
		if parent == "" || !compatible(rev, xr) {
			return nil, unresolved("unsupported-pinned-revision", "revision %q lacks compatible type or Composition provenance", name)
		}
		if enforced != "" && enforced != parent {
			return nil, unresolved("revision-enforcement-conflict", "revision %q is not from enforced Composition %q", name, enforced)
		}
		if enforced == "" && explicit != "" && explicit != parent {
			return nil, unresolved("revision-reference-conflict", "revision %q is not from Composition %q", name, explicit)
		}
		comp := rev.DeepCopy()
		comp.SetKind("Composition")
		comp.SetName(parent)
		comp.SetOwnerReferences(nil)
		unstructured.RemoveNestedField(comp.Object, "spec", "revision")
		unstructured.RemoveNestedField(comp.Object, "status")
		return comp, nil
	}
	return nil, unresolved("unsupported-pinned-revision", "pinned CompositionRevision %q is not in the inventory", name)
}
