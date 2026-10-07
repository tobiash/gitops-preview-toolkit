package preview

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/tobiash/gitops-preview-toolkit/pkg/ai"
	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/diff"
	"github.com/tobiash/gitops-preview-toolkit/pkg/policy"
)

// DiffRunOptions describes one complete rendered manifest diff run.
type DiffRunOptions struct {
	RetainSnapshots bool
	LeftPath        string
	RightPath       string
	DiffWriter      io.Writer
	Policies        *config.PolicyConfig
	PolicyBaseDir   string
	AI              *config.AIConfig
	AIAssessor      ai.Assessor
}

// DiffRunResult is the domain result used by CLI and GitHub Action adapters.
type DiffRunResult struct {
	Before, After *Snapshot
	Complete      bool
	Result        *diff.DiffResult
	Summary       diff.ResultSummary
	DiffText      string
	Warnings      []string
	PolicyResult  *policy.Result
	AIAssessment  *ai.Assessment
}

// RunDiff renders both sides, computes the rendered manifest diff, and applies policy checks.
// Incomplete renders return an error and a result with Complete=false, diagnostics,
// no authoritative changes, and no policy or AI assessment.
func (p *Preview) RunDiff(ctx context.Context, opts DiffRunOptions) (run *DiffRunResult, runErr error) {
	var diffText bytes.Buffer
	result, before, after, err := p.diffSnapshots(ctx, opts.LeftPath, opts.RightPath, &diffText)
	defer func() {
		if opts.RetainSnapshots {
			if run == nil {
				run = &DiffRunResult{Result: &diff.DiffResult{}}
			}
			run.Before, run.After = before, after
		}
	}()
	if err != nil {
		var expansionErr *ExpansionError
		if !errors.As(err, &expansionErr) || result == nil {
			return nil, err
		}
		if len(expansionErr.Errors) > 0 {
			return &DiffRunResult{Result: &diff.DiffResult{}, Warnings: expansionWarnings(err)}, err
		}
	}
	if opts.DiffWriter != nil {
		if _, err := io.Copy(opts.DiffWriter, bytes.NewReader(diffText.Bytes())); err != nil {
			return nil, fmt.Errorf("writing diff text: %w", err)
		}
	}

	warnings := expansionWarnings(err)
	var policyResult *policy.Result
	if opts.Policies != nil {
		policyResult, err = policy.Evaluate(ctx, result, opts.Policies, opts.PolicyBaseDir)
		if err != nil {
			return nil, fmt.Errorf("evaluating policies: %w", err)
		}
	}
	if policyResult == nil {
		policyResult = &policy.Result{}
	}

	aiAssessment, aiWarnings, err := runAIAssessment(ctx, opts, result, policyResult)
	warnings = append(warnings, aiWarnings...)
	if err != nil {
		if ai.FailOnError(opts.AI) {
			return nil, err
		}
		warnings = append(warnings, err.Error())
	}
	if aiAssessment != nil {
		policyResult.Classifications = append(policyResult.Classifications, aiAssessment.Classifications...)
		policy.ApplyEffects(policyResult, opts.Policies)
	}

	return &DiffRunResult{
		Complete:     true,
		Result:       result,
		Summary:      result.Summary(),
		DiffText:     diffText.String(),
		Warnings:     warnings,
		PolicyResult: policyResult,
		AIAssessment: aiAssessment,
	}, nil
}

func runAIAssessment(ctx context.Context, opts DiffRunOptions, result *diff.DiffResult, policyResult *policy.Result) (*ai.Assessment, []string, error) {
	if !ai.Enabled(opts.AI) || result == nil || result.TotalChanged() == 0 {
		return nil, nil, nil
	}
	assessor := opts.AIAssessor
	if assessor == nil {
		assessor = ai.NewLangChainAssessor()
	}
	assessment, err := assessor.Assess(ctx, ai.Request{Config: opts.AI, Result: result, PolicyResult: policyResult})
	if err != nil {
		return nil, nil, fmt.Errorf("ai assessment failed: %w", err)
	}
	if assessment == nil {
		return nil, nil, nil
	}
	return assessment, assessment.Warnings, nil
}

func expansionWarnings(err error) []string {
	var expansionErr *ExpansionError
	if !errors.As(err, &expansionErr) {
		return nil
	}
	warnings := make([]string, 0, len(expansionErr.Errors)+len(expansionErr.Warnings))
	seen := make(map[string]struct{})
	issues := append(append([]error(nil), expansionErr.Errors...), expansionErr.Warnings...)
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		msg := issue.Error()
		if _, ok := seen[msg]; ok {
			continue
		}
		seen[msg] = struct{}{}
		warnings = append(warnings, msg)
	}
	return warnings
}
