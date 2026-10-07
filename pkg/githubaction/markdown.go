package githubaction

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tobiash/gitops-preview-toolkit/pkg/policy"
)

// RenderSummaryMarkdown generates a GitHub Step Summary markdown document.
func RenderSummaryMarkdown(req *Request, report *ActionReport) string {
	var b strings.Builder

	b.WriteString("## 🔄 gitops-preview-toolkit\n\n")

	statusEmoji := map[string]string{
		StatusClean:   "✅",
		StatusChanged: "📝",
		StatusWarning: "⚠️",
		StatusError:   "❌",
	}
	emoji := statusEmoji[report.Status]
	if emoji == "" {
		emoji = "❓"
	}
	_, _ = fmt.Fprintf(&b, "**Status:** %s %s\n\n", emoji, strings.ToUpper(report.Status))
	b.WriteString(renderChangeSummary(report))
	b.WriteString("\n")
	writeKindBreakdown(&b, report)
	writeClusterBreakdown(&b, report)
	writePolicySections(&b, report)
	writeAIAssessment(&b, report)

	if len(report.Warnings) > 0 {
		b.WriteString("### ⚠️ Warnings\n\n")
		for _, w := range report.Warnings {
			_, _ = fmt.Fprintf(&b, "- %s\n", escapeMarkdown(w))
		}
		b.WriteString("\n")
	}

	if len(report.Errors) > 0 {
		b.WriteString("### ❌ Errors\n\n")
		for _, e := range report.Errors {
			_, _ = fmt.Fprintf(&b, "- %s\n", escapeMarkdown(e))
		}
		b.WriteString("\n")
	}

	if report.DiffPreview != "" {
		b.WriteString("### Diff Preview\n\n")
		b.WriteString("<details>\n<summary>Click to expand</summary>\n\n")
		b.WriteString("```diff\n")
		b.WriteString(report.DiffPreview)
		b.WriteString("\n```\n\n")
		b.WriteString("</details>\n\n")
	}

	if report.ExportDir != "" {
		_, _ = fmt.Fprintf(&b, "### 📦 Export\n\nRendered manifests exported to `%s`.\n\n", report.ExportDir)
	}

	return b.String()
}

// RenderCommentMarkdown generates a PR comment markdown document.
func RenderCommentMarkdown(req *Request, report *ActionReport) string {
	var b strings.Builder

	statusEmoji := map[string]string{
		StatusClean:   "✅",
		StatusChanged: "📝",
		StatusWarning: "⚠️",
		StatusError:   "❌",
	}
	emoji := statusEmoji[report.Status]
	if emoji == "" {
		emoji = "❓"
	}

	_, _ = fmt.Fprintf(&b, "### %s gitops-preview-toolkit\n\n", emoji)

	if len(report.Errors) > 0 {
		b.WriteString("**Errors detected.**\n\n")
	} else if len(report.Warnings) > 0 {
		b.WriteString("**Completed with warnings.**\n\n")
	} else if report.Changed {
		b.WriteString("**Manifest changes detected.**\n\n")
	} else {
		b.WriteString("**No manifest changes detected.**\n\n")
	}

	b.WriteString(renderChangeSummary(report))
	b.WriteString("\n")
	writeKindBreakdown(&b, report)
	writeClusterBreakdown(&b, report)
	writePolicySections(&b, report)
	writeAIAssessment(&b, report)

	if len(report.Warnings) > 0 {
		b.WriteString("**Warnings:**\n")
		for _, w := range report.Warnings {
			_, _ = fmt.Fprintf(&b, "- %s\n", escapeMarkdown(w))
		}
		b.WriteString("\n")
	}

	if len(report.Errors) > 0 {
		b.WriteString("**Errors:**\n")
		for _, e := range report.Errors {
			_, _ = fmt.Fprintf(&b, "- %s\n", escapeMarkdown(e))
		}
		b.WriteString("\n")
	}

	if report.DiffPreview != "" {
		b.WriteString("<details>\n<summary>Diff Preview</summary>\n\n")
		b.WriteString("```diff\n")
		b.WriteString(report.DiffPreview)
		b.WriteString("\n```\n\n")
		b.WriteString("</details>\n\n")
	} else if report.DiffTruncated {
		b.WriteString("*Diff too large to display inline. Full diff available in workflow artifacts or outputs.*\n\n")
	}

	if report.ExportDir != "" {
		_, _ = fmt.Fprintf(&b, "📦 Exported manifests: `%s`\n\n", report.ExportDir)
	}

	b.WriteString("<!-- fmp-comment-marker -->\n")

	return b.String()
}

func escapeMarkdown(s string) string {
	// Minimal escaping for markdown inline use
	s = strings.ReplaceAll(s, "|", "\\|")
	return s
}

func renderChangeSummary(report *ActionReport) string {
	if report.ResourcesTotal == 0 {
		return "✅ **No resource changes.**\n\n"
	}

	return fmt.Sprintf(
		"🟢 **%d** to add, 🟡 **%d** to change, 🔴 **%d** to destroy.\n\n",
		report.ResourcesAdded,
		report.ResourcesModified,
		report.ResourcesDeleted,
	)
}

func writeKindBreakdown(b *strings.Builder, report *ActionReport) {
	rows := sortedKindBreakdown(report.KindBreakdown)
	if len(rows) == 0 {
		return
	}

	_, _ = fmt.Fprintf(b, "<details>\n<summary>Changed resources by kind (%d kinds)</summary>\n\n", len(rows))
	b.WriteString("| Kind | Added | Modified | Deleted | Total |\n")
	b.WriteString("| :--- | ---: | ---: | ---: | ---: |\n")
	for _, row := range rows {
		_, _ = fmt.Fprintf(
			b,
			"| %s | %d | %d | %d | %d |\n",
			row.Kind,
			row.Breakdown.Added,
			row.Breakdown.Modified,
			row.Breakdown.Deleted,
			row.Breakdown.Total,
		)
	}
	b.WriteString("\n</details>\n\n")
}

func writeClusterBreakdown(b *strings.Builder, report *ActionReport) {
	rows := sortedClusterBreakdown(report.ByCluster)
	if len(rows) == 0 {
		return
	}

	_, _ = fmt.Fprintf(b, "<details>\n<summary>Changed resources by cluster (%d clusters)</summary>\n\n", len(rows))
	b.WriteString("| Cluster | Added | Modified | Deleted | Total |\n")
	b.WriteString("| :--- | ---: | ---: | ---: | ---: |\n")
	for _, row := range rows {
		cluster := row.Cluster
		if cluster == "" {
			cluster = "(default)"
		}
		_, _ = fmt.Fprintf(
			b,
			"| %s | %d | %d | %d | %d |\n",
			cluster,
			row.Breakdown.Added,
			row.Breakdown.Modified,
			row.Breakdown.Deleted,
			row.Breakdown.Total,
		)
	}
	b.WriteString("\n</details>\n\n")
}

func writePolicySections(b *strings.Builder, report *ActionReport) {
	if len(report.Classifications) > 0 {
		b.WriteString("**Classifications:**\n")
		for _, item := range summarizeClassifications(report.Classifications) {
			_, _ = fmt.Fprintf(b, "- `%s` [%s] (%d)\n", item.id, item.provenance, item.count)
		}
		b.WriteString("\n")
	}

	if len(report.Violations) > 0 {
		b.WriteString("**Violations:**\n")
		for _, violation := range report.Violations {
			message := violation.Message
			if message == "" {
				message = violation.ID
			}
			_, _ = fmt.Fprintf(b, "- `%s`: %s\n", violation.ID, escapeMarkdown(message))
		}
		b.WriteString("\n")
	}

	if len(report.Labels) > 0 {
		b.WriteString("**Suggested labels:**\n")
		for _, label := range report.Labels {
			_, _ = fmt.Fprintf(b, "- `%s`\n", escapeMarkdown(label))
		}
		b.WriteString("\n")
	}

	if report.PolicyFailed {
		b.WriteString("**Policy enforcement failed.**\n")
		for _, id := range report.PolicyFailures {
			_, _ = fmt.Fprintf(b, "- `%s` matched `fail-on`\n", id)
		}
		b.WriteString("\n")
	}
}

func writeAIAssessment(b *strings.Builder, report *ActionReport) {
	if report.AIAssessment == nil {
		return
	}
	b.WriteString("### AI Assessment\n\n")
	_, _ = fmt.Fprintf(b, "Generated by `%s`", escapeMarkdown(report.AIAssessment.Provider))
	if report.AIAssessment.Model != "" {
		_, _ = fmt.Fprintf(b, " using `%s`", escapeMarkdown(report.AIAssessment.Model))
	}
	b.WriteString(". Review generated content before acting on it.\n\n")
	if report.AIAssessment.Summary != "" {
		b.WriteString(report.AIAssessment.Summary)
		b.WriteString("\n\n")
	}
	if report.AIAssessment.Truncated {
		b.WriteString("_AI input was truncated; assessment may not cover every diff detail._\n\n")
	}
	if len(report.AIAssessment.Warnings) > 0 {
		b.WriteString("**AI assessment warnings:**\n")
		for _, warning := range report.AIAssessment.Warnings {
			_, _ = fmt.Fprintf(b, "- %s\n", escapeMarkdown(warning))
		}
		b.WriteString("\n")
	}
}

type kindBreakdownRow struct {
	Kind      string
	Breakdown ChangeBreakdown
}

type summarizedClassification struct {
	id         string
	provenance string
	count      int
}

func sortedKindBreakdown(m map[string]ChangeBreakdown) []kindBreakdownRow {
	if len(m) == 0 {
		return nil
	}

	rows := make([]kindBreakdownRow, 0, len(m))
	for kind, breakdown := range m {
		rows = append(rows, kindBreakdownRow{Kind: kind, Breakdown: breakdown})
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Breakdown.Total == rows[j].Breakdown.Total {
			return rows[i].Kind < rows[j].Kind
		}
		return rows[i].Breakdown.Total > rows[j].Breakdown.Total
	})

	return rows
}

type clusterBreakdownRow struct {
	Cluster   string
	Breakdown ChangeBreakdown
}

func sortedClusterBreakdown(m map[string]ChangeBreakdown) []clusterBreakdownRow {
	if len(m) == 0 {
		return nil
	}

	rows := make([]clusterBreakdownRow, 0, len(m))
	for cluster, breakdown := range m {
		rows = append(rows, clusterBreakdownRow{Cluster: cluster, Breakdown: breakdown})
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Breakdown.Total == rows[j].Breakdown.Total {
			return rows[i].Cluster < rows[j].Cluster
		}
		return rows[i].Breakdown.Total > rows[j].Breakdown.Total
	})

	return rows
}

func summarizeClassifications(items []policy.Classification) []summarizedClassification {
	if len(items) == 0 {
		return nil
	}
	counts := make(map[string]int)
	for _, item := range items {
		provenance := item.Provenance
		if provenance == "" {
			provenance = "policy"
		}
		counts[item.ID+"\x00"+provenance]++
	}
	rows := make([]summarizedClassification, 0, len(counts))
	for key, count := range counts {
		parts := strings.SplitN(key, "\x00", 2)
		rows = append(rows, summarizedClassification{id: parts[0], provenance: parts[1], count: count})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].count == rows[j].count {
			if rows[i].id == rows[j].id {
				return rows[i].provenance < rows[j].provenance
			}
			return rows[i].id < rows[j].id
		}
		return rows[i].count > rows[j].count
	})
	return rows
}
