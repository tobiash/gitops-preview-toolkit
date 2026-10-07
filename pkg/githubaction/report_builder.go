package githubaction

import (
	"github.com/tobiash/gitops-preview-toolkit/pkg/ai"
	"github.com/tobiash/gitops-preview-toolkit/pkg/diff"
	"github.com/tobiash/gitops-preview-toolkit/pkg/policy"
)

// ReportInput contains the domain data needed to assemble an impact report.
type ReportInput struct {
	Result             *diff.DiffResult
	PolicyResult       *policy.Result
	Warnings           []string
	FullDiff           string
	MaxInlineDiffBytes int
	DiffPreviewLines   int
	AIAssessment       *ai.Assessment
}

// BuildReport assembles the shared impact report used by CLI and GitHub Action adapters.
func BuildReport(input ReportInput) *ActionReport {
	result := input.Result
	if result == nil {
		return &ActionReport{Status: StatusError}
	}
	summary := result.Summary()

	preview, truncated := TruncateDiff(input.FullDiff, input.MaxInlineDiffBytes, input.DiffPreviewLines)
	report := &ActionReport{
		Status:            StatusFromCounts(summary.Total > 0, len(input.Warnings), 0),
		Changed:           summary.Total > 0,
		Warnings:          input.Warnings,
		DiffBytes:         len(input.FullDiff),
		DiffTruncated:     truncated,
		DiffPreview:       preview,
		ResourcesAdded:    len(result.Added),
		ResourcesModified: len(result.Modified),
		ResourcesDeleted:  len(result.Deleted),
		ResourcesTotal:    summary.Total,
		ByKind:            summary.ByKind,
		KindBreakdown:     summary.KindBreakdown,
		ByCluster:         summary.ClusterBreakdown,
		AIAssessment:      input.AIAssessment,
	}
	applyPolicyResult(report, input.PolicyResult)
	return report
}

func applyPolicyResult(report *ActionReport, result *policy.Result) {
	if result == nil {
		return
	}
	report.Classifications = result.Classifications
	report.Violations = result.Violations
	report.Labels = result.Labels
	report.PolicyFailures = result.PolicyFailures
	report.PolicyFailed = result.PolicyFailed
	if report.PolicyFailed {
		report.Status = StatusError
	}
}
