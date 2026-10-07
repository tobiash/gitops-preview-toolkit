package preview

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/diff"
	"github.com/tobiash/gitops-preview-toolkit/pkg/filter"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	"github.com/tobiash/gitops-preview-toolkit/pkg/pluginhost"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"gopkg.in/yaml.v3"
)

// Preview renders and diffs Flux GitOps resources.
type Preview struct {
	paths           []string
	clusterPaths    map[string][]string
	recursive       bool
	sortOutput      bool
	excludeCRDs     bool
	helmReleaseName string
	sopsDecrypt     bool
	filters         *filter.FilterConfig
	fluxKSEnabled   bool
	resolveGit      bool
	helmSettings    *config.HelmSettings
	commands        []plugin.Command
	crossplane      *plugin.Command
	host            *pluginhost.Host
	borrowedHost    bool
	log             logr.Logger
	localOnly       bool
	strictInputs    bool
	runID           string
	runActive       bool
}

// beginRun freezes source-selector resolutions across the independent sides of
// one comparison, while a subsequent operation starts a new acquisition epoch.
func (p *Preview) beginRun() func() {
	previousID, previousActive := p.runID, p.runActive
	p.runID, p.runActive = rand.Text(), true
	return func() { p.runID, p.runActive = previousID, previousActive }
}

func (p *Preview) isClustered() bool {
	return p.clusterPaths != nil
}

// ExpansionError is returned when one or more non-fatal errors were
// encountered during expansion (e.g. a HelmRelease whose chart could
// not be resolved). The render/diff output is still produced but may
// be incomplete.
type ExpansionError struct {
	Errors   []error
	Warnings []error
}

func (e *ExpansionError) Error() string {
	var msgs []string
	for _, err := range e.Errors {
		msgs = append(msgs, err.Error())
	}
	return fmt.Sprintf("expansion errors: %s", strings.Join(msgs, "; "))
}

func sortedClusterNames(results map[string]*loadRepoResult) []string {
	clusters := make([]string, 0, len(results))
	for cluster := range results {
		clusters = append(clusters, cluster)
	}
	sort.Strings(clusters)
	return clusters
}

// Render renders the resources at path and writes the YAML output.
func (p *Preview) Render(ctx context.Context, path string, out io.Writer) error {
	results, err := p.loadRepo(ctx, path)
	if err != nil {
		return fmt.Errorf("error loading repo: %w", err)
	}
	if p.isClustered() {
		_, _ = fmt.Fprintln(out, "# Rendered manifests")
		clusters := sortedClusterNames(results)
		for _, cluster := range clusters {
			result := results[cluster]
			p.applyOutputOptions(result.render)
			_, _ = fmt.Fprintf(out, "\n---\n# cluster: %s\n---\n", cluster)
			yaml, err := result.asYAML()
			if err != nil {
				return fmt.Errorf("error transforming cluster %q to yaml: %w", cluster, err)
			}
			if _, err := out.Write(yaml); err != nil {
				return fmt.Errorf("error writing output: %w", err)
			}
		}
		var allErrors []error
		var allWarnings []error
		for _, cluster := range clusters {
			allErrors = append(allErrors, results[cluster].errors...)
			allWarnings = append(allWarnings, results[cluster].warnings...)
		}
		if len(allErrors) > 0 {
			return &ExpansionError{Errors: allErrors, Warnings: allWarnings}
		}
		return nil
	}
	result := results[""]
	p.applyOutputOptions(result.render)
	yaml, err := result.asYAML()
	if err != nil {
		return fmt.Errorf("error transforming to yaml: %w", err)
	}
	if _, err := out.Write(yaml); err != nil {
		return fmt.Errorf("error writing output: %w", err)
	}
	if len(result.errors) > 0 {
		return &ExpansionError{Errors: result.errors, Warnings: result.warnings}
	}
	return nil
}

// RenderJSON renders the resources at path and writes JSON output.
func (p *Preview) RenderJSON(ctx context.Context, path string, out io.Writer) error {
	results, err := p.loadRepo(ctx, path)
	if err != nil {
		return fmt.Errorf("error loading repo: %w", err)
	}
	diagnostics := &ExpansionError{}
	clusters := sortedClusterNames(results)
	for _, cluster := range clusters {
		diagnostics.Errors = append(diagnostics.Errors, results[cluster].errors...)
		diagnostics.Warnings = append(diagnostics.Warnings, results[cluster].warnings...)
	}
	if p.isClustered() {
		items := make([]map[string]any, 0)
		for _, cluster := range clusters {
			result := results[cluster]
			p.applyOutputOptions(result.render)
			objects, err := result.objects()
			if err != nil {
				diagnostics.Errors = append(diagnostics.Errors, fmt.Errorf("converting cluster %q to JSON: %w", cluster, err))
				return diagnostics
			}
			for _, m := range objects {
				m["_fmp_cluster"] = cluster
				items = append(items, m)
			}
		}
		list := map[string]any{
			"apiVersion": "v1",
			"kind":       "List",
			"items":      items,
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(list); err != nil {
			return fmt.Errorf("error encoding json: %w", err)
		}
		if len(diagnostics.Errors) > 0 {
			return diagnostics
		}
		return nil
	}
	result := results[""]
	p.applyOutputOptions(result.render)
	objects, err := result.objects()
	if err != nil {
		diagnostics.Errors = append(diagnostics.Errors, fmt.Errorf("error transforming to json: %w", err))
		return diagnostics
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{"apiVersion": "v1", "kind": "List", "items": objects}); err != nil {
		return fmt.Errorf("error encoding json: %w", err)
	}
	if len(diagnostics.Errors) > 0 {
		return diagnostics
	}
	return nil
}

// TestResult is the JSON representation of a test run.
type TestResult struct {
	Status   string      `json:"status"`
	Warnings []TestIssue `json:"warnings,omitempty"`
	Errors   []TestIssue `json:"errors,omitempty"`
}

// TestIssue describes a single warning or error encountered during testing.
type TestIssue struct {
	Message string `json:"message"`
}

// Test validates that all Kustomizations build and HelmReleases render.
// Returns nil on success, or an error describing the failure.
func (p *Preview) Test(ctx context.Context, path string, out io.Writer) error {
	results, err := p.loadRepo(ctx, path)
	if err != nil {
		_, _ = fmt.Fprintf(out, "FAIL: %v\n", err)
		return err
	}
	var allErrors []error
	clusters := sortedClusterNames(results)
	for _, cluster := range clusters {
		result := results[cluster]
		if p.isClustered() {
			_, _ = fmt.Fprintf(out, "Cluster %q: ", cluster)
		}
		if len(result.errors) > 0 {
			for _, e := range result.errors {
				_, _ = fmt.Fprintf(out, "ERROR: %v\n", e)
			}
			_, _ = fmt.Fprintln(out, "FAIL (incomplete render)")
		} else {
			_, _ = fmt.Fprintln(out, "PASS")
		}
		allErrors = append(allErrors, result.errors...)
	}
	if len(allErrors) > 0 {
		return &ExpansionError{Errors: allErrors}
	}
	return nil
}

// TestJSON validates resources and returns a structured test result.
func (p *Preview) TestJSON(ctx context.Context, path string) (*TestResult, error) {
	results, err := p.loadRepo(ctx, path)
	if err != nil {
		return &TestResult{
			Status: "fail",
			Errors: []TestIssue{{Message: err.Error()}},
		}, err
	}
	var warnings []TestIssue
	var issues []TestIssue
	var expansionErrors []error
	for _, cluster := range sortedClusterNames(results) {
		for _, e := range results[cluster].errors {
			issues = append(issues, TestIssue{Message: e.Error()})
			expansionErrors = append(expansionErrors, e)
		}
		for _, e := range results[cluster].warnings {
			warnings = append(warnings, TestIssue{Message: e.Error()})
		}
	}
	if len(issues) > 0 {
		return &TestResult{Status: "fail", Errors: issues, Warnings: warnings}, &ExpansionError{Errors: expansionErrors}
	}
	if len(warnings) > 0 {
		return &TestResult{Status: "pass_with_warnings", Warnings: warnings}, nil
	}
	return &TestResult{Status: "pass"}, nil
}

// Diff computes and writes the diff between two repository paths.
// If a HelmRelease filter is set, only resources from that release are included.
func (p *Preview) Diff(ctx context.Context, a, b string, out io.Writer) error {
	_, err := p.DiffResult(ctx, a, b, out)
	var expansionErr *ExpansionError
	if errors.As(err, &expansionErr) && len(expansionErr.Errors) == 0 {
		return nil
	}
	return err
}

// DiffResult computes and writes the diff between two repository paths,
// returning structured change metadata alongside the rendered diff text.
// An ExpansionError with only Warnings accompanies a complete comparison;
// expansion errors suppress all changes because either side may be incomplete.
func (p *Preview) DiffResult(ctx context.Context, a, b string, out io.Writer) (result *diff.DiffResult, resultErr error) {
	result, _, _, resultErr = p.diffSnapshots(ctx, a, b, out)
	return result, resultErr
}

// Opt is a functional option for configuring Preview.
type Opt func(p *Preview) error

// New creates a new Preview with the given options.
func New(opts ...Opt) (*Preview, error) {
	var p Preview
	for _, opt := range opts {
		if err := opt(&p); err != nil {
			return nil, errors.Join(err, p.Close())
		}
	}
	if p.localOnly && p.sopsDecrypt {
		return nil, errors.Join(fmt.Errorf("local-only mode does not support SOPS key acquisition"), p.Close())
	}
	if p.borrowedHost {
		if len(p.commands) != 0 || p.crossplane != nil {
			return nil, fmt.Errorf("borrowed plugin host cannot select plugin commands")
		}
		if _, err := p.sessionConfig(); err != nil {
			return nil, err
		}
		return &p, nil
	}
	commands, err := p.pluginCommands()
	if err != nil {
		return nil, err
	}
	p.host, err = pluginhost.New(commands)
	if err != nil {
		return nil, fmt.Errorf("creating plugin host: %w", err)
	}
	return &p, nil
}

// Close releases owned plugin processes after rendering stops. Borrowed hosts
// remain alive until their owner closes them.
func (p *Preview) Close() error {
	if p.host != nil && !p.borrowedHost {
		return p.host.Close()
	}
	return nil
}

// WithLocalOnly rejects remote acquisition and filesystem references outside the
// current source root and implies WithStrictInputs. This is not an OS sandbox.
// The bundled renderer enforces this policy; Kustomize execution plugins remain disabled.
func WithLocalOnly() Opt {
	return func(p *Preview) error {
		p.localOnly = true
		p.strictInputs = true
		return nil
	}
}

// WithStrictInputs rejects known unsupported Flux rendering inputs rather than
// silently omitting them. It does not restrict source acquisition or local paths.
func WithStrictInputs() Opt {
	return func(p *Preview) error {
		p.strictInputs = true
		return nil
	}
}

// WithLogger sets the logger for the Preview.
func WithLogger(log logr.Logger) Opt {
	return func(p *Preview) error {
		p.log = log
		return nil
	}
}

// WithFilterFile configures filters from a YAML file.
func WithFilterFile(f *os.File) Opt {
	return func(p *Preview) error {
		m := &filter.FilterConfig{}
		d := yaml.NewDecoder(f)
		if err := d.Decode(m); err != nil {
			return err
		}
		p.filters = m
		return nil
	}
}

// WithFilterYAML configures filters from a raw YAML string.
func WithFilterYAML(f string) Opt {
	return func(p *Preview) error {
		m := &filter.FilterConfig{}
		if err := yaml.Unmarshal([]byte(f), m); err != nil {
			return err
		}
		p.filters = m
		return nil
	}
}

// WithFilterConfig configures filters from a parsed FilterConfig.
func WithFilterConfig(fc *filter.FilterConfig) Opt {
	return func(p *Preview) error {
		p.filters = fc
		return nil
	}
}

// WithHelm enables Helm rendering with neutral settings. Empty settings use the
// Flux plugin's Helm environment defaults.
func WithHelm(settings *config.HelmSettings) Opt {
	return func(p *Preview) error {
		p.helmSettings = &config.HelmSettings{}
		if settings != nil {
			*p.helmSettings = *settings
		}
		return nil
	}
}

// WithFluxKS enables discovery of paths from Flux Kustomization resources in
// the Flux plugin. WithGitRepo enables acquisition of their external sources.
func WithFluxKS() Opt {
	return func(p *Preview) error {
		p.fluxKSEnabled = true
		return nil
	}
}

// WithGitRepo enables GitRepository acquisition in the Flux plugin.
func WithGitRepo() Opt {
	return func(p *Preview) error {
		p.resolveGit = true
		return nil
	}
}

// WithPaths configures the paths to render and whether to recurse into subdirectories.
// If any path contains a cluster prefix (e.g. "kube:clusters/kube"), the
// preview switches to cluster mode automatically.
func WithPaths(paths []string, recursive bool) Opt {
	return func(p *Preview) error {
		clusterPaths := make(map[string][]string)
		for _, s := range paths {
			cluster, path := config.ParseClusterPath(s)
			if cluster != "" {
				clusterPaths[cluster] = append(clusterPaths[cluster], path)
			}
		}
		if len(clusterPaths) > 0 {
			// Some paths had prefixes — merge any unprefixed paths into "" cluster
			for _, s := range paths {
				cluster, path := config.ParseClusterPath(s)
				if cluster == "" {
					clusterPaths[""] = append(clusterPaths[""], path)
				}
			}
			p.clusterPaths = clusterPaths
		} else {
			p.paths = append(p.paths, paths...)
		}
		p.recursive = recursive
		return nil
	}
}

// WithClusterPaths configures explicit per-cluster paths. This overrides any
// paths set via WithPaths.
func WithClusterPaths(clusterPaths map[string][]string) Opt {
	return func(p *Preview) error {
		p.clusterPaths = clusterPaths
		p.paths = nil
		return nil
	}
}

// applyOutputOptions applies sort and CRD filtering to the render result.
func (p *Preview) applyOutputOptions(r *render.Render) {
	if p.sortOutput {
		r.Sort()
	}
	if p.excludeCRDs {
		r.FilterCRDs()
	}
}

// WithSort enables deterministic output sorting by (kind, namespace, name).
func WithSort() Opt {
	return func(p *Preview) error {
		p.sortOutput = true
		return nil
	}
}

// WithExcludeCRDs strips CustomResourceDefinitions from rendered output.
func WithExcludeCRDs() Opt {
	return func(p *Preview) error {
		p.excludeCRDs = true
		return nil
	}
}

// WithHelmReleaseFilter filters diff output to only resources from the
// specified HelmRelease (matched by the helm.toolkit.fluxcd.io/name label).
func WithHelmReleaseFilter(name string) Opt {
	return func(p *Preview) error {
		p.helmReleaseName = name
		return nil
	}
}

// WithSOPSDecrypt enables decryption of SOPS-encrypted secrets before
// diffing or rendering. Requires access to the appropriate decryption keys.
func WithSOPSDecrypt() Opt {
	return func(p *Preview) error {
		p.sopsDecrypt = true
		return nil
	}
}

// DetectPermadiffs renders the same path twice and compares the results
// to find non-deterministic output. It generates a filter config that
// can be used to normalize these fields in subsequent diff/render runs.
// Each render pass uses a fresh evaluation while reusing the plugin processes.
func (p *Preview) DetectPermadiffs(ctx context.Context, path string, out io.Writer) error {
	return normalizationDiscovery{preview: p, path: path}.WritePermadiffConfig(ctx, out)
}

// DiagnoseNormalization renders twice and returns non-deterministic fields with cluster context.
func (p *Preview) DiagnoseNormalization(ctx context.Context, path string) (*NormalizationDiagnosis, error) {
	return normalizationDiscovery{preview: p, path: path}.Diagnose(ctx)
}

// GenerateInitConfig renders the repo twice to detect permadiffs and
// writes a complete .fmp.yaml config file to destPath.
func (p *Preview) GenerateInitConfig(ctx context.Context, path, destPath string) error {
	diffs, err := normalizationDiscovery{preview: p, path: path}.Detect(ctx)
	if err != nil {
		return err
	}

	type initConfig struct {
		Paths       []string `yaml:"paths,omitempty"`
		Recursive   *bool    `yaml:"recursive,omitempty"`
		Helm        *bool    `yaml:"helm,omitempty"`
		ResolveGit  *bool    `yaml:"resolve-git,omitempty"`
		SOPSDecrypt *bool    `yaml:"sops-decrypt,omitempty"`
		Sort        *bool    `yaml:"sort,omitempty"`
		ExcludeCRDs *bool    `yaml:"exclude-crds,omitempty"`
		Filters     []any    `yaml:"filters,omitempty"`
	}

	cfg := initConfig{
		Sort:        boolPtr(true),
		ExcludeCRDs: boolPtr(true),
	}

	if len(diffs) > 0 {
		rules := diff.GroupDiffsToRules(diffs)
		for _, rule := range rules {
			cfg.Filters = append(cfg.Filters, map[string]any{
				"kind":       "FieldNormalizer",
				"match":      rule.Match,
				"fieldPaths": rule.FieldPaths,
			})
		}
	}

	data, err := yaml.Marshal(&cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("creating %s: %w", destPath, err)
	}
	defer func() { _ = f.Close() }()

	if _, err := fmt.Fprint(f, "# .fmp.yaml — fmp per-repo configuration\n"); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return nil
}

// freshLoadRepo opens independent render sessions. Acquisition artifacts may be
// reused, but discovery and evaluation state must not hide permadiffs.
func (p *Preview) freshLoadRepo(ctx context.Context, path string) (map[string]*loadRepoResult, error) {
	return p.loadRepoOptions(ctx, path, true)
}

func boolPtr(b bool) *bool {
	return &b
}
