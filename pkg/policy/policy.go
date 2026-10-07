package policy

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/open-policy-agent/opa/v1/rego"

	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/diff"
)

//go:embed builtin.rego
var builtinModule string

type Classification struct {
	ID         string `json:"id"`
	Title      string `json:"title,omitempty"`
	Severity   string `json:"severity,omitempty"`
	Priority   int    `json:"priority,omitempty"`
	Message    string `json:"message,omitempty"`
	Provenance string `json:"provenance"`
	Cluster    string `json:"cluster,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name,omitempty"`
	LogicalID  string `json:"logicalId,omitempty"`
}

type Violation struct {
	ID        string `json:"id"`
	Severity  string `json:"severity,omitempty"`
	Priority  int    `json:"priority,omitempty"`
	Message   string `json:"message,omitempty"`
	Cluster   string `json:"cluster,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	LogicalID string `json:"logicalId,omitempty"`
}

type Result struct {
	Classifications []Classification `json:"classifications,omitempty"`
	Violations      []Violation      `json:"violations,omitempty"`
	Labels          []string         `json:"labels,omitempty"`
	PolicyFailures  []string         `json:"policy_failures,omitempty"`
	PolicyFailed    bool             `json:"policy_failed"`
}

type inputDocument struct {
	Builtins []string     `json:"builtins,omitempty"`
	Summary  summary      `json:"summary"`
	Changes  []diffChange `json:"changes"`
}

type summary struct {
	Added    int `json:"added"`
	Modified int `json:"modified"`
	Deleted  int `json:"deleted"`
	Total    int `json:"total"`
}

type diffChange struct {
	Action    string         `json:"action"`
	Cluster   string         `json:"cluster,omitempty"`
	Kind      string         `json:"kind"`
	Name      string         `json:"name"`
	Namespace string         `json:"namespace"`
	Producer  string         `json:"producer,omitempty"`
	LogicalID string         `json:"logicalId,omitempty"`
	Old       map[string]any `json:"old,omitempty"`
	New       map[string]any `json:"new,omitempty"`
}

type packageDocument struct {
	Classifications []Classification `json:"classifications"`
	Violations      []Violation      `json:"violations"`
	Labels          []string         `json:"labels"`
}

func Evaluate(ctx context.Context, result *diff.DiffResult, cfg *config.PolicyConfig, baseDir string) (*Result, error) {
	if cfg == nil || !hasPolicyConfig(cfg) {
		return &Result{}, nil
	}

	moduleOpts, err := moduleOptions(cfg, baseDir)
	if err != nil {
		return nil, err
	}

	query, err := rego.New(append([]func(*rego.Rego){rego.Query("data.fmp")}, moduleOpts...)...).PrepareForEval(ctx)
	if err != nil {
		return nil, fmt.Errorf("preparing policy evaluation: %w", err)
	}

	input := inputDocument{
		Builtins: cfg.Builtin,
		Summary: summary{
			Added:    len(result.Added),
			Modified: len(result.Modified),
			Deleted:  len(result.Deleted),
			Total:    result.TotalChanged(),
		},
		Changes: toInputChanges(result),
	}

	results, err := query.Eval(ctx, rego.EvalInput(input))
	if err != nil {
		return nil, fmt.Errorf("evaluating policies: %w", err)
	}

	out := &Result{}
	if len(results) > 0 && len(results[0].Expressions) > 0 {
		var doc packageDocument
		if err := decodeValue(results[0].Expressions[0].Value, &doc); err != nil {
			return nil, fmt.Errorf("decoding policy results: %w", err)
		}
		out.Classifications = normalizeClassifications(doc.Classifications)
		out.Violations = normalizeViolations(doc.Violations)
		out.Labels = normalizeLabels(doc.Labels)
	}

	ApplyEffects(out, cfg)

	return out, nil
}

func ApplyEffects(result *Result, cfg *config.PolicyConfig) {
	if result == nil || cfg == nil {
		return
	}
	result.Classifications = normalizeClassifications(result.Classifications)
	result.Violations = normalizeViolations(result.Violations)
	result.Labels = normalizeLabels(append(result.Labels, labelsFromMappings(cfg, result.Classifications, result.Violations)...))
	result.PolicyFailures = matchedFailures(cfg.FailOn, result.Classifications, result.Violations)
	result.PolicyFailed = len(result.PolicyFailures) > 0
}

func hasPolicyConfig(cfg *config.PolicyConfig) bool {
	return len(cfg.Builtin) > 0 || len(cfg.Modules) > 0 || len(cfg.Inline) > 0 || len(cfg.FailOn) > 0 || len(cfg.Labels) > 0
}

func moduleOptions(cfg *config.PolicyConfig, baseDir string) ([]func(*rego.Rego), error) {
	var opts []func(*rego.Rego)
	if len(cfg.Builtin) > 0 {
		opts = append(opts, rego.Module("builtin.rego", builtinModule))
	}

	for i, module := range cfg.Inline {
		opts = append(opts, rego.Module(fmt.Sprintf("inline-%d.rego", i+1), module))
	}

	for _, pattern := range cfg.Modules {
		matches, err := expandPattern(pattern, baseDir)
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			data, err := os.ReadFile(match)
			if err != nil {
				return nil, fmt.Errorf("reading policy module %s: %w", match, err)
			}
			opts = append(opts, rego.Module(match, string(data)))
		}
	}

	return opts, nil
}

func expandPattern(pattern, baseDir string) ([]string, error) {
	full := pattern
	if !filepath.IsAbs(full) {
		full = filepath.Join(baseDir, full)
	}
	matches, err := filepath.Glob(full)
	if err != nil {
		return nil, fmt.Errorf("expanding policy module pattern %s: %w", pattern, err)
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("policy module pattern %s matched no files", pattern)
	}
	sort.Strings(matches)
	return matches, nil
}

func toInputChanges(result *diff.DiffResult) []diffChange {
	out := make([]diffChange, 0, result.TotalChanged())
	appendChanges := func(changes []diff.ResourceChange) {
		for _, change := range changes {
			out = append(out, diffChange{
				Action:    change.Action,
				Cluster:   change.Cluster,
				Kind:      change.Kind,
				Name:      change.Name,
				Namespace: change.Namespace,
				Producer:  change.Producer,
				LogicalID: change.LogicalID,
				Old:       change.Old,
				New:       change.New,
			})
		}
	}
	appendChanges(result.Added)
	appendChanges(result.Modified)
	appendChanges(result.Deleted)
	return out
}

func decodeValue(value any, target any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func normalizeClassifications(items []Classification) []Classification {
	if len(items) == 0 {
		return nil
	}
	for i := range items {
		if items[i].Provenance == "" {
			items[i].Provenance = "policy"
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Cluster != items[j].Cluster {
			return items[i].Cluster < items[j].Cluster
		}
		if items[i].ID != items[j].ID {
			return items[i].ID < items[j].ID
		}
		if items[i].Provenance != items[j].Provenance {
			return items[i].Provenance < items[j].Provenance
		}
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		if items[i].Namespace != items[j].Namespace {
			return items[i].Namespace < items[j].Namespace
		}
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		return items[i].LogicalID < items[j].LogicalID
	})
	return items
}

func normalizeViolations(items []Violation) []Violation {
	if len(items) == 0 {
		return nil
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Cluster != items[j].Cluster {
			return items[i].Cluster < items[j].Cluster
		}
		if items[i].ID != items[j].ID {
			return items[i].ID < items[j].ID
		}
		if items[i].Kind != items[j].Kind {
			return items[i].Kind < items[j].Kind
		}
		if items[i].Namespace != items[j].Namespace {
			return items[i].Namespace < items[j].Namespace
		}
		if items[i].Name != items[j].Name {
			return items[i].Name < items[j].Name
		}
		return items[i].LogicalID < items[j].LogicalID
	})
	return items
}

func normalizeLabels(labels []string) []string {
	if len(labels) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(labels))
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

func labelsFromMappings(cfg *config.PolicyConfig, classifications []Classification, violations []Violation) []string {
	if len(cfg.Labels) == 0 {
		return nil
	}
	var out []string
	for _, classification := range classifications {
		for _, label := range cfg.Labels[classification.ID] {
			out = append(out, label)
		}
	}
	for _, violation := range violations {
		for _, label := range cfg.Labels[violation.ID] {
			out = append(out, label)
		}
	}
	return out
}

func matchedFailures(failOn []string, classifications []Classification, violations []Violation) []string {
	if len(failOn) == 0 {
		return nil
	}
	ids := make(map[string]bool, len(failOn))
	for _, id := range failOn {
		ids[id] = true
	}
	seen := make(map[string]bool)
	var out []string
	for _, classification := range classifications {
		if ids[classification.ID] && !seen[classification.ID] {
			seen[classification.ID] = true
			out = append(out, classification.ID)
		}
	}
	for _, violation := range violations {
		if ids[violation.ID] && !seen[violation.ID] {
			seen[violation.ID] = true
			out = append(out, violation.ID)
		}
	}
	sort.Strings(out)
	return out
}
