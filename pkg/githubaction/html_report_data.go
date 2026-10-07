package githubaction

import (
	"encoding/json"
	"fmt"
	"html/template"
	"sort"
	"strings"
	"time"

	"github.com/tobiash/gitops-preview-toolkit/pkg/ai"
	fmpdiff "github.com/tobiash/gitops-preview-toolkit/pkg/diff"
	"github.com/tobiash/gitops-preview-toolkit/pkg/policy"
)

type HTMLReportData struct {
	Meta      HTMLReportMeta       `json:"meta"`
	Summary   HTMLReportSummary    `json:"summary"`
	Policies  HTMLReportPolicies   `json:"policies"`
	AI        *ai.Assessment       `json:"ai,omitempty"`
	Resources []HTMLResourceChange `json:"resources"`
}

type HTMLReportMeta struct {
	Status        string   `json:"status"`
	GeneratedAt   string   `json:"generatedAt"`
	Base          string   `json:"base"`
	Target        string   `json:"target"`
	Warnings      []string `json:"warnings,omitempty"`
	DiffBytes     int      `json:"diffBytes"`
	DiffTruncated bool     `json:"diffTruncated"`
}

type HTMLReportSummary struct {
	Added            int                        `json:"added"`
	Modified         int                        `json:"modified"`
	Deleted          int                        `json:"deleted"`
	Total            int                        `json:"total"`
	KindBreakdown    map[string]ChangeBreakdown `json:"kindBreakdown,omitempty"`
	ClusterBreakdown map[string]ChangeBreakdown `json:"clusterBreakdown,omitempty"`
}

type HTMLReportPolicies struct {
	Classifications []policy.Classification `json:"classifications,omitempty"`
	Violations      []policy.Violation      `json:"violations,omitempty"`
	Labels          []string                `json:"labels,omitempty"`
	PolicyFailures  []string                `json:"policyFailures,omitempty"`
	PolicyFailed    bool                    `json:"policyFailed"`
}

type HTMLResourceChange struct {
	Index        int           `json:"index"`
	ID           string        `json:"id"`
	Action       string        `json:"action"`
	Cluster      string        `json:"cluster"`
	APIVersion   string        `json:"apiVersion"`
	Kind         string        `json:"kind"`
	Namespace    string        `json:"namespace"`
	Name         string        `json:"name"`
	LogicalID    string        `json:"logicalId,omitempty"`
	Slot         string        `json:"slot,omitempty"`
	Producer     string        `json:"producer"`
	AddedLines   int           `json:"addedLines"`
	DeletedLines int           `json:"deletedLines"`
	DiffRows     []HTMLDiffRow `json:"diffRows"`
	Truncated    bool          `json:"truncated,omitempty"`
}

type HTMLDiffRow struct {
	Type    string `json:"type"`
	OldLine int    `json:"oldLine,omitempty"`
	NewLine int    `json:"newLine,omitempty"`
	OldText string `json:"oldText,omitempty"`
	NewText string `json:"newText,omitempty"`
}

func BuildHTMLReportData(req *Request, report *ActionReport, result *fmpdiff.DiffResult) HTMLReportData {
	data := HTMLReportData{
		Meta: HTMLReportMeta{
			Status:        report.Status,
			GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
			Base:          req.DiffLeft(),
			Target:        req.DiffRight(),
			Warnings:      report.Warnings,
			DiffBytes:     report.DiffBytes,
			DiffTruncated: report.DiffTruncated,
		},
		Summary: HTMLReportSummary{
			Added:            report.ResourcesAdded,
			Modified:         report.ResourcesModified,
			Deleted:          report.ResourcesDeleted,
			Total:            report.ResourcesTotal,
			KindBreakdown:    report.KindBreakdown,
			ClusterBreakdown: report.ByCluster,
		},
		Policies: HTMLReportPolicies{
			Classifications: report.Classifications,
			Violations:      report.Violations,
			Labels:          report.Labels,
			PolicyFailures:  report.PolicyFailures,
			PolicyFailed:    report.PolicyFailed,
		},
		AI: report.AIAssessment,
	}

	if result == nil {
		return data
	}

	resources := htmlResourceChanges(result.Changes(), req.HTMLReportMaxResourceDiffBytes)
	sort.Slice(resources, func(i, j int) bool {
		return resourceSortKey(resources[i]) < resourceSortKey(resources[j])
	})
	for i := range resources {
		resources[i].Index = i
	}
	data.Resources = resources
	return data
}

func htmlResourceChanges(changes []fmpdiff.ResourceChange, maxDiffBytes int) []HTMLResourceChange {
	out := make([]HTMLResourceChange, 0, len(changes))
	for _, change := range changes {
		unified := change.UnifiedDiff()
		truncated := false
		if maxDiffBytes > 0 && len(unified) > maxDiffBytes {
			unified = unified[:maxDiffBytes]
			truncated = true
		}
		rows := parseUnifiedDiffRows(unified)
		added, deleted := countChangedRows(rows)
		apiVersion := gvkAPIVersion(change.ID.Group, change.ID.Version)
		out = append(out, HTMLResourceChange{
			ID:           resourceIdentity(change),
			Action:       change.Action,
			Cluster:      change.Cluster,
			APIVersion:   apiVersion,
			Kind:         change.Kind,
			Namespace:    change.Namespace,
			Name:         change.Name,
			LogicalID:    change.LogicalID,
			Slot:         compositionSlot(change),
			Producer:     change.Producer,
			AddedLines:   added,
			DeletedLines: deleted,
			DiffRows:     rows,
			Truncated:    truncated,
		})
	}
	return out
}

func resourceSortKey(r HTMLResourceChange) string {
	return strings.Join([]string{r.Cluster, r.Producer, r.Kind, r.Namespace, r.Name, r.LogicalID, r.Action}, "\x00")
}

func resourceIdentity(change fmpdiff.ResourceChange) string {
	if change.LogicalID != "" {
		return strings.Join([]string{change.Cluster, "logical", change.LogicalID}, "|")
	}
	return strings.Join([]string{
		change.Cluster, change.Producer, gvkAPIVersion(change.ID.Group, change.ID.Version),
		change.Kind, change.Namespace, change.Name,
	}, "|")
}

func compositionSlot(change fmpdiff.ResourceChange) string {
	if change.LogicalID == "" || change.Name != "" {
		return ""
	}
	for _, object := range []map[string]any{change.New, change.Old} {
		metadata, _ := object["metadata"].(map[string]any)
		annotations, _ := metadata["annotations"].(map[string]any)
		if slot, _ := annotations["crossplane.io/composition-resource-name"].(string); slot != "" {
			return slot
		}
	}
	return ""
}

func parseUnifiedDiffRows(unified string) []HTMLDiffRow {
	lines := strings.Split(strings.ReplaceAll(unified, "\r\n", "\n"), "\n")
	rows := make([]HTMLDiffRow, 0, len(lines))
	oldLine, newLine := 0, 0
	for _, line := range lines {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "+++") {
			continue
		}
		if strings.HasPrefix(line, "@@") {
			oldLine, newLine = parseHunkLineNumbers(line)
			rows = append(rows, HTMLDiffRow{Type: "hunk", OldText: line, NewText: line})
			continue
		}
		switch {
		case strings.HasPrefix(line, "+"):
			rows = append(rows, HTMLDiffRow{Type: "added", NewLine: newLine, NewText: strings.TrimPrefix(line, "+")})
			newLine++
		case strings.HasPrefix(line, "-"):
			rows = append(rows, HTMLDiffRow{Type: "deleted", OldLine: oldLine, OldText: strings.TrimPrefix(line, "-")})
			oldLine++
		default:
			text := strings.TrimPrefix(line, " ")
			rows = append(rows, HTMLDiffRow{Type: "context", OldLine: oldLine, NewLine: newLine, OldText: text, NewText: text})
			oldLine++
			newLine++
		}
	}
	return rows
}

func parseHunkLineNumbers(line string) (int, int) {
	var oldStart, newStart int
	_, _ = fmt.Sscanf(line, "@@ -%d", &oldStart)
	if idx := strings.Index(line, " +"); idx >= 0 {
		_, _ = fmt.Sscanf(line[idx+1:], "+%d", &newStart)
	}
	return oldStart, newStart
}

func countChangedRows(rows []HTMLDiffRow) (int, int) {
	var added, deleted int
	for _, row := range rows {
		switch row.Type {
		case "added":
			added++
		case "deleted":
			deleted++
		}
	}
	return added, deleted
}

func gvkAPIVersion(group, version string) string {
	if group == "" {
		return version
	}
	return group + "/" + version
}

func reportDataJSON(data HTMLReportData) (template.JS, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return "", err
	}
	s := string(b)
	s = strings.ReplaceAll(s, "</script", "<\\/script")
	s = strings.ReplaceAll(s, "\u2028", "\\u2028")
	s = strings.ReplaceAll(s, "\u2029", "\\u2029")
	return template.JS(s), nil //nolint:gosec // JSON is marshaled and script terminators are escaped above.
}
