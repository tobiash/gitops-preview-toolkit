package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/go-logr/logr"
	"github.com/go-logr/zerologr"
	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/preview"
)

var (
	kustomizations  []string
	recursive       bool
	renderHelm      bool
	filtersFile     string
	filterYAML      string
	sortOutput      bool
	excludeCRDs     bool
	verbose         bool
	quiet           bool
	resolveGit      bool
	sopsDecrypt     bool
	configFile      string
	outputFormat    string
	helmRelease     string
	diffSummary     bool
	diffSummaryOnly bool
	diffHTML        bool
	diffHTMLOpen    bool
	diffExitCode    bool
	aiAssessment    bool
	aiProvider      string
	aiModel         string
	aiFailOnError   bool
	initConfig      bool

	helmRegistryConfig   string
	helmRepositoryConfig string
	helmRepositoryCache  string

	// version is set by goreleaser via ldflags.
	version = "dev"
)

// Run executes the shared CLI and returns its process exit code.
func Run(name, buildVersion string) int {
	version = buildVersion
	restorePath := exposeSiblingPlugins()
	defer restorePath()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var jsonOutput bytes.Buffer
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	zerologr.NameFieldName = "logger"
	zerologr.NameSeparator = "/"

	rootCmd := &cobra.Command{
		Use:   name,
		Short: "gitops-preview-toolkit — render and diff GitOps resources",
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Lookup("output") != nil && outputFormat != "yaml" && outputFormat != "json" {
				return fmt.Errorf("%w: output must be yaml or json", ErrUserInput)
			}
			if cmd.Name() == "diff" && diffHTML && outputFormat == "json" {
				return fmt.Errorf("%w: --html and --output json cannot be combined", ErrUserInput)
			}
			return nil
		},
	}

	rootCmd.PersistentFlags().StringSliceVarP(&kustomizations, "path", "k", nil, "Path to render (kustomize base or directory of YAML, relative to repo root, repeatable)")
	rootCmd.PersistentFlags().BoolVarP(&recursive, "recursive", "r", false, "Recursively discover all paths under each -k directory")
	rootCmd.PersistentFlags().BoolVarP(&renderHelm, "render-helm", "H", true, "Render HelmRelease objects")
	rootCmd.PersistentFlags().StringVar(&filtersFile, "filter", "", "KIO filters definition file (overrides repository filters)")
	rootCmd.PersistentFlags().StringVar(&filterYAML, "filter-yaml", "", "KIO filters YAML string (overrides repository filters)")
	rootCmd.PersistentFlags().BoolVarP(&sortOutput, "sort", "s", false, "Sort output by (kind, namespace, name) for deterministic diffs")
	rootCmd.PersistentFlags().BoolVar(&excludeCRDs, "exclude-crds", false, "Strip CustomResourceDefinitions from output")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable debug logging")
	rootCmd.PersistentFlags().BoolVarP(&quiet, "quiet", "q", false, "Suppress informational output (only show errors)")
	rootCmd.PersistentFlags().BoolVar(&resolveGit, "resolve-git", false, "Clone external GitRepository sources to temp dirs")
	rootCmd.PersistentFlags().BoolVar(&sopsDecrypt, "sops-decrypt", false, "Decrypt SOPS-encrypted secrets")
	rootCmd.PersistentFlags().StringVar(&helmRegistryConfig, "registry-config", "", "Helm Registry Config")
	rootCmd.PersistentFlags().StringVar(&helmRepositoryConfig, "repository-config", "", "Helm Repository Config")
	rootCmd.PersistentFlags().StringVar(&helmRepositoryCache, "repository-cache", "", "Helm Repository Cache")
	rootCmd.PersistentFlags().StringVar(&configFile, "config", "", "Explicit repository configuration file")
	rootCmd.PersistentFlags().BoolVar(&crossplaneEnabled, "crossplane", false, "Enable Crossplane rendering (executes composition functions)")
	rootCmd.PersistentFlags().StringVar(&crossplaneEngine, "crossplane-engine", "", "Trusted external Crossplane v2.4.2 engine executable")
	rootCmd.PersistentFlags().StringArrayVar(&crossplaneFunctions, "crossplane-function", nil, "Trusted development target function=host:port (repeatable)")
	rootCmd.PersistentFlags().StringVar(&fluxPlugin, "flux-plugin", "", "Trusted Flux plugin executable")
	rootCmd.PersistentFlags().StringVar(&crossplanePlugin, "crossplane-plugin", "", "Trusted Crossplane plugin executable")

	renderCmd := &cobra.Command{
		Use:   "render <path>",
		Short: "Render a single path",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := cliLogger()
			opts, err := buildOpts(cmd, log, args[0])
			if err != nil {
				return err
			}
			p, err := preview.New(opts...)
			if err != nil {
				return fmt.Errorf("error creating preview: %w", err)
			}
			defer func() {
				if err := p.Close(); err != nil {
					log.Error(err, "closing preview")
				}
			}()
			if outputFormat == "json" {
				return p.RenderJSON(cmd.Context(), args[0], &jsonOutput)
			}
			return p.Render(cmd.Context(), args[0], os.Stdout)
		},
	}

	renderCmd.Flags().StringVarP(&outputFormat, "output", "o", "yaml", "Output format (yaml or json)")
	diffCmd := &cobra.Command{
		Use:   "diff [<rev>|<source-a> <source-b>]",
		Short: "Diff git revisions or directories",
		Long: `Diff rendered manifests from git revisions or filesystem paths.

With no arguments, compares HEAD against the current dirty worktree.
With one argument, compares that git revision against the current worktree.
With two arguments, existing filesystem paths are treated as directory inputs,
and valid git revisions are treated as revision inputs. For mixed or ambiguous
inputs, use explicit git: or path: prefixes.`,
		Example: `  fmp diff
  fmp diff HEAD~1
  fmp diff main feature-branch
  fmp diff ./before ./after
  fmp diff git:HEAD path:/tmp/rendered`,
		Args: validateDiffArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := cliLogger()
			if diffHTML {
				return runDiffHTML(cmd, log, args)
			}
			if outputFormat == "json" {
				return runDiffJSON(cmd, log, args, &jsonOutput)
			}
			summaryOut, diffOut := diffOutputs(os.Stdout, os.Stderr)
			return runDiff(cmd, log, args, summaryOut, diffOut)
		},
	}
	diffCmd.Flags().StringVarP(&outputFormat, "output", "o", "yaml", "Output format (yaml or json)")
	diffCmd.Flags().BoolVar(&diffExitCode, "exit-code", false, "Exit 1 when a complete diff has changes (default: exit 0)")
	diffCmd.Flags().StringVar(&helmRelease, "hr", "", "Filter diff to a specific HelmRelease by name")
	diffCmd.Flags().BoolVar(&diffSummary, "summary", false, "Print a human-readable change summary to stderr before the raw diff")
	diffCmd.Flags().BoolVar(&diffSummaryOnly, "summary-only", false, "Print only the human-readable summary and suppress the raw diff")
	diffCmd.Flags().BoolVar(&diffHTML, "html", false, "Generate an HTML report and open it in the browser")
	diffCmd.Flags().BoolVar(&diffHTMLOpen, "html-open", true, "Open the HTML report in the browser (use --html-open=false to suppress)")
	diffCmd.Flags().BoolVar(&aiAssessment, "ai-assessment", false, "Enable optional AI assessment for rendered manifest changes")
	diffCmd.Flags().StringVar(&aiProvider, "ai-provider", "", "AI provider to use (openai, anthropic, zai, minimax, openrouter)")
	diffCmd.Flags().StringVar(&aiModel, "ai-model", "", "AI model override")
	diffCmd.Flags().BoolVar(&aiFailOnError, "ai-fail-on-error", false, "Fail when AI assessment cannot be generated")
	testCmd := &cobra.Command{
		Use:   "test <path>",
		Short: "Validate all Kustomizations build and HelmReleases render",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := cliLogger()
			opts, err := buildOpts(cmd, log, args[0])
			if err != nil {
				return err
			}
			p, err := preview.New(opts...)
			if err != nil {
				return fmt.Errorf("error creating preview: %w", err)
			}
			defer func() {
				if err := p.Close(); err != nil {
					log.Error(err, "closing preview")
				}
			}()
			if outputFormat == "json" {
				result, err := p.TestJSON(cmd.Context(), args[0])
				enc := json.NewEncoder(&jsonOutput)
				enc.SetIndent("", "  ")
				if encErr := enc.Encode(result); encErr != nil {
					return encErr
				}
				return err
			}
			return p.Test(cmd.Context(), args[0], os.Stderr)
		},
	}
	testCmd.Flags().StringVarP(&outputFormat, "output", "o", "yaml", "Output format (yaml or json)")

	getCmd := &cobra.Command{
		Use:   "get",
		Short: "List discovered Flux resources",
	}
	getKSCmd := &cobra.Command{
		Use:   "ks <path>",
		Short: "List Flux Kustomizations",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := cliLogger()
			opts, err := buildOpts(cmd, log, args[0])
			if err != nil {
				return err
			}
			p, err := preview.New(opts...)
			if err != nil {
				return fmt.Errorf("error creating preview: %w", err)
			}
			defer func() {
				if err := p.Close(); err != nil {
					log.Error(err, "closing preview")
				}
			}()
			ks, err := p.ListKustomizations(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if outputFormat == "json" {
				enc := json.NewEncoder(&jsonOutput)
				enc.SetIndent("", "  ")
				return enc.Encode(preview.KustomizationsToJSON(ks))
			}
			preview.PrintKustomizations(ks, os.Stdout)
			return nil
		},
	}
	getHRCmd := &cobra.Command{
		Use:   "hr <path>",
		Short: "List HelmReleases",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := cliLogger()
			opts, err := buildOpts(cmd, log, args[0])
			if err != nil {
				return err
			}
			p, err := preview.New(opts...)
			if err != nil {
				return fmt.Errorf("error creating preview: %w", err)
			}
			defer func() {
				if err := p.Close(); err != nil {
					log.Error(err, "closing preview")
				}
			}()
			hrs, err := p.ListHelmReleases(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if outputFormat == "json" {
				enc := json.NewEncoder(&jsonOutput)
				enc.SetIndent("", "  ")
				return enc.Encode(preview.HelmReleasesToJSON(hrs))
			}
			preview.PrintHelmReleases(hrs, os.Stdout)
			return nil
		},
	}
	getCmd.PersistentFlags().StringVarP(&outputFormat, "output", "o", "yaml", "Output format (yaml or json)")
	getCmd.AddCommand(getKSCmd, getHRCmd)

	ciCmd := &cobra.Command{
		Use:   "ci",
		Args:  cobra.NoArgs,
		Short: "Run CI diff using environment variables",
		Long: `Run CI diff using environment variables.
Auto-discovers .gitops-preview.yaml or legacy .fmp.yaml from FMP_REPO_A (base branch).
FMP_CONFIG can point to an explicit config file (overrides auto-discovery).
CLI flags and FMP_* env vars override the config file.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			log := cliLogger()
			repoA := os.Getenv("FMP_REPO_A")
			repoB := os.Getenv("FMP_REPO_B")
			if repoA == "" || repoB == "" {
				return fmt.Errorf("must set both FMP_REPO_A and FMP_REPO_B environment variables")
			}

			if ks := os.Getenv("FMP_KUSTOMIZATIONS"); ks != "" {
				kustomizations = parseLines(ks)
			}
			if v := os.Getenv("FMP_RENDER_HELM"); v != "" {
				renderHelm = v == "true" || v == "1"
			}

			configRepo := repoA
			if explicitConfig := os.Getenv("FMP_CONFIG"); explicitConfig != "" {
				configFile = explicitConfig
				configRepo = ""
			}

			opts, err := buildOpts(cmd, log, configRepo)
			if err != nil {
				return err
			}

			if v := os.Getenv("FMP_FILTER"); v != "" {
				opts = append(opts, preview.WithFilterYAML(v))
			}

			p, err := preview.New(opts...)
			if err != nil {
				return fmt.Errorf("error creating preview: %w", err)
			}
			defer func() {
				if err := p.Close(); err != nil {
					log.Error(err, "closing preview")
				}
			}()
			return p.Diff(cmd.Context(), repoA, repoB, os.Stdout)
		},
	}
	versionCmd := &cobra.Command{
		Use:   "version",
		Args:  cobra.NoArgs,
		Short: "Print tool version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("%s %s\n", name, version)
		},
	}

	detectCmd := &cobra.Command{
		Use:   "detect-permadiffs <path>",
		Short: "Detect non-deterministic output and generate normalization filter config",
		Long: `Renders the same path twice and compares the results.
Any resource that differs between two renders of the same code is
non-deterministic (e.g. auto-generated TLS keys). Outputs a filter
config YAML that can be used with --filter to normalize these fields.

Use --init to generate a .gitops-preview.yaml config file in the repo.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			log := cliLogger()
			if initConfig {
				return generateInitConfig(cmd.Context(), args[0])
			}
			opts, err := buildOptsNoFilters(cmd, log, args[0])
			if err != nil {
				return err
			}
			p, err := preview.New(opts...)
			if err != nil {
				return fmt.Errorf("error creating preview: %w", err)
			}
			defer func() {
				if err := p.Close(); err != nil {
					log.Error(err, "closing preview")
				}
			}()
			return p.DetectPermadiffs(cmd.Context(), args[0], os.Stdout)
		},
	}
	detectCmd.Flags().BoolVar(&initConfig, "init", false, "Generate a .gitops-preview.yaml config file in the repo root")

	describeCmd := &cobra.Command{
		Use:    "describe",
		Args:   cobra.NoArgs,
		Short:  "Print JSON description of CLI and exit",
		Hidden: true,
		Run: func(cmd *cobra.Command, args []string) {
			desc := struct {
				Name        string           `json:"name"`
				Description string           `json:"description"`
				Version     string           `json:"version"`
				Commands    []map[string]any `json:"commands"`
			}{
				Name:        rootCmd.Name(),
				Description: rootCmd.Short,
				Version:     version,
			}
			for _, c := range rootCmd.Commands() {
				if c.Hidden {
					continue
				}
				cmdDesc := map[string]any{
					"name":        c.Name(),
					"description": c.Short,
					"usage":       c.UseLine(),
					"long":        c.Long,
					"flags":       describeFlags(c),
				}
				if len(c.Commands()) > 0 {
					subcommands := []map[string]any{}
					for _, sc := range c.Commands() {
						if sc.Hidden {
							continue
						}
						subcommands = append(subcommands, map[string]any{
							"name":        sc.Name(),
							"description": sc.Short,
							"usage":       sc.UseLine(),
							"flags":       describeFlags(sc),
						})
					}
					cmdDesc["commands"] = subcommands
				}
				desc.Commands = append(desc.Commands, cmdDesc)
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			_ = enc.Encode(desc)
		},
	}

	rootCmd.SilenceErrors = true
	rootCmd.SilenceUsage = true
	rootCmd.AddCommand(renderCmd, diffCmd, testCmd, getCmd, ciCmd, detectCmd, versionCmd, githubActionCmd(), describeCmd, agentCmd(), mcpCmd())

	rootCmd.SetContext(ctx)
	executed, err := rootCmd.ExecuteC()
	if err != nil {
		if executed != nil && (executed.Name() == "agent" || executed.Name() == "mcp") {
			if !errors.Is(err, errAgentHandled) {
				fmt.Fprintln(os.Stderr, err)
			}
			return 2
		}
		code := exitCodeFor(err)
		if outputFormat == "json" || requestsJSON(os.Args[1:]) {
			writeJSONFailure(os.Stdout, jsonOutput.Bytes(), err)
			return code
		}
		var expErr *preview.ExpansionError
		if errors.As(err, &expErr) {
			for _, w := range expErr.Warnings {
				fmt.Fprintf(os.Stderr, "WARNING: %v\n", w)
			}
			for _, e := range expErr.Errors {
				fmt.Fprintf(os.Stderr, "ERROR: %v\n", e)
			}
			return code
		}
		fmt.Fprintln(os.Stderr, err)
		return code
	}
	if jsonOutput.Len() > 0 {
		if _, err := jsonOutput.WriteTo(os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return 0
}

func describeFlags(cmd *cobra.Command) []map[string]any {
	flags := []map[string]any{}
	for _, set := range []*pflag.FlagSet{cmd.LocalFlags(), cmd.InheritedFlags()} {
		set.VisitAll(func(flag *pflag.Flag) {
			if !flag.Hidden {
				flags = append(flags, map[string]any{"name": flag.Name, "shorthand": flag.Shorthand, "type": flag.Value.Type(), "default": flag.DefValue, "description": flag.Usage})
			}
		})
	}
	return flags
}

// Inspect raw arguments too: Cobra can stop parsing before reaching --output.
func requestsJSON(args []string) bool {
	for i, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--output=json" || arg == "-o=json" || arg == "-ojson" ||
			((arg == "--output" || arg == "-o") && i+1 < len(args) && args[i+1] == "json") {
			return true
		}
	}
	return false
}

func writeJSONFailure(out io.Writer, data []byte, err error) {
	var doc map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if decoder.Decode(&doc) != nil || doc == nil {
		doc = map[string]any{"data": nil}
	}
	doc["status"] = "failure"
	if _, ok := doc["complete"]; !ok {
		doc["complete"] = false
	}
	doc["error"] = jsonError{Reason: "CommandFailed", Message: err.Error()}
	var expansionErr *preview.ExpansionError
	if _, exists := doc["warnings"]; !exists && errors.As(err, &expansionErr) {
		warnings := []string{}
		for _, warning := range expansionErr.Warnings {
			warnings = append(warnings, warning.Error())
		}
		doc["warnings"] = warnings
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	_ = enc.Encode(doc)
}

// Semantic exit codes for agent-friendly error handling.
//
//	0 = successful command (including differences unless --exit-code is set)
//	1 = differences with --exit-code, or any error not matched below
//	2 = errors wrapping ErrUserInput
//	3 = errors wrapping ErrDependency or containing preview.ExpansionError
//	5 = errors wrapping ErrPolicyViolation
var (
	ErrUserInput       = errors.New("user input error")
	ErrDependency      = errors.New("dependency failure")
	ErrPolicyViolation = errors.New("policy violation")
)

func exitCodeFor(err error) int {
	if errors.Is(err, ErrUserInput) {
		return 2
	}
	if errors.Is(err, ErrDependency) {
		return 3
	}
	if errors.Is(err, ErrPolicyViolation) {
		return 5
	}
	// Every ExpansionError maps to dependency failure.
	var expErr *preview.ExpansionError
	if errors.As(err, &expErr) {
		return 3
	}
	return 1
}

type jsonError struct {
	Reason  string         `json:"reason"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func cliLogger() logr.Logger {
	level := zerolog.WarnLevel
	if verbose {
		level = zerolog.DebugLevel
	}
	if quiet {
		level = zerolog.ErrorLevel
	}

	zl := zerolog.New(os.Stderr).Level(level)
	zl = zl.With().Caller().Timestamp().Logger().Output(zerolog.ConsoleWriter{Out: os.Stderr})
	return zerologr.New(&zl)
}

type resolvedSettings struct {
	paths        []string
	clusterPaths map[string][]string
	recursive    bool
	resolveGit   bool
	renderHelm   bool
	sort         bool
	excludeCRDs  bool
	sopsDecrypt  bool
	helmRelease  string
	helm         *config.HelmSettings
}

func optsFromSettings(log logr.Logger, s resolvedSettings) []preview.Opt {
	opts := []preview.Opt{preview.WithLogger(log)}
	if len(s.clusterPaths) > 0 {
		opts = append(opts, preview.WithClusterPaths(s.clusterPaths))
	} else {
		opts = append(opts, preview.WithPaths(s.paths, s.recursive))
	}
	if s.resolveGit {
		opts = append(opts, preview.WithGitRepo())
	}
	opts = append(opts, preview.WithFluxKS())
	if s.renderHelm {
		opts = append(opts, preview.WithHelm(s.helm))
	}
	if s.sort {
		opts = append(opts, preview.WithSort())
	}
	if s.excludeCRDs {
		opts = append(opts, preview.WithExcludeCRDs())
	}
	if s.sopsDecrypt {
		opts = append(opts, preview.WithSOPSDecrypt())
	}
	if s.helmRelease != "" {
		opts = append(opts, preview.WithHelmReleaseFilter(s.helmRelease))
	}
	return opts
}

func buildOpts(cmd *cobra.Command, log logr.Logger, configRepoPath string) ([]preview.Opt, error) {
	return buildOptsWithFilters(cmd, log, configRepoPath, true)
}

func buildOptsNoFilters(cmd *cobra.Command, log logr.Logger, configRepoPath string) ([]preview.Opt, error) {
	return buildOptsWithFilters(cmd, log, configRepoPath, false)
}

func buildOptsWithFilters(cmd *cobra.Command, log logr.Logger, configRepoPath string, applyFilters bool) ([]preview.Opt, error) {
	cfg, err := loadConfigForRepo(configRepoPath, configFile)
	if err != nil {
		if configFile != "" {
			return nil, fmt.Errorf("loading config %s: %w", configFile, err)
		}
		return nil, fmt.Errorf("loading config: %w", err)
	}

	s := resolvedSettings{
		paths:        kustomizations,
		recursive:    recursive,
		renderHelm:   renderHelm,
		resolveGit:   resolveGit,
		sort:         sortOutput,
		excludeCRDs:  excludeCRDs,
		sopsDecrypt:  sopsDecrypt,
		helmRelease:  helmRelease,
		clusterPaths: make(map[string][]string),
		helm:         helmSettings(),
	}

	if cfg != nil {
		if len(s.paths) == 0 && len(cfg.Paths) > 0 {
			s.paths = cfg.Paths
		}
		if cfg.ClusterPaths() != nil {
			s.clusterPaths = cfg.ClusterPaths()
		}
		if !flagChanged(cmd, "recursive") {
			s.recursive = config.BoolOr(cfg.Recursive, s.recursive)
		}
		if !flagChanged(cmd, "render-helm") {
			s.renderHelm = config.BoolOr(cfg.Helm, s.renderHelm)
		}
		if !flagChanged(cmd, "resolve-git") {
			s.resolveGit = config.BoolOr(cfg.ResolveGit, s.resolveGit)
		}
		if !flagChanged(cmd, "sort") {
			s.sort = config.BoolOr(cfg.Sort, s.sort)
		}
		if !flagChanged(cmd, "exclude-crds") {
			s.excludeCRDs = config.BoolOr(cfg.ExcludeCRDs, s.excludeCRDs)
		}
		if !flagChanged(cmd, "sops-decrypt") {
			s.sopsDecrypt = config.BoolOr(cfg.SOPSDecrypt, s.sopsDecrypt)
		}
		if cfg.HelmSettings != nil {
			s.helm = mergeHelmSettings(s.helm, cfg.HelmSettings)
		}
	}

	opts := optsFromSettings(log, s)
	pluginOpts, err := runtimePluginOpts(cfg)
	if err != nil {
		return nil, err
	}
	opts = append(opts, pluginOpts...)

	if applyFilters {
		if filtersFile != "" {
			data, err := os.ReadFile(filtersFile)
			if err != nil {
				return nil, fmt.Errorf("opening filter file: %w", err)
			}
			opts = append(opts, preview.WithFilterYAML(string(data)))
		} else if filterYAML != "" {
			opts = append(opts, preview.WithFilterYAML(filterYAML))
		} else if cfg != nil && len(cfg.Filters.Filters) > 0 {
			opts = append(opts, preview.WithFilterConfig(&cfg.Filters))
		}
	}

	return opts, nil
}

func flagChanged(cmd *cobra.Command, name string) bool {
	if cmd == nil {
		return false
	}
	f := cmd.Flags().Lookup(name)
	if f == nil {
		return false
	}
	return f.Changed
}

func generateInitConfig(ctx context.Context, repoPath string) (resultErr error) {
	dest := config.DiscoverConfigPath(repoPath)
	if dest != "" {
		return fmt.Errorf("config file already exists at %s (remove it first or edit manually)", dest)
	}

	dest = repoPath + "/.gitops-preview.yaml"

	opts, err := buildOpts(nil, logr.Discard(), repoPath)
	if err != nil {
		return err
	}

	p, err := preview.New(opts...)
	if err != nil {
		return fmt.Errorf("error creating preview: %w", err)
	}

	defer func() {
		resultErr = errors.Join(resultErr, p.Close())
	}()
	if err := p.GenerateInitConfig(ctx, repoPath, dest); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "Generated %s\n", dest)
	return nil
}

func helmSettings() *config.HelmSettings {
	settings := &config.HelmSettings{}
	if helmRepositoryConfig != "" {
		settings.RepositoryConfig = helmRepositoryConfig
	}
	if helmRegistryConfig != "" {
		settings.RegistryConfig = helmRegistryConfig
	}
	if helmRepositoryCache != "" {
		settings.RepositoryCache = helmRepositoryCache
	}
	return settings
}

func mergeHelmSettings(explicit, defaults *config.HelmSettings) *config.HelmSettings {
	settings := &config.HelmSettings{}
	if defaults != nil {
		*settings = *defaults
	}
	if explicit == nil {
		return settings
	}
	if explicit.RegistryConfig != "" {
		settings.RegistryConfig = explicit.RegistryConfig
	}
	if explicit.RepositoryConfig != "" {
		settings.RepositoryConfig = explicit.RepositoryConfig
	}
	if explicit.RepositoryCache != "" {
		settings.RepositoryCache = explicit.RepositoryCache
	}
	return settings
}

func parseLines(s string) []string {
	var result []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

func diffOutputs(stdout, stderr io.Writer) (io.Writer, io.Writer) {
	if diffSummaryOnly {
		return stdout, io.Discard
	}
	if diffSummary {
		return stderr, stdout
	}
	return io.Discard, stdout
}
