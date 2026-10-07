package githubaction

import (
	"strings"
	"testing"

	"github.com/tobiash/gitops-preview-toolkit/pkg/policy"
)

func TestStatusFromCounts(t *testing.T) {
	tests := []struct {
		changed  bool
		warnings int
		errors   int
		want     string
	}{
		{false, 0, 0, StatusClean},
		{true, 0, 0, StatusChanged},
		{false, 1, 0, StatusWarning},
		{true, 1, 0, StatusWarning},
		{false, 0, 1, StatusError},
		{true, 0, 1, StatusError},
		{false, 1, 1, StatusError},
	}
	for _, tt := range tests {
		got := StatusFromCounts(tt.changed, tt.warnings, tt.errors)
		if got != tt.want {
			t.Errorf("StatusFromCounts(changed=%v, warnings=%d, errors=%d) = %q, want %q",
				tt.changed, tt.warnings, tt.errors, got, tt.want)
		}
	}
}

func TestRequestShouldFail(t *testing.T) {
	tests := []struct {
		name   string
		req    Request
		report ActionReport
		want   bool
	}{
		{"clean no fail", Request{FailOnError: true, FailOnWarning: false}, ActionReport{Status: StatusClean}, false},
		{"error fail", Request{FailOnError: true}, ActionReport{Errors: []string{"err"}}, true},
		{"warning no fail", Request{FailOnWarning: false}, ActionReport{Warnings: []string{"warn"}}, false},
		{"warning fail", Request{FailOnWarning: true}, ActionReport{Warnings: []string{"warn"}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.req.ShouldFail(&tt.report)
			if got != tt.want {
				t.Errorf("ShouldFail() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRequestShouldComment(t *testing.T) {
	tests := []struct {
		name   string
		req    Request
		report ActionReport
		want   bool
	}{
		{"off", Request{Comment: false}, ActionReport{Changed: true}, false},
		{"changes with changes", Request{Comment: true, CommentMode: "changes"}, ActionReport{Changed: true}, true},
		{"changes clean", Request{Comment: true, CommentMode: "changes"}, ActionReport{Changed: false}, false},
		{"always", Request{Comment: true, CommentMode: "always"}, ActionReport{Changed: false}, true},
		{"failure with error", Request{Comment: true, CommentMode: "failure"}, ActionReport{Errors: []string{"err"}}, true},
		{"failure clean", Request{Comment: true, CommentMode: "failure"}, ActionReport{Changed: false}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.req.ShouldComment(&tt.report)
			if got != tt.want {
				t.Errorf("ShouldComment() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRequestShouldDeleteComment(t *testing.T) {
	tests := []struct {
		name   string
		req    Request
		report ActionReport
		want   bool
	}{
		{"off", Request{Comment: false, CommentMode: "changes"}, ActionReport{Changed: false}, false},
		{"changes clean", Request{Comment: true, CommentMode: "changes"}, ActionReport{Changed: false}, true},
		{"changes with changes", Request{Comment: true, CommentMode: "changes"}, ActionReport{Changed: true}, false},
		{"always clean", Request{Comment: true, CommentMode: "always"}, ActionReport{Changed: false}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.req.ShouldDeleteComment(&tt.report)
			if got != tt.want {
				t.Errorf("ShouldDeleteComment() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTruncateDiff(t *testing.T) {
	full := "line1\nline2\nline3\nline4\nline5"

	preview, truncated := TruncateDiff(full, 1000, 0)
	if truncated || preview != full {
		t.Errorf("no limits: got truncated=%v preview=%q", truncated, preview)
	}

	preview, truncated = TruncateDiff(full, 0, 3)
	if !truncated {
		t.Fatalf("expected truncation")
	}
	if !contains(preview, "line3") {
		t.Errorf("line truncation should include line3, got %q", preview)
	}
	if contains(preview, "line4") {
		t.Errorf("line truncation should exclude line4, got %q", preview)
	}

	preview, truncated = TruncateDiff(full, 10, 0)
	if !truncated {
		t.Fatalf("expected byte truncation")
	}
	if len(preview) > 200 {
		t.Errorf("byte truncation unexpectedly long: %d bytes", len(preview))
	}
}

func TestRenderSummaryMarkdownSeparatesClassificationProvenance(t *testing.T) {
	got := RenderSummaryMarkdown(&Request{}, &ActionReport{
		Status:         StatusChanged,
		Changed:        true,
		ResourcesTotal: 2,
		Classifications: []policy.Classification{
			{ID: "image_update", Provenance: "policy"},
			{ID: "image_update", Provenance: "ai"},
		},
	})
	if !strings.Contains(got, "`image_update` [policy] (1)") {
		t.Fatalf("expected policy classification count, got:\n%s", got)
	}
	if !strings.Contains(got, "`image_update` [ai] (1)") {
		t.Fatalf("expected ai classification count, got:\n%s", got)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsAt(s, substr))
}

func containsAt(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
