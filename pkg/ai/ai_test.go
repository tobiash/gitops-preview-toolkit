package ai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/diff"
	"github.com/tobiash/gitops-preview-toolkit/pkg/policy"
)

func TestPayloadDistinguishesRedactedLogicalSecrets(t *testing.T) {
	result := &diff.DiffResult{}
	for _, id := range []string{"xr/parent/a", "xr/parent/b"} {
		result.Added = append(result.Added, diff.ResourceChange{
			Action: "added", Kind: "Secret", Namespace: "apps", Producer: "XR apps/parent", LogicalID: id,
			New: map[string]any{"data": map[string]any{"password": "sensitive-value"}},
		})
	}
	text, truncated, err := buildInputPayload(Request{Result: result}, inputLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if truncated || strings.Contains(text, "sensitive-value") {
		t.Fatalf("unexpected payload truncation or secret leak: %s", text)
	}
	var input payload
	if err := json.Unmarshal([]byte(text), &input); err != nil {
		t.Fatal(err)
	}
	if len(input.Changes) != 2 || input.Changes[0].LogicalID == input.Changes[1].LogicalID {
		t.Fatalf("redacted slots indistinguishable: %s", text)
	}
	for i, id := range []string{"xr/parent/a", "xr/parent/b"} {
		change := input.Changes[i]
		if change.LogicalID != id || change.Name != "" || change.UnifiedDiff != "[redacted secret diff]" {
			t.Fatalf("incorrect logical secret projection: %+v", change)
		}
	}
}

func TestAssessmentLogicalAttribution(t *testing.T) {
	assessment, err := decodeAssessment(`{"classifications":[{"id":"review","logicalId":"xr/parent/a"},{"id":"review","logicalId":"xr/parent/b"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := filterClassifications(assessment.Classifications, []string{"review"}, nil)
	if len(items) != 2 || items[0].LogicalID != "xr/parent/a" || items[1].LogicalID != "xr/parent/b" {
		t.Fatalf("assessment lost logical attribution: %+v", items)
	}
	if !strings.Contains(systemPrompt(&config.AIConfig{}, false), `"logicalId":"optional"`) {
		t.Fatal("assessment schema omits logical attribution")
	}
	encoded, err := json.Marshal(assessment)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"logicalId":"xr/parent/a"`) {
		t.Fatalf("assessment JSON lost logical identity: %s", encoded)
	}
}

func TestNamedAIPayloadOmitsLogicalID(t *testing.T) {
	text, _, err := buildInputPayload(Request{Result: &diff.DiffResult{
		Added: []diff.ResourceChange{{Action: "added", Kind: "Secret", Name: "credentials"}},
	}}, inputLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "logicalId") || !strings.Contains(text, `"name":"credentials"`) {
		t.Fatalf("named AI input compatibility changed: %s", text)
	}
}

func TestBoundedResourceDiffRedactsSecretResources(t *testing.T) {
	got, truncated := boundedResourceDiff(diff.ResourceChange{Kind: "Secret"}, 80)
	if truncated {
		t.Fatal("expected secret redaction not to mark diff as truncated")
	}
	if got != "[redacted secret diff]" {
		t.Fatalf("expected redacted secret diff, got %q", got)
	}
}

func TestBoundedResourceDiffRedactsSensitiveLines(t *testing.T) {
	got, _ := boundedResourceDiff(diff.ResourceChange{
		Kind: "ConfigMap",
		Old:  map[string]any{"data": map[string]any{"DATABASE_PASSWORD": "alpha-secret"}},
		New:  map[string]any{"data": map[string]any{"DATABASE_PASSWORD": "beta-secret"}},
	}, 80)
	if strings.Contains(got, "alpha-secret") || strings.Contains(got, "beta-secret") {
		t.Fatalf("expected sensitive values to be redacted, got %q", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("expected redaction marker, got %q", got)
	}
}

func TestFilterClassificationsDropsDisallowedIDs(t *testing.T) {
	items, warnings := filterClassifications([]policy.Classification{
		{ID: "allowed", Provenance: "ai"},
		{ID: "blocked", Provenance: "ai"},
	}, []string{"allowed"}, nil)
	if len(items) != 1 || items[0].ID != "allowed" {
		t.Fatalf("expected only allowed classification, got %+v", items)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "blocked") {
		t.Fatalf("expected warning about blocked classification, got %v", warnings)
	}
}

func TestDecodeAssessmentSetsAIProvenance(t *testing.T) {
	assessment, err := decodeAssessment(`{"summary":"review this","classifications":[{"id":"network_change"}]}`)
	if err != nil {
		t.Fatalf("decodeAssessment() error = %v", err)
	}
	if len(assessment.Classifications) != 1 {
		t.Fatalf("expected classification, got %+v", assessment.Classifications)
	}
	if assessment.Classifications[0].Provenance != "ai" {
		t.Fatalf("expected ai provenance, got %+v", assessment.Classifications[0])
	}
}

func TestDecodeAssessmentExtractsJSONAfterThinking(t *testing.T) {
	assessment, err := decodeAssessment(`<think>reasoning</think>

{"summary":"review this","classifications":[{"id":"config_change"}]}`)
	if err != nil {
		t.Fatalf("decodeAssessment() error = %v", err)
	}
	if assessment.Summary != "review this" {
		t.Fatalf("expected summary from extracted JSON, got %+v", assessment)
	}
	if len(assessment.Classifications) != 1 || assessment.Classifications[0].Provenance != "ai" {
		t.Fatalf("expected ai classification from extracted JSON, got %+v", assessment.Classifications)
	}
}

func TestResolveProviderSupportsMiniMaxAndOpenRouter(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		envName  string
	}{
		{name: "minimax", provider: ProviderMiniMax, envName: "MINIMAX_API_KEY"},
		{name: "openrouter", provider: ProviderOpenRouter, envName: "OPENROUTER_API_KEY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assessor := &LangChainAssessor{env: func(name string) string {
				if name == tt.envName {
					return "token"
				}
				return ""
			}}
			provider, token, err := assessor.resolveProvider(&config.AIConfig{Provider: tt.provider})
			if err != nil {
				t.Fatalf("resolveProvider() error = %v", err)
			}
			if provider != tt.provider || token != "token" {
				t.Fatalf("resolveProvider() = %q, %q; want %q, token", provider, token, tt.provider)
			}
		})
	}
}
