package githubaction

import (
	"encoding/json"
	"strings"
	"testing"

	fmpdiff "github.com/tobiash/gitops-preview-toolkit/pkg/diff"
	"sigs.k8s.io/kustomize/kyaml/resid"
)

func TestHTMLReportLogicalSlots(t *testing.T) {
	result := &fmpdiff.DiffResult{}
	for _, slot := range []string{"second", "first"} {
		result.Added = append(result.Added, fmpdiff.ResourceChange{
			LogicalID: "xr/parent/" + slot,
			ID:        resid.NewResIdWithNamespace(resid.Gvk{Version: "v1", Kind: "Secret"}, "", "apps"),
			Kind:      "Secret", Namespace: "apps", Producer: "XR apps/parent", Action: "added",
			New: map[string]any{"metadata": map[string]any{"annotations": map[string]any{
				"crossplane.io/composition-resource-name": slot,
			}}},
		})
	}
	data := BuildHTMLReportData(&Request{}, BuildReport(ReportInput{Result: result}), result)
	if len(data.Resources) != 2 || data.Resources[0].ID == data.Resources[1].ID {
		t.Fatalf("logical slots collapsed: %+v", data.Resources)
	}
	for i, slot := range []string{"first", "second"} {
		resource := data.Resources[i]
		if resource.Index != i || resource.Name != "" || resource.Slot != slot || resource.LogicalID != "xr/parent/"+slot {
			t.Fatalf("incorrect slot projection: %+v", resource)
		}
	}
	encoded, err := reportDataJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	var decoded HTMLReportData
	if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Resources[0].LogicalID != data.Resources[0].LogicalID {
		t.Fatalf("JSON lost logical identity: %+v", decoded.Resources)
	}
	html, err := RenderHTMLReport(data)
	if err != nil {
		t.Fatal(err)
	}
	js, err := reportUI.ReadFile("reportui/report.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(html, string(js)) || !strings.Contains(html, `"slot":"first"`) {
		t.Fatal("rendered report does not include runtime JS and logical slot data")
	}
}

func TestHTMLReportSlotFallbackAndNamedCompatibility(t *testing.T) {
	change := fmpdiff.ResourceChange{
		LogicalID: "xr/parent/output", Kind: "Secret", Action: "deleted",
		Old: map[string]any{"metadata": map[string]any{"annotations": map[string]any{
			"crossplane.io/composition-resource-name": "old-slot",
		}}},
	}
	if got := htmlResourceChanges([]fmpdiff.ResourceChange{change}, 0)[0]; got.Slot != "old-slot" || got.Name != "" {
		t.Fatalf("deleted slot fallback = %+v", got)
	}
	change.LogicalID = ""
	change.Name = "named-secret"
	change.ID = resid.NewResId(resid.Gvk{Version: "v1", Kind: "Secret"}, change.Name)
	got := htmlResourceChanges([]fmpdiff.ResourceChange{change}, 0)[0]
	if got.ID != "||v1|Secret||named-secret" || got.Slot != "" || got.Name != change.Name {
		t.Fatalf("named report changed: %+v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "logicalId") || strings.Contains(string(encoded), `"slot"`) {
		t.Fatalf("named report gained empty properties: %s", encoded)
	}
}

func TestParseUnifiedDiffRows(t *testing.T) {
	rows := parseUnifiedDiffRows("--- a\n+++ b\n@@ -2,2 +2,3 @@\n keep\n-old\n+new\n+next")
	if len(rows) != 5 {
		t.Fatalf("len(rows) = %d, want 5", len(rows))
	}
	if rows[0].Type != "hunk" {
		t.Fatalf("rows[0].Type = %q, want hunk", rows[0].Type)
	}
	if rows[1].Type != "context" || rows[1].OldLine != 2 || rows[1].NewLine != 2 {
		t.Fatalf("context row = %+v, want old/new line 2", rows[1])
	}
	if rows[2].Type != "deleted" || rows[2].OldLine != 3 || rows[2].OldText != "old" {
		t.Fatalf("deleted row = %+v", rows[2])
	}
	if rows[3].Type != "added" || rows[3].NewLine != 3 || rows[3].NewText != "new" {
		t.Fatalf("added row = %+v", rows[3])
	}
	if rows[4].Type != "added" || rows[4].NewLine != 4 || rows[4].NewText != "next" {
		t.Fatalf("added row[4] = %+v", rows[4])
	}
}

func TestBuildHTMLReportDataKeepsSameResourceInDifferentClustersDistinct(t *testing.T) {
	result := &fmpdiff.DiffResult{
		Modified: []fmpdiff.ResourceChange{
			{
				Cluster:   "staging",
				ID:        resid.NewResIdWithNamespace(resid.Gvk{Group: "apps", Version: "v1", Kind: "Deployment"}, "checkout-api", "apps"),
				Kind:      "Deployment",
				Name:      "checkout-api",
				Namespace: "apps",
				Producer:  "apps/checkout",
				Action:    "modified",
				Old:       map[string]any{"kind": "Deployment", "metadata": map[string]any{"name": "checkout-api", "namespace": "apps"}, "spec": map[string]any{"replicas": 2}},
				New:       map[string]any{"kind": "Deployment", "metadata": map[string]any{"name": "checkout-api", "namespace": "apps"}, "spec": map[string]any{"replicas": 3}},
			},
			{
				Cluster:   "production",
				ID:        resid.NewResIdWithNamespace(resid.Gvk{Group: "apps", Version: "v1", Kind: "Deployment"}, "checkout-api", "apps"),
				Kind:      "Deployment",
				Name:      "checkout-api",
				Namespace: "apps",
				Producer:  "apps/checkout",
				Action:    "modified",
				Old:       map[string]any{"kind": "Deployment", "metadata": map[string]any{"name": "checkout-api", "namespace": "apps"}, "spec": map[string]any{"replicas": 2}},
				New:       map[string]any{"kind": "Deployment", "metadata": map[string]any{"name": "checkout-api", "namespace": "apps"}, "spec": map[string]any{"replicas": 4}},
			},
		},
	}
	report := &ActionReport{Status: StatusChanged, ResourcesModified: 2, ResourcesTotal: 2}

	data := BuildHTMLReportData(&Request{}, report, result)

	if len(data.Resources) != 2 {
		t.Fatalf("len(data.Resources) = %d, want 2", len(data.Resources))
	}
	if data.Resources[0].ID == data.Resources[1].ID {
		t.Fatalf("resource IDs should include cluster and be distinct, got %q", data.Resources[0].ID)
	}
	if !strings.HasPrefix(data.Resources[0].ID, "production|") || !strings.HasPrefix(data.Resources[1].ID, "staging|") {
		t.Fatalf("resources sorted/identified without cluster: %#v", []string{data.Resources[0].ID, data.Resources[1].ID})
	}
}

func TestRenderHTMLReportEscapesScriptTerminators(t *testing.T) {
	data := HTMLReportData{
		Meta: HTMLReportMeta{Status: StatusChanged},
		Resources: []HTMLResourceChange{{
			Kind:     "ConfigMap",
			Name:     "bad</script><script>alert(1)</script>",
			Action:   "modified",
			DiffRows: []HTMLDiffRow{{Type: "added", NewLine: 1, NewText: "</script><script>alert(1)</script>"}},
		}},
	}

	html, err := RenderHTMLReport(data)
	if err != nil {
		t.Fatalf("RenderHTMLReport() error = %v", err)
	}
	if !strings.Contains(html, "id=\"fmp-report-data\"") {
		t.Fatal("missing report data script")
	}
	if strings.Contains(html, "</script><script>alert") {
		t.Fatalf("script terminator was not escaped: %s", html)
	}
	if !strings.Contains(html, "gitops-preview-toolkit Report") {
		t.Fatal("missing report title")
	}
}
