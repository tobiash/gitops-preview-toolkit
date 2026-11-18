package fluxrender

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/adler32"
	"sort"
	"strings"
	"unicode"

	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	kubejson "sigs.k8s.io/json"
	"sigs.k8s.io/kustomize/api/resource"
)

const resourceSetAPIVersion = "fluxcd.controlplane.io/v1"

const maxResourceSetInputs = 10000

type resourceSetInputGroup struct {
	name string
	sets []map[string]any
}

type resourceSetInputStrategy struct {
	Name                  string `json:"name"`
	IncludeEmptyProviders bool   `json:"includeEmptyProviders"`
}

type resourceSetInputReference struct {
	APIVersion string                `json:"apiVersion"`
	Kind       string                `json:"kind"`
	Name       string                `json:"name"`
	Selector   *metav1.LabelSelector `json:"selector"`
}

func resourceSetInputs(ctx context.Context, object map[string]any, control *resource.Resource, inventory *render.Render) ([]map[string]any, bool, error) {
	var inline []map[string]any
	if err := decodeResourceSetField(object, &inline, "spec", "inputs"); err != nil {
		return nil, false, err
	}
	for _, in := range inline {
		if in == nil {
			return nil, false, fmt.Errorf("ResourceSet input must be an object")
		}
		in["provider"] = resourceSetProviderReference(control)
	}
	var refs []resourceSetInputReference
	if err := decodeResourceSetField(object, &refs, "spec", "inputsFrom"); err != nil {
		return nil, false, err
	}
	if len(refs) > maxResourceSetProviders {
		return nil, true, fmt.Errorf("ResourceSet exceeds %d provider references", maxResourceSetProviders)
	}
	selected := map[string]*resource.Resource{}
	for _, ref := range refs {
		if (ref.Kind != "" && ref.Kind != "ResourceSetInputProvider") || (ref.APIVersion != "" && ref.APIVersion != resourceSetAPIVersion) || (ref.Name == "") == (ref.Selector == nil) {
			return nil, true, fmt.Errorf("unsupported or invalid ResourceSet inputsFrom reference: %+v", ref)
		}
		selector := labels.Everything()
		if ref.Selector != nil {
			var err error
			selector, err = metav1.LabelSelectorAsSelector(ref.Selector)
			if err != nil {
				return nil, true, fmt.Errorf("ResourceSet input selector: %w", err)
			}
		}
		found := false
		for _, candidate := range inventory.Resources() {
			if err := ctx.Err(); err != nil {
				return nil, true, err
			}
			if candidate.GetApiVersion() != resourceSetAPIVersion || candidate.GetKind() != "ResourceSetInputProvider" || candidate.GetNamespace() != control.GetNamespace() {
				continue
			}
			if (ref.Name != "" && ref.Name != candidate.GetName()) || !selector.Matches(labels.Set(candidate.GetLabels())) {
				continue
			}
			selected[candidate.GetName()] = candidate
			if len(selected) > maxResourceSetProviders {
				return nil, true, fmt.Errorf("ResourceSet exceeds %d selected providers", maxResourceSetProviders)
			}
			found = true
		}
		if ref.Name != "" && !found {
			return nil, true, fmt.Errorf("ResourceSet input provider %s/%s not found", control.GetNamespace(), ref.Name)
		}
	}
	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	groups := []resourceSetInputGroup{{name: control.GetName(), sets: inline}}
	providerBytes := 0
	for _, name := range names {
		provider := selected[name]
		size := len(provider.MustYaml())
		if size > maxResourceSetBytes-providerBytes {
			return nil, true, fmt.Errorf("ResourceSet providers exceed %d total input bytes", maxResourceSetBytes)
		}
		providerBytes += size
		obj, err := provider.Map()
		if err != nil {
			return nil, true, err
		}
		kind, _, err := unstructured.NestedString(obj, "spec", "type")
		if err != nil {
			return nil, true, err
		}
		if kind == "" {
			return nil, true, fmt.Errorf("ResourceSet input provider %s requires spec.type", name)
		}
		var sets []map[string]any
		if kind == "Static" {
			if strings.EqualFold(provider.GetAnnotations()["fluxcd.controlplane.io/reconcile"], "disabled") {
				return nil, true, fmt.Errorf("ResourceSet Static provider %s is disabled; offline input synthesis is unsupported", name)
			}
			var spec struct {
				Type          string         `json:"type"`
				DefaultValues map[string]any `json:"defaultValues"`
			}
			if err := decodeResourceSetField(obj, &spec, "spec"); err != nil {
				return nil, true, err
			}
			defaults := spec.DefaultValues
			if defaults == nil {
				if value, present, _ := unstructured.NestedFieldNoCopy(obj, "spec", "defaultValues"); present && value == nil {
					return nil, true, fmt.Errorf("ResourceSet Static provider defaultValues must be an object")
				}
				defaults = map[string]any{}
			}
			uid, _, err := unstructured.NestedString(obj, "metadata", "uid")
			if err != nil {
				return nil, true, err
			}
			// Manifests have no API-server UID. A stable namespaced identity
			// provides the offline equivalent without inventing live metadata.
			if uid == "" {
				uid = provider.GetNamespace() + "/" + provider.GetName()
			}
			defaults["id"] = fmt.Sprint(adler32.Checksum([]byte(uid)))
			sets = []map[string]any{defaults}
		} else {
			_, present, err := unstructured.NestedFieldNoCopy(obj, "status", "exportedInputs")
			if err != nil || !present {
				return nil, true, fmt.Errorf("ResourceSet input provider %s/%s type %q requires pre-baked status.exportedInputs; live discovery is unsupported", provider.GetNamespace(), name, kind)
			}
			if err := decodeResourceSetField(obj, &sets, "status", "exportedInputs"); err != nil {
				return nil, true, err
			}
		}
		for _, in := range sets {
			if in == nil {
				return nil, true, fmt.Errorf("ResourceSet provider %s input must be an object", name)
			}
			in["provider"] = resourceSetProviderReference(provider)
		}
		groups = append(groups, resourceSetInputGroup{name: name, sets: sets})
	}
	var strategy resourceSetInputStrategy
	if err := decodeResourceSetField(object, &strategy, "spec", "inputStrategy"); err != nil {
		return nil, true, err
	}
	combined, err := combineResourceSetInputs(ctx, groups, strategy)
	return combined, len(refs) > 0, err
}

func combineResourceSetInputs(ctx context.Context, groups []resourceSetInputGroup, strategy resourceSetInputStrategy) ([]map[string]any, error) {
	if strategy.Name == "" || strategy.Name == "Flatten" {
		if strategy.IncludeEmptyProviders {
			return nil, fmt.Errorf("ResourceSet includeEmptyProviders requires Permute")
		}
		combined := []map[string]any{}
		for _, group := range groups {
			if len(group.sets) > maxResourceSetInputs-len(combined) {
				return nil, fmt.Errorf("ResourceSet exceeds %d combined inputs", maxResourceSetInputs)
			}
			combined = append(combined, group.sets...)
		}
		return combined, nil
	}
	if strategy.Name != "Permute" {
		return nil, fmt.Errorf("unsupported ResourceSet input strategy %q", strategy.Name)
	}
	combined := []map[string]any{{}}
	ids := []string{""}
	scopes := map[string]bool{}
	for _, group := range groups {
		if len(group.sets) == 0 && !strategy.IncludeEmptyProviders {
			continue
		}
		scope := normalizeResourceSetKey(group.name)
		if scope == "" || scope == "id" || scopes[scope] {
			return nil, fmt.Errorf("ResourceSet Permute provider %q has an empty, reserved or duplicate normalized name %q", group.name, scope)
		}
		scopes[scope] = true
		if len(group.sets) == 0 {
			return []map[string]any{}, nil
		}
		if len(group.sets) > maxResourceSetInputs/len(combined) {
			return nil, fmt.Errorf("ResourceSet exceeds %d permutations", maxResourceSetInputs)
		}
		next := []map[string]any{}
		nextIDs := []string{}
		for i, prefix := range combined {
			for j, in := range group.sets {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				permutation := make(map[string]any, len(prefix)+1)
				for key, value := range prefix {
					permutation[key] = value
				}
				permutation[scope] = in
				parts := fmt.Sprintf("%s=%d", scope, j)
				if ids[i] != "" {
					parts = ids[i] + "/" + parts
				}
				next = append(next, permutation)
				nextIDs = append(nextIDs, parts)
			}
		}
		combined, ids = next, nextIDs
	}
	if len(scopes) == 0 {
		return []map[string]any{}, nil
	}
	for i, in := range combined {
		in["id"] = fmt.Sprint(adler32.Checksum([]byte(ids[i])))
	}
	return combined, nil
}

func normalizeResourceSetKey(name string) string {
	var normalized strings.Builder
	separator := false
	for _, r := range strings.ToLower(name) {
		if unicode.IsPunct(r) || unicode.IsSpace(r) {
			separator = normalized.Len() > 0
		} else if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if separator {
				normalized.WriteByte('_')
				separator = false
			}
			normalized.WriteRune(r)
		}
	}
	return normalized.String()
}

func resourceSetProviderReference(provider *resource.Resource) map[string]any {
	return map[string]any{"apiVersion": resourceSetAPIVersion, "kind": provider.GetKind(), "name": provider.GetName(), "namespace": provider.GetNamespace()}
}

func decodeResourceSetField(object map[string]any, target any, path ...string) error {
	value, found, err := unstructured.NestedFieldNoCopy(object, path...)
	if err != nil || !found {
		return err
	}
	if value == nil {
		return fmt.Errorf("ResourceSet %v must not be null", path)
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	strictErrors, err := kubejson.UnmarshalStrict(data, target)
	if err != nil {
		return fmt.Errorf("ResourceSet %v: %w", path, err)
	}
	if len(strictErrors) > 0 {
		return fmt.Errorf("ResourceSet %v: %w", path, errors.Join(strictErrors...))
	}
	return nil
}
