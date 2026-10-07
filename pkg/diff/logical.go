package diff

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"sigs.k8s.io/kustomize/kyaml/resid"
)

type logicalView struct {
	resource  plugin.Resource
	object    map[string]any
	canonical string
	id        resid.ResId
}

func logicalViews(resources []plugin.Resource) (map[string]logicalView, error) {
	views := make(map[string]logicalView, len(resources))
	for _, resource := range resources {
		if resource.ID == "" || !resource.Logical {
			return nil, fmt.Errorf("logical resource requires a logical ID")
		}
		if _, exists := views[resource.ID]; exists {
			return nil, fmt.Errorf("duplicate logical resource %q", resource.ID)
		}
		object, err := plugin.Object(resource)
		if err != nil {
			return nil, fmt.Errorf("logical resource %q: %w", resource.ID, err)
		}
		apiVersion, _ := object["apiVersion"].(string)
		kind, _ := object["kind"].(string)
		metadata := map[string]any{}
		if raw, exists := object["metadata"]; exists {
			var ok bool
			metadata, ok = raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("logical resource %q: metadata must be an object", resource.ID)
			}
		}
		for _, key := range []string{"name", "namespace", "generateName"} {
			if raw, exists := metadata[key]; exists {
				if _, ok := raw.(string); !ok {
					return nil, fmt.Errorf("logical resource %q: metadata.%s must be a string", resource.ID, key)
				}
			}
		}
		name, _ := metadata["name"].(string)
		namespace, _ := metadata["namespace"].(string)
		if apiVersion == "" || kind == "" || name != "" {
			return nil, fmt.Errorf("logical resource %q requires apiVersion, kind and no metadata.name", resource.ID)
		}
		canonical, err := json.Marshal(object)
		if err != nil {
			return nil, fmt.Errorf("logical resource %q: invalid object: %w", resource.ID, err)
		}
		views[resource.ID] = logicalView{
			resource: resource, object: object, canonical: string(canonical),
			id: resid.NewResIdWithNamespace(resid.NewGvk(gvkGroup(apiVersion), gvkVersion(apiVersion), kind), "", namespace),
		}
	}
	return views, nil
}

func logicalOrigin(view logicalView) render.Provenance {
	p := view.resource.Provenance
	return render.Provenance{Kind: p.Kind, Name: p.Name, Namespace: p.Namespace, Path: p.Path, Text: p.Text}
}

// LogicalChangeSet compares unnamed outputs by their stable logical IDs.
// Object comparison ignores YAML formatting, while diffs retain the original YAML.
func LogicalChangeSet(before, after []plugin.Resource) (*DiffResult, error) {
	left, err := logicalViews(before)
	if err != nil {
		return nil, err
	}
	right, err := logicalViews(after)
	if err != nil {
		return nil, err
	}
	result := &DiffResult{}
	for id, view := range left {
		if _, exists := right[id]; exists {
			continue
		}
		origin := logicalOrigin(view)
		result.Deleted = append(result.Deleted, ResourceChange{
			LogicalID: id, ID: view.id, Kind: view.id.Kind, Namespace: view.id.Namespace,
			BeforeOrigin: &origin, Provenance: origin, Producer: origin.String(),
			Action: "deleted", Old: view.object, oldYAML: view.resource.YAML,
		})
	}
	for id, view := range right {
		old, exists := left[id]
		if exists && old.canonical == view.canonical {
			continue
		}
		origin := logicalOrigin(view)
		change := ResourceChange{
			LogicalID: id, ID: view.id, Kind: view.id.Kind, Namespace: view.id.Namespace,
			AfterOrigin: &origin, Provenance: origin, Producer: origin.String(),
			Action: "added", New: view.object, newYAML: view.resource.YAML,
		}
		if exists {
			beforeOrigin := logicalOrigin(old)
			change.BeforeOrigin = &beforeOrigin
			change.Action, change.Old, change.oldYAML = "modified", old.object, old.resource.YAML
			result.Modified = append(result.Modified, change)
			continue
		}
		result.Added = append(result.Added, change)
	}
	result.Sort()
	return result, nil
}

// DetectLogicalPermadiffs discovers unstable fields in matched logical outputs.
func DetectLogicalPermadiffs(before, after []plugin.Resource) ([]FieldDiff, error) {
	left, err := logicalViews(before)
	if err != nil {
		return nil, err
	}
	right, err := logicalViews(after)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(right))
	for id := range right {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	diffs := []FieldDiff{}
	for _, id := range ids {
		old, exists := left[id]
		if !exists {
			continue
		}
		fields := findDifferences(old.object, right[id].object, nil)
		slices.SortFunc(fields, func(a, b fieldDiffEntry) int { return slices.Compare(a.path, b.path) })
		for _, field := range fields {
			diffs = append(diffs, FieldDiff{
				LogicalID: id, GVK: old.id.Gvk, Namespace: old.id.Namespace,
				FieldPath: field.path, IsLeaf: field.isLeaf,
			})
		}
	}
	return diffs, nil
}
