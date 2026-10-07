package diff

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
)

func TestLogicalCanonicalComparisonPreservesTypedYAML(t *testing.T) {
	before := plugin.Resource{ID: "logical:parent/slot", Logical: true,
		YAML: "apiVersion: v1\nkind: ConfigMap\ndata:\n  value: !!str 001\n"}
	formatOnly := before
	formatOnly.YAML = "kind: ConfigMap\napiVersion: v1\ndata: {value: '001'}\n"
	result, err := LogicalChangeSet([]plugin.Resource{before}, []plugin.Resource{formatOnly})
	if err != nil || result.TotalChanged() != 0 {
		t.Fatalf("format-only change: %+v, %v", result, err)
	}
	after := before
	after.YAML = strings.Replace(before.YAML, "!!str 001", "!!int 1", 1)
	result, err = LogicalChangeSet([]plugin.Resource{before}, []plugin.Resource{after})
	if err != nil || len(result.Modified) != 1 {
		t.Fatalf("typed value change: %+v, %v", result, err)
	}
	change := result.Modified[0]
	if !strings.Contains(change.UnifiedDiff(), "!!str 001") || !strings.Contains(change.UnifiedDiff(), "!!int 1") {
		t.Fatalf("diff lost original tagged YAML: %s", change.UnifiedDiff())
	}
	if change.ID.Name != "" || change.Name != "" || result.ToJSON().Modified[0].LogicalID != before.ID {
		t.Fatal("logical identity projected as a name")
	}
	encoded, err := json.Marshal(result.ToJSON())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"logicalId":"logical:parent/slot"`) || !strings.Contains(string(encoded), `"name":""`) {
		t.Fatalf("JSON logical identity contract lost: %s", encoded)
	}
}

func TestLogicalComparisonRejectsInvalidInventory(t *testing.T) {
	valid := plugin.Resource{ID: "logical:slot", Logical: true, YAML: "apiVersion: v1\nkind: ConfigMap\n"}
	for _, test := range []struct {
		name      string
		resources []plugin.Resource
	}{
		{name: "duplicate", resources: []plugin.Resource{valid, valid}},
		{name: "missing ID", resources: []plugin.Resource{{Logical: true, YAML: valid.YAML}}},
		{name: "named logical", resources: []plugin.Resource{{ID: valid.ID, Logical: true, YAML: valid.YAML + "metadata: {name: invented}\n"}}},
		{name: "multiple documents", resources: []plugin.Resource{{ID: valid.ID, Logical: true, YAML: valid.YAML + "---\n" + valid.YAML}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := LogicalChangeSet(nil, test.resources); err == nil {
				t.Fatal("accepted invalid logical inventory")
			}
		})
	}
}

func TestLogicalNormalizationDetectsScalarTypeChanges(t *testing.T) {
	before := plugin.Resource{ID: "logical:typed", Logical: true,
		YAML: "apiVersion: v1\nkind: ConfigMap\ndata: {value: !!str 1}\n"}
	after := before
	after.YAML = strings.Replace(before.YAML, "!!str", "!!int", 1)
	findings, err := DetectLogicalPermadiffs([]plugin.Resource{before}, []plugin.Resource{after})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || strings.Join(findings[0].FieldPath, ".") != "data.value" {
		t.Fatalf("scalar type instability omitted: %+v", findings)
	}
}
