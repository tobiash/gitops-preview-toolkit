package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-logr/logr"
	"github.com/sethvargo/go-githubactions"
	"github.com/spf13/cobra"

	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/diff"
	"github.com/tobiash/gitops-preview-toolkit/pkg/diffsource"
	"github.com/tobiash/gitops-preview-toolkit/pkg/githubaction"
	"github.com/tobiash/gitops-preview-toolkit/pkg/preview"
)

func githubActionCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "github-action",
		Short:  "Run fmp in GitHub Actions mode",
		Long:   `Executes a manifest diff and produces structured outputs, summaries, and comments for GitHub Actions.`,
		RunE:   runGitHubAction,
		Hidden: true,
	}
}

func runGitHubAction(cmd *cobra.Command, args []string) error {
	act := githubactions.New()
	log := cliLogger()
	inCI := os.Getenv("GITHUB_ACTIONS") == "true"

	req, err := githubaction.ParseRequestFromEnv()
	if err != nil {
		if inCI {
			act.Errorf("%v", err)
			act.SetOutput("status", githubaction.StatusError)
			act.SetOutput("errors-count", "1")
		}
		return err
	}
	applyActionRuntimeFlags(cmd, req)

	report, diffResult, err := executeAction(cmd.Context(), log, req)
	if err != nil {
		if inCI {
			act.Errorf("action execution failed: %v", err)
		}
		if report == nil {
			report = &githubaction.ActionReport{
				Status: githubaction.StatusError,
				Errors: []string{err.Error()},
			}
		}
	}

	// Write report artifacts
	reportDir := os.Getenv("RUNNER_TEMP")
	if reportDir == "" {
		reportDir, _ = os.MkdirTemp("", "fmp-action-*")
	}

	reportFile := filepath.Join(reportDir, "fmp-report.json")
	report.ReportFile = reportFile

	diffFile := filepath.Join(reportDir, "fmp-diff.txt")
	if report.DiffPreview != "" {
		if err := os.WriteFile(diffFile, []byte(report.DiffPreview), 0o644); err != nil {
			if inCI {
				act.Warningf("writing diff file: %v", err)
			}
		} else {
			report.DiffFile = diffFile
		}
	}

	var summaryFile, commentFile string
	if req.WriteSummary {
		summaryFile = filepath.Join(reportDir, "fmp-summary.md")
		if err := os.WriteFile(summaryFile, []byte(githubaction.RenderSummaryMarkdown(req, report)), 0o644); err != nil {
			if inCI {
				act.Warningf("writing summary: %v", err)
			}
		} else {
			report.SummaryFile = summaryFile
			if inCI && os.Getenv("GITHUB_STEP_SUMMARY") != "" {
				act.AddStepSummary(githubaction.RenderSummaryMarkdown(req, report))
			}
		}
	}

	if req.ShouldComment(report) {
		commentFile = filepath.Join(reportDir, "fmp-comment.md")
		if err := os.WriteFile(commentFile, []byte(githubaction.RenderCommentMarkdown(req, report)), 0o644); err != nil {
			if inCI {
				act.Warningf("writing comment: %v", err)
			}
		} else {
			report.CommentFile = commentFile
		}
	}

	if req.HTMLReport {
		htmlDir := filepath.Join(reportDir, "fmp-html-report")
		if err := os.MkdirAll(htmlDir, 0o755); err != nil {
			if inCI {
				act.Warningf("creating html report dir: %v", err)
			}
		} else {
			htmlFile := filepath.Join(htmlDir, "index.html")
			html, err := githubaction.RenderHTMLReport(githubaction.BuildHTMLReportData(req, report, diffResult))
			if err != nil {
				if inCI {
					act.Warningf("rendering html report: %v", err)
				}
			} else if err := os.WriteFile(htmlFile, []byte(html), 0o644); err != nil {
				if inCI {
					act.Warningf("writing html report: %v", err)
				}
			} else {
				report.HTMLReportFile = htmlFile
			}
		}
	}

	if err := writeJSON(reportFile, report); err != nil {
		if inCI {
			act.Warningf("writing report: %v", err)
		}
	}

	// Set outputs
	if inCI {
		act.SetOutput("status", report.Status)
		act.SetOutput("changed", fmt.Sprintf("%t", report.Changed))
		act.SetOutput("warnings-count", fmt.Sprintf("%d", len(report.Warnings)))
		act.SetOutput("errors-count", fmt.Sprintf("%d", len(report.Errors)))
		act.SetOutput("resources-added", fmt.Sprintf("%d", report.ResourcesAdded))
		act.SetOutput("resources-modified", fmt.Sprintf("%d", report.ResourcesModified))
		act.SetOutput("resources-deleted", fmt.Sprintf("%d", report.ResourcesDeleted))
		act.SetOutput("resources-total", fmt.Sprintf("%d", report.ResourcesTotal))
		act.SetOutput("diff-bytes", fmt.Sprintf("%d", report.DiffBytes))
		act.SetOutput("diff-truncated", fmt.Sprintf("%t", report.DiffTruncated))
		act.SetOutput("diff-file", report.DiffFile)
		act.SetOutput("summary-file", report.SummaryFile)
		act.SetOutput("comment-file", report.CommentFile)
		act.SetOutput("report-file", report.ReportFile)
		act.SetOutput("html-report-file", report.HTMLReportFile)
		act.SetOutput("export-dir", report.ExportDir)
		act.SetOutput("classifications-json", mustJSON(report.Classifications))
		act.SetOutput("violations-json", mustJSON(report.Violations))
		act.SetOutput("labels-json", mustJSON(report.Labels))
		act.SetOutput("policy-failed", fmt.Sprintf("%t", report.PolicyFailed))
		act.SetOutput("ai-assessment-json", mustJSON(report.AIAssessment))
		if report.AIAssessment != nil {
			act.SetOutput("ai-summary", report.AIAssessment.Summary)
		}
	}

	// A failed render is not an advisory finding: no complete comparison exists.
	if err != nil {
		return err
	}
	if req.ShouldFail(report) || report.PolicyFailed {
		return fmt.Errorf("fmp action failed: status=%s errors=%d warnings=%d", report.Status, len(report.Errors), len(report.Warnings))
	}
	return nil
}

func executeAction(ctx context.Context, log logr.Logger, req *githubaction.Request) (*githubaction.ActionReport, *diff.DiffResult, error) {
	var diffText bytes.Buffer

	plan, err := actionDiffPlan(req)
	if err != nil {
		return nil, nil, err
	}
	materialized, cleanup, err := plan.Materialize(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer cleanup()

	cfg, err := loadActionConfig(req, materialized.ConfigRoot)
	if err != nil {
		return nil, nil, err
	}

	opts, err := buildActionOpts(log, req, cfg)
	if err != nil {
		return nil, nil, err
	}

	p, err := preview.New(opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("creating preview: %w", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			log.Error(err, "closing preview")
		}
	}()

	var policies *config.PolicyConfig
	var policyDir string
	if cfg != nil {
		policies = cfg.Policies
		policyDir = policyBaseDir(materialized.ConfigRoot, cfg)
	}

	run, err := p.RunDiff(ctx, preview.DiffRunOptions{
		RetainSnapshots: req.ExportDir != "",
		LeftPath:        materialized.LeftPath,
		RightPath:       materialized.RightPath,
		DiffWriter:      &diffText,
		Policies:        policies,
		PolicyBaseDir:   policyDir,
		AI:              aiConfigForAction(req.AIAssessment, req.AIProvider, req.AIModel, req.AIFailOnError, cfg),
	})
	if err != nil {
		// Try to produce a partial report even on error
		report := &githubaction.ActionReport{
			Status:    githubaction.StatusError,
			Errors:    []string{err.Error()},
			DiffBytes: diffText.Len(),
		}
		if run != nil {
			report.Warnings = run.Warnings
		}
		if diffText.Len() > 0 {
			preview, truncated := githubaction.TruncateDiff(diffText.String(), req.MaxInlineDiffBytes, req.DiffPreviewLines)
			report.DiffPreview = preview
			report.DiffTruncated = truncated
		}
		return report, nil, err
	}

	report := githubaction.BuildReport(githubaction.ReportInput{
		Result:             run.Result,
		PolicyResult:       run.PolicyResult,
		Warnings:           run.Warnings,
		FullDiff:           run.DiffText,
		MaxInlineDiffBytes: req.MaxInlineDiffBytes,
		DiffPreviewLines:   req.DiffPreviewLines,
		AIAssessment:       run.AIAssessment,
	})

	if req.ExportDir != "" {
		if err := exportSnapshot(ctx, run.After, run.Result, req.ExportDir, req.ExportChangedOnly); err != nil {
			err = fmt.Errorf("exporting rendered target: %w", err)
			report.Status = githubaction.StatusError
			report.Errors = append(report.Errors, err.Error())
			return report, run.Result, err
		}
		report.ExportDir = req.ExportDir
	}

	return report, run.Result, nil
}

func actionDiffPlan(req *githubaction.Request) (*diffsource.Plan, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("determining current directory: %w", err)
	}
	return diffsource.ResolvePlan([]string{
		actionDiffTarget(req.DiffLeft(), cwd),
		actionDiffTarget(req.DiffRight(), cwd),
	}, cwd)
}

func actionDiffTarget(target, cwd string) string {
	if hasDiffSourcePrefix(target) {
		return target
	}
	path := target
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	if _, err := os.Stat(path); err == nil {
		return "path:" + target
	}
	return "git:" + target
}

func hasDiffSourcePrefix(target string) bool {
	return strings.HasPrefix(target, "git:") || strings.HasPrefix(target, "path:")
}

func buildActionOpts(log logr.Logger, req *githubaction.Request, cfg *config.Config) ([]preview.Opt, error) {
	paths := req.Paths
	if len(paths) == 0 && cfg != nil && len(cfg.Paths) > 0 {
		paths = cfg.Paths
	}

	s := resolvedSettings{
		paths:       paths,
		recursive:   req.Recursive,
		renderHelm:  req.RenderHelm,
		resolveGit:  req.ResolveGit,
		sort:        req.Sort,
		excludeCRDs: req.ExcludeCRDs,
		helmRelease: req.HelmRelease,
		helm: &config.HelmSettings{
			RegistryConfig: req.RegistryConfig, RepositoryConfig: req.RepositoryConfig, RepositoryCache: req.RepositoryCache,
		},
	}

	clusterPaths := req.ClusterPaths
	if cfg != nil {
		s.helm = mergeHelmSettings(s.helm, cfg.HelmSettings)
		if len(s.paths) == 0 && len(clusterPaths) == 0 && cfg.ClusterPaths() != nil {
			clusterPaths = cfg.ClusterPaths()
		}
		if !s.recursive && cfg.Recursive != nil {
			s.recursive = *cfg.Recursive
		}
		if !s.renderHelm && cfg.Helm != nil {
			s.renderHelm = *cfg.Helm
		}
		if !s.sort && cfg.Sort != nil {
			s.sort = *cfg.Sort
		}
		if !s.excludeCRDs && cfg.ExcludeCRDs != nil {
			s.excludeCRDs = *cfg.ExcludeCRDs
		}
	}
	s.clusterPaths = clusterPaths

	if req.SOPSDecrypt {
		return nil, fmt.Errorf("sops-decrypt is not supported in GitHub Action mode")
	}

	opts := optsFromSettings(log, s)
	pluginOpts, err := runtimePluginOptions(cfg, runtimeSettings{
		enabled: req.Crossplane,
		engine:  req.CrossplaneEngine, functions: req.CrossplaneFunctions,
		flux: req.FluxPlugin, crossplane: req.CrossplanePlugin,
	})
	if err != nil {
		return nil, err
	}
	opts = append(opts, pluginOpts...)

	if req.FilterFile != "" {
		data, err := os.ReadFile(req.FilterFile)
		if err != nil {
			return nil, fmt.Errorf("opening filter file: %w", err)
		}
		opts = append(opts, preview.WithFilterYAML(string(data)))
	} else if req.FilterYAML != "" {
		opts = append(opts, preview.WithFilterYAML(req.FilterYAML))
	} else if cfg != nil && len(cfg.Filters.Filters) > 0 {
		opts = append(opts, preview.WithFilterConfig(&cfg.Filters))
	}

	return opts, nil
}

func applyActionRuntimeFlags(cmd *cobra.Command, req *githubaction.Request) {
	if flagChanged(cmd, "crossplane") {
		req.Crossplane = crossplaneEnabled
	}
	if flagChanged(cmd, "crossplane-engine") {
		req.CrossplaneEngine = crossplaneEngine
	}
	if flagChanged(cmd, "crossplane-function") {
		req.CrossplaneFunctions = append([]string(nil), crossplaneFunctions...)
	}
	if flagChanged(cmd, "flux-plugin") {
		req.FluxPlugin = fluxPlugin
	}
	if flagChanged(cmd, "crossplane-plugin") {
		req.CrossplanePlugin = crossplanePlugin
	}
}

func loadActionConfig(req *githubaction.Request, configRoot string) (*config.Config, error) {
	cfg, err := loadConfigForRepo(configRoot, req.ConfigFile)
	if err != nil {
		if req.ConfigFile != "" {
			return nil, fmt.Errorf("loading config %s: %w", req.ConfigFile, err)
		}
		return nil, fmt.Errorf("loading config: %w", err)
	}
	return cfg, nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func mustJSON(v any) string {
	if v == nil {
		return "[]"
	}
	data, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	if string(data) == "null" {
		return "[]"
	}
	return string(data)
}
