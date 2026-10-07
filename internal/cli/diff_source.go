package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/go-logr/logr"
	"github.com/spf13/cobra"

	"github.com/tobiash/gitops-preview-toolkit/pkg/ai"
	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/diff"
	"github.com/tobiash/gitops-preview-toolkit/pkg/diffsource"
	"github.com/tobiash/gitops-preview-toolkit/pkg/githubaction"
	"github.com/tobiash/gitops-preview-toolkit/pkg/policy"
	"github.com/tobiash/gitops-preview-toolkit/pkg/preview"
)

func validateDiffArgs(_ *cobra.Command, args []string) error {
	if len(args) > 2 {
		return fmt.Errorf("accepts at most 2 arg(s), received %d", len(args))
	}
	return nil
}

type diffExecResult struct {
	result       *diff.DiffResult
	diffText     string
	warnings     []string
	policyResult *policy.Result
	aiAssessment *ai.Assessment
	plan         *diffsource.Plan
}

func executeDiff(cmd *cobra.Command, ctx context.Context, log logr.Logger, args []string) (*diffExecResult, func(), error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, fmt.Errorf("determining current directory: %w", err)
	}

	plan, err := diffsource.ResolvePlan(args, cwd)
	if err != nil {
		return nil, nil, err
	}

	materialized, cleanup, err := plan.Materialize(ctx)
	if err != nil {
		return nil, nil, err
	}

	opts, err := buildOpts(cmd, log, materialized.ConfigRoot)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	if helmRelease != "" {
		opts = append(opts, preview.WithHelmReleaseFilter(helmRelease))
	}

	p, err := preview.New(opts...)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("error creating preview: %w", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			log.Error(err, "closing preview")
		}
	}()

	cfg, err := loadConfigForRepo(materialized.ConfigRoot, configFile)
	if err != nil {
		cleanup()
		if configFile != "" {
			return nil, nil, fmt.Errorf("loading config %s: %w", configFile, err)
		}
		return nil, nil, fmt.Errorf("loading config: %w", err)
	}

	var policies *config.PolicyConfig
	var policyDir string
	if cfg != nil {
		policies = cfg.Policies
		policyDir = policyBaseDir(materialized.ConfigRoot, cfg)
	}

	run, err := p.RunDiff(ctx, preview.DiffRunOptions{
		LeftPath:      materialized.LeftPath,
		RightPath:     materialized.RightPath,
		Policies:      policies,
		PolicyBaseDir: policyDir,
		AI:            aiConfigForCommand(cmd, cfg),
	})
	if err != nil {
		cleanup()
		return nil, nil, err
	}

	return &diffExecResult{
		result:       run.Result,
		diffText:     run.DiffText,
		warnings:     run.Warnings,
		policyResult: run.PolicyResult,
		aiAssessment: run.AIAssessment,
		plan:         plan,
	}, cleanup, nil
}

func runDiff(cmd *cobra.Command, log logr.Logger, args []string, summaryOut io.Writer, diffOut io.Writer) error {
	dr, cleanup, err := executeDiff(cmd, commandContext(cmd), log, args)
	if err != nil {
		return err
	}
	defer cleanup()

	for _, warning := range dr.warnings {
		fmt.Fprintf(os.Stderr, "WARNING: %s\n", warning)
	}
	if err := writeDiffSummary(summaryOut, dr.result, dr.policyResult); err != nil {
		return fmt.Errorf("writing diff summary: %w", err)
	}
	if err := writeAISummary(summaryOut, dr.aiAssessment); err != nil {
		return fmt.Errorf("writing ai summary: %w", err)
	}
	if _, err = io.Copy(diffOut, strings.NewReader(dr.diffText)); err != nil {
		return err
	}
	if dr.policyResult != nil && dr.policyResult.PolicyFailed {
		return fmt.Errorf("%w: %s", ErrPolicyViolation, strings.Join(dr.policyResult.PolicyFailures, ", "))
	}
	if diffExitCode && dr.result.TotalChanged() > 0 {
		return fmt.Errorf("manifest differences found")
	}
	return nil
}

func runDiffJSON(cmd *cobra.Command, log logr.Logger, args []string, out io.Writer) error {
	dr, cleanup, err := executeDiff(cmd, commandContext(cmd), log, args)
	if err != nil {
		return err
	}
	defer cleanup()

	jsonResult := dr.result.ToJSON()
	output := map[string]any{
		"complete":      true,
		"warnings":      dr.warnings,
		"changes":       jsonResult.Changes,
		"added":         jsonResult.Added,
		"deleted":       jsonResult.Deleted,
		"modified":      jsonResult.Modified,
		"policy":        dr.policyResult,
		"ai_assessment": dr.aiAssessment,
	}

	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(output); err != nil {
		return fmt.Errorf("encoding JSON: %w", err)
	}

	if dr.policyResult != nil && dr.policyResult.PolicyFailed {
		return fmt.Errorf("%w: %s", ErrPolicyViolation, strings.Join(dr.policyResult.PolicyFailures, ", "))
	}
	if diffExitCode && dr.result.TotalChanged() > 0 {
		return fmt.Errorf("manifest differences found")
	}
	return nil
}

func runDiffHTML(cmd *cobra.Command, log logr.Logger, args []string) error {
	dr, cleanup, err := executeDiff(cmd, commandContext(cmd), log, args)
	if err != nil {
		return err
	}
	defer cleanup()

	report := githubaction.BuildReport(githubaction.ReportInput{
		Result:       dr.result,
		PolicyResult: dr.policyResult,
		Warnings:     dr.warnings,
		FullDiff:     dr.diffText,
		AIAssessment: dr.aiAssessment,
	})
	req := &githubaction.Request{
		BaseRef:                        dr.plan.Left.Label(),
		Repo:                           dr.plan.Right.Label(),
		HTMLReportMaxResourceDiffBytes: 2_000_000,
	}
	html, err := githubaction.RenderHTMLReport(githubaction.BuildHTMLReportData(req, report, dr.result))
	if err != nil {
		return fmt.Errorf("rendering html report: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "fmp-html-report-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	htmlFile := filepath.Join(tmpDir, "index.html")
	if err := os.WriteFile(htmlFile, []byte(html), 0o644); err != nil {
		return fmt.Errorf("writing html report: %w", err)
	}

	fmt.Fprintf(os.Stderr, "HTML report written to %s\n", htmlFile)
	if diffHTMLOpen {
		if err := openBrowser(htmlFile); err != nil {
			return err
		}
	}
	if dr.policyResult != nil && dr.policyResult.PolicyFailed {
		return fmt.Errorf("%w: %s", ErrPolicyViolation, strings.Join(dr.policyResult.PolicyFailures, ", "))
	}
	if diffExitCode && dr.result.TotalChanged() > 0 {
		return fmt.Errorf("manifest differences found")
	}
	return nil
}

func openBrowser(url string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
		args = []string{url}
	case "windows":
		cmd = "cmd"
		args = []string{"/c", "start", url}
	default:
		cmd = "xdg-open"
		args = []string{url}
	}
	bin, err := exec.LookPath(cmd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not find %q to open browser; open the file manually.\n", cmd)
		return nil
	}
	return exec.Command(bin, args...).Start()
}

func commandContext(cmd *cobra.Command) context.Context {
	if cmd == nil {
		return context.Background()
	}
	return cmd.Context()
}
