package crossplanerender

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/crossplane/crossplane-runtime/v2/pkg/resource/unstructured/composed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/xcrd"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
)

func stableCompositeEvidence(object map[string]any) map[string]any {
	o := &unstructured.Unstructured{Object: object}
	return o.DeepCopy().Object
}

func desiredResources(xr inventoryResource, outputs []composed.Unstructured, declared map[string]string) ([]plugin.Resource, error) {
	resources := make([]plugin.Resource, 0, len(outputs))
	seen := map[string]bool{}
	provenance := plugin.Provenance{
		Kind: "XR", Name: xr.object.GetName(), Namespace: xr.object.GetNamespace(),
		Text: fmt.Sprintf("XR %s %s/%s", xr.object.GetKind(), xr.object.GetNamespace(), xr.object.GetName()),
	}
	for _, output := range outputs {
		o := output.DeepCopy()
		key := o.GetAnnotations()[xcrd.AnnotationKeyCompositionResourceName]
		if key == "" {
			return nil, unresolved("output-resource-name-missing", "composed output lacks composition-resource-name annotation")
		}
		name, known := declared[key]
		if !known {
			return nil, unresolved("output-name-evidence-missing", "no actual function naming evidence for composed resource %q", key)
		}
		// Status belongs to evidence, not desired inventory. Engine-generated
		// metadata must not be presented as identities assigned by a cluster.
		unstructured.RemoveNestedField(o.Object, "status")
		for _, field := range []string{"uid", "resourceVersion", "managedFields", "ownerReferences", "creationTimestamp"} {
			unstructured.RemoveNestedField(o.Object, "metadata", field)
		}
		logical := name == ""
		if logical {
			unstructured.RemoveNestedField(o.Object, "metadata", "name")
		}
		data, err := yaml.Marshal(o.Object)
		if err != nil {
			return nil, fmt.Errorf("marshal composed output %q: %w", key, err)
		}
		var resource plugin.Resource
		if logical {
			// A logical ID is producer-relative, independent of synthetic engine
			// names and generated-name prefixes. Preserve metadata.generateName.
			identity, _ := json.Marshal([]string{xr.resource.ID, key})
			hash := sha256.Sum256(identity)
			resource = plugin.Resource{
				ID: "logical:crossplane:" + hex.EncodeToString(hash[:]), YAML: string(data),
				Logical: true, Provenance: provenance,
			}
		} else {
			if o.GetName() != name {
				return nil, unresolved("output-name-mismatch", "engine changed explicitly named output %q", key)
			}
			resource, err = plugin.ParseResource(data, provenance)
			if err != nil {
				return nil, fmt.Errorf("parse named composed output %q: %w", key, err)
			}
		}
		if seen[resource.ID] {
			return nil, unresolved("output-identity-conflict", "duplicate desired output identity %q", resource.ID)
		}
		seen[resource.ID] = true
		resources = append(resources, resource)
	}
	slices.SortFunc(resources, func(a, b plugin.Resource) int { return strings.Compare(a.ID, b.ID) })
	return resources, nil
}
