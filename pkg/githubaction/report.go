package githubaction

import (
	"strings"

	"github.com/tobiash/gitops-preview-toolkit/pkg/ai"
	"github.com/tobiash/gitops-preview-toolkit/pkg/diff"
	"github.com/tobiash/gitops-preview-toolkit/pkg/policy"
)

// Status values for ActionReport.
const (
	StatusClean   = "clean"
	StatusChanged = "changed"
	StatusWarning = "warning"
	StatusError   = "error"
)

// ActionReport is the structured result of a GitHub Action run.
type ActionReport struct {
	Status   string   `json:"status"`
	Changed  bool     `json:"changed"`
	Warnings []string `json:"warnings,omitempty"`
	Errors   []string `json:"errors,omitempty"`

	DiffBytes     int    `json:"diff_bytes"`
	DiffTruncated bool   `json:"diff_truncated"`
	DiffPreview   string `json:"diff_preview,omitempty"`

	DiffFile       string `json:"diff_file,omitempty"`
	SummaryFile    string `json:"summary_file,omitempty"`
	CommentFile    string `json:"comment_file,omitempty"`
	ReportFile     string `json:"report_file,omitempty"`
	HTMLReportFile string `json:"html_report_file,omitempty"`
	ExportDir      string `json:"export_dir,omitempty"`

	ResourcesAdded    int `json:"resources_added"`
	ResourcesModified int `json:"resources_modified"`
	ResourcesDeleted  int `json:"resources_deleted"`
	ResourcesTotal    int `json:"resources_total"`

	ByKind        map[string]int                  `json:"by_kind,omitempty"`
	KindBreakdown map[string]diff.ChangeBreakdown `json:"kind_breakdown,omitempty"`
	ByProducer    map[string]int                  `json:"by_producer,omitempty"`
	ByCluster     map[string]diff.ChangeBreakdown `json:"by_cluster,omitempty"`

	Classifications []policy.Classification `json:"classifications,omitempty"`
	Violations      []policy.Violation      `json:"violations,omitempty"`
	Labels          []string                `json:"labels,omitempty"`
	PolicyFailures  []string                `json:"policy_failures,omitempty"`
	PolicyFailed    bool                    `json:"policy_failed"`
	AIAssessment    *ai.Assessment          `json:"ai_assessment,omitempty"`
}

// ChangeBreakdown is an alias for diff.ChangeBreakdown for backward compatibility.
type ChangeBreakdown = diff.ChangeBreakdown

// StatusFromCounts derives the overall status from changes, warnings, and errors.
func StatusFromCounts(changed bool, warnings int, errors int) string {
	if errors > 0 {
		return StatusError
	}
	if warnings > 0 {
		return StatusWarning
	}
	if changed {
		return StatusChanged
	}
	return StatusClean
}

// ShouldFail returns true if the request says we should fail for the current report state.
func (r *Request) ShouldFail(report *ActionReport) bool {
	if r.FailOnError && len(report.Errors) > 0 {
		return true
	}
	if r.FailOnWarning && len(report.Warnings) > 0 {
		return true
	}
	return false
}

// ShouldComment returns true if a comment should be posted/updated for this report.
func (r *Request) ShouldComment(report *ActionReport) bool {
	if !r.Comment {
		return false
	}
	switch r.CommentMode {
	case "changes":
		return report.Changed || len(report.Warnings) > 0 || len(report.Errors) > 0
	case "always":
		return true
	case "failure":
		return len(report.Errors) > 0
	default:
		return report.Changed || len(report.Warnings) > 0 || len(report.Errors) > 0
	}
}

// ShouldDeleteComment returns true when an existing comment should be deleted (no longer relevant).
func (r *Request) ShouldDeleteComment(report *ActionReport) bool {
	if !r.Comment {
		return false
	}
	// Only delete if mode is "changes" and there's nothing to report
	if r.CommentMode != "changes" {
		return false
	}
	return !report.Changed && len(report.Warnings) == 0 && len(report.Errors) == 0
}

// TruncateDiff produces a preview-safe diff string respecting size and line limits.
func TruncateDiff(full string, maxBytes, maxLines int) (preview string, truncated bool) {
	if maxBytes <= 0 && maxLines <= 0 {
		return full, false
	}

	lines := strings.Split(full, "\n")
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}

	preview = strings.Join(lines, "\n")
	if maxBytes > 0 && len(preview) > maxBytes {
		preview = preview[:maxBytes]
		// Trim to last newline to avoid cutting a line in half
		if idx := strings.LastIndex(preview, "\n"); idx > 0 {
			preview = preview[:idx]
		}
		truncated = true
	}

	if truncated {
		preview += "\n\n*... (diff truncated — full diff available in artifact or output)*"
	}
	return preview, truncated
}

// StringSlice returns a stable string slice for use in markdown tables.
func StringSlice(v []string) []string {
	if v == nil {
		return nil
	}
	out := make([]string, len(v))
	copy(out, v)
	return out
}
