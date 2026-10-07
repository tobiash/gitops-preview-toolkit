package preview

import (
	"context"
	"fmt"
	"io"

	"github.com/tobiash/gitops-preview-toolkit/pkg/diff"
)

type normalizationDiscovery struct {
	preview *Preview
	path    string
}

type NormalizationFinding struct {
	Cluster string
	Diff    diff.FieldDiff
}

type NormalizationDiagnosis struct {
	Findings []NormalizationFinding
}

func (d NormalizationDiagnosis) FieldDiffs() []diff.FieldDiff {
	diffs := make([]diff.FieldDiff, 0, len(d.Findings))
	for _, finding := range d.Findings {
		diffs = append(diffs, finding.Diff)
	}
	return diffs
}

func (d normalizationDiscovery) WritePermadiffConfig(ctx context.Context, out io.Writer) error {
	diagnosis, err := d.Diagnose(ctx)
	if err != nil {
		return err
	}

	config, err := diff.GenerateFilterConfig(diagnosis.FieldDiffs())
	if err != nil {
		return err
	}
	_, err = out.Write(config)
	return err
}

func (d normalizationDiscovery) Detect(ctx context.Context) ([]diff.FieldDiff, error) {
	diagnosis, err := d.Diagnose(ctx)
	if err != nil {
		return nil, err
	}
	return diagnosis.FieldDiffs(), nil
}

func (d normalizationDiscovery) Diagnose(ctx context.Context) (*NormalizationDiagnosis, error) {
	left, right, err := d.renderTwice(ctx)
	if err != nil {
		return nil, err
	}

	diagnosis := &NormalizationDiagnosis{}
	clusters := sortedClusterNames(left)
	for _, cluster := range clusters {
		diffs, err := diff.DetectPermadiffs(left[cluster].render, right[cluster].render)
		if err != nil {
			if d.preview.isClustered() {
				return nil, fmt.Errorf("cluster %q: detecting permadiffs: %w", cluster, err)
			}
			return nil, fmt.Errorf("detecting permadiffs: %w", err)
		}
		logicalDiffs, err := diff.DetectLogicalPermadiffs(left[cluster].logical, right[cluster].logical)
		if err != nil {
			return nil, fmt.Errorf("cluster %q: detecting logical permadiffs: %w", cluster, err)
		}
		diffs = append(diffs, logicalDiffs...)
		for _, fieldDiff := range diffs {
			diagnosis.Findings = append(diagnosis.Findings, NormalizationFinding{
				Cluster: cluster,
				Diff:    fieldDiff,
			})
		}
	}
	return diagnosis, nil
}

func (d normalizationDiscovery) renderTwice(ctx context.Context) (map[string]*loadRepoResult, map[string]*loadRepoResult, error) {
	defer d.preview.beginRun()()
	left, err := d.preview.freshLoadRepo(ctx, d.path)
	if err != nil {
		return nil, nil, fmt.Errorf("error loading repo (first pass): %w", err)
	}

	right, err := d.preview.freshLoadRepo(ctx, d.path)
	if err != nil {
		return nil, nil, fmt.Errorf("error loading repo (second pass): %w", err)
	}
	for _, results := range []map[string]*loadRepoResult{left, right} {
		for _, result := range results {
			if len(result.errors) != 0 {
				return nil, nil, &ExpansionError{Errors: result.errors, Warnings: result.warnings}
			}
		}
	}

	return left, right, nil
}
