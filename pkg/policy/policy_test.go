package policy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/diff"
)

func TestEvaluateLogicalFindingsRemainDistinct(t *testing.T) {
	changes := []diff.ResourceChange{}
	for _, id := range []string{"xr/parent/b", "xr/parent/a"} {
		changes = append(changes, diff.ResourceChange{
			Action: "added", Kind: "Secret", Namespace: "apps", Producer: "XR apps/parent", LogicalID: id,
		})
	}
	result, err := Evaluate(t.Context(), &diff.DiffResult{Added: changes}, &config.PolicyConfig{
		Builtin: []string{"secret_change"},
		Inline: []string{`package fmp
import rego.v1
violations contains {"id": "review_slot", "logicalId": change.logicalId} if {
  some change in input.changes
}`},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Classifications) != 2 || len(result.Violations) != 2 {
		t.Fatalf("logical findings collapsed: %+v", result)
	}
	for i, id := range []string{"xr/parent/a", "xr/parent/b"} {
		if result.Classifications[i].LogicalID != id || result.Violations[i].LogicalID != id {
			t.Fatalf("logical findings lost identity or deterministic ordering: %+v", result)
		}
		if result.Classifications[i].Name != "" {
			t.Fatalf("invented name: %+v", result.Classifications[i])
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Result
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Classifications[1].LogicalID != "xr/parent/b" || decoded.Violations[1].LogicalID != "xr/parent/b" {
		t.Fatalf("result JSON lost logical identity: %s", encoded)
	}
}

func TestNamedPolicyInputOmitsLogicalID(t *testing.T) {
	result, err := Evaluate(t.Context(), &diff.DiffResult{Added: []diff.ResourceChange{{
		Action: "added", Kind: "Secret", Name: "credentials", Namespace: "apps",
	}}}, &config.PolicyConfig{
		Builtin: []string{"secret_change"},
		Inline: []string{`package fmp
import rego.v1
labels contains "legacy-compatible" if {
  some change in input.changes
  change.name == "credentials"
  not "logicalId" in object.keys(change)
}`},
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Classifications) != 1 || len(result.Labels) != 1 || result.Labels[0] != "legacy-compatible" {
		t.Fatalf("named policy compatibility changed: %+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "logicalId") {
		t.Fatalf("named findings gained logicalId: %s", encoded)
	}
}

func TestAllBuiltinsAttributeLogicalSlots(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
	}{
		{name: "image_update", kind: "Deployment"},
		{name: "secret_change", kind: "Secret"},
		{name: "ingress_change", kind: "Ingress"},
		{name: "crd_change", kind: "CustomResourceDefinition"},
		{name: "namespace_delete", kind: "Namespace"},
		{name: "stateful_workload_change", kind: "StatefulSet"},
		{name: "pvc_change", kind: "PersistentVolumeClaim"},
		{name: "service_type_change", kind: "Service"},
		{name: "replicas_change", kind: "Deployment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changes := []diff.ResourceChange{}
			for _, id := range []string{"xr/parent/b", "xr/parent/a"} {
				changes = append(changes, diff.ResourceChange{
					Action: "modified", Kind: tc.kind, Namespace: "apps", LogicalID: id,
					Old: map[string]any{"spec": map[string]any{
						"type": "ClusterIP", "replicas": 1,
						"template": map[string]any{"spec": map[string]any{
							"containers": []any{map[string]any{"image": "old:1"}},
						}},
					}},
					New: map[string]any{"spec": map[string]any{
						"type": "LoadBalancer", "replicas": 2,
						"template": map[string]any{"spec": map[string]any{
							"containers": []any{map[string]any{"image": "new:2"}},
						}},
					}},
				})
			}
			input := &diff.DiffResult{Modified: changes}
			if tc.name == "namespace_delete" {
				for i := range changes {
					changes[i].Action = "deleted"
				}
				input = &diff.DiffResult{Deleted: changes}
			}
			result, err := Evaluate(t.Context(), input, &config.PolicyConfig{Builtin: []string{tc.name}}, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Classifications) != 2 {
				t.Fatalf("builtin collapsed logical slots: %+v", result.Classifications)
			}
			for i, id := range []string{"xr/parent/a", "xr/parent/b"} {
				if result.Classifications[i].ID != tc.name || result.Classifications[i].LogicalID != id {
					t.Fatalf("incorrect logical attribution: %+v", result.Classifications)
				}
			}
		})
	}
}

func TestEvaluateBuiltinsAndMappings(t *testing.T) {
	result, err := Evaluate(context.Background(), &diff.DiffResult{
		Modified: []diff.ResourceChange{{
			Action:    "modified",
			Cluster:   "staging",
			Kind:      "Deployment",
			Name:      "web",
			Namespace: "default",
			Old:       map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"image": "nginx:1.0"}}}}}},
			New:       map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"image": "nginx:1.1"}}}}}},
		}},
	}, &config.PolicyConfig{
		Builtin: []string{"image_update"},
		Labels:  map[string]config.LabelList{"image_update": {"image-update"}},
	}, t.TempDir())
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if len(result.Classifications) != 1 || result.Classifications[0].ID != "image_update" {
		t.Fatalf("expected image_update classification, got %+v", result.Classifications)
	}
	if result.Classifications[0].Cluster != "staging" {
		t.Fatalf("expected cluster on classification, got %+v", result.Classifications[0])
	}
	if result.Classifications[0].Priority != 20 {
		t.Fatalf("expected priority on classification, got %+v", result.Classifications[0])
	}
	if result.Classifications[0].Provenance != "policy" {
		t.Fatalf("expected policy provenance, got %+v", result.Classifications[0])
	}
	if len(result.Labels) != 1 || result.Labels[0] != "image-update" {
		t.Fatalf("expected mapped label, got %v", result.Labels)
	}
}

func TestApplyEffectsUsesAIClassifications(t *testing.T) {
	result := &Result{
		Classifications: []Classification{{ID: "needs_ai_review", Provenance: "ai"}},
	}
	ApplyEffects(result, &config.PolicyConfig{
		FailOn: []string{"needs_ai_review"},
		Labels: map[string]config.LabelList{"needs_ai_review": {"ai-review"}},
	})
	if !result.PolicyFailed {
		t.Fatal("expected AI classification to match fail-on")
	}
	if len(result.PolicyFailures) != 1 || result.PolicyFailures[0] != "needs_ai_review" {
		t.Fatalf("expected needs_ai_review failure, got %v", result.PolicyFailures)
	}
	if len(result.Labels) != 1 || result.Labels[0] != "ai-review" {
		t.Fatalf("expected ai-review label, got %v", result.Labels)
	}
}

func TestEvaluateAllowsEffectsOnlyConfig(t *testing.T) {
	result, err := Evaluate(context.Background(), &diff.DiffResult{}, &config.PolicyConfig{
		FailOn: []string{"ai_only"},
		Labels: map[string]config.LabelList{"ai_only": {"ai-review"}},
	}, t.TempDir())
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if result == nil {
		t.Fatal("expected empty policy result")
	}
}

func TestEvaluateBuiltinWithoutCluster(t *testing.T) {
	result, err := Evaluate(context.Background(), &diff.DiffResult{
		Modified: []diff.ResourceChange{{
			Action:    "modified",
			Kind:      "Deployment",
			Name:      "web",
			Namespace: "default",
			Old:       map[string]any{"spec": map[string]any{"replicas": 1}},
			New:       map[string]any{"spec": map[string]any{"replicas": 2}},
		}},
	}, &config.PolicyConfig{Builtin: []string{"replicas_change"}}, t.TempDir())
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if len(result.Classifications) != 1 || result.Classifications[0].Cluster != "" {
		t.Fatalf("expected classification with empty cluster, got %+v", result.Classifications)
	}
}

func TestEvaluateCustomModulesAndFailOn(t *testing.T) {
	dir := t.TempDir()
	modulePath := filepath.Join(dir, "policy.rego")
	if err := os.WriteFile(modulePath, []byte(`package fmp
import rego.v1

violations contains {
  "id": "forbid_latest",
  "message": "latest tag is forbidden",
  "severity": "error"
} if {
  some change in input.changes
  change.new.spec.template.spec.containers[_].image == "nginx:latest"
}

labels contains "needs-manual-review" if true
`), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	result, err := Evaluate(context.Background(), &diff.DiffResult{
		Modified: []diff.ResourceChange{{
			Action:    "modified",
			Kind:      "Deployment",
			Name:      "web",
			Namespace: "default",
			New:       map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"image": "nginx:latest"}}}}}},
		}},
	}, &config.PolicyConfig{
		Modules: []string{"*.rego"},
		FailOn:  []string{"forbid_latest"},
	}, dir)
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if !result.PolicyFailed {
		t.Fatal("expected policy failure")
	}
	if len(result.PolicyFailures) != 1 || result.PolicyFailures[0] != "forbid_latest" {
		t.Fatalf("expected forbid_latest failure, got %v", result.PolicyFailures)
	}
	if len(result.Labels) != 1 || result.Labels[0] != "needs-manual-review" {
		t.Fatalf("expected direct label from policy, got %v", result.Labels)
	}
}
