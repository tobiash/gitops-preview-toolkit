package crossplanerender

import (
	"reflect"
	"testing"
)

func TestCompositeEvidencePreservesActualTimestampsAndOwnsItsCopy(t *testing.T) {
	condition := map[string]any{"type": "Ready", "lastTransitionTime": "2026-10-07T12:34:56Z"}
	object := map[string]any{"status": map[string]any{"conditions": []any{condition}}}
	evidence := stableCompositeEvidence(object)
	if !reflect.DeepEqual(evidence, object) {
		t.Fatalf("evidence changed factual condition data: %#v", evidence)
	}
	copyCondition := evidence["status"].(map[string]any)["conditions"].([]any)[0].(map[string]any)
	copyCondition["lastTransitionTime"] = "changed"
	if condition["lastTransitionTime"] != "2026-10-07T12:34:56Z" {
		t.Fatal("evidence shares mutable condition storage with the engine result")
	}
}
