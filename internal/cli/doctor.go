package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
)

type doctorCheck struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

type doctorReport struct {
	Status   string        `json:"status"`
	Complete bool          `json:"complete"`
	Scope    string        `json:"scope"`
	Checks   []doctorCheck `json:"checks"`
}

func doctorCmd(jsonOut io.Writer) *cobra.Command {
	var format string
	cmd := &cobra.Command{
		Use: "doctor [repository]", Short: "Check first-run configuration and plugin prerequisites without rendering",
		Long: `Check local configuration, render roots and installed plugin protocol compatibility.
Starts trusted plugins for Describe only; never opens a render session, downloads
sources or executes composition functions. Crossplane engine discovery and version
checks require --crossplane. A successful preflight is not render validation.`,
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) > 1 {
				return fmt.Errorf("%w: doctor accepts at most one repository", ErrUserInput)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			repo := "."
			if len(args) == 1 {
				repo = args[0]
			}
			report, err := runDoctor(cmd.Context(), repo)
			if format == "json" {
				enc := json.NewEncoder(jsonOut)
				enc.SetIndent("", "  ")
				if writeErr := enc.Encode(report); writeErr != nil {
					return writeErr
				}
			} else if writeErr := writeDoctorText(cmd.OutOrStdout(), report); writeErr != nil {
				return writeErr
			}
			return err
		},
	}
	cmd.Flags().StringVarP(&format, "output", "o", "text", "Output format (text or json)")
	return cmd
}

func runDoctor(ctx context.Context, repo string) (doctorReport, error) {
	report := doctorReport{Status: "success", Complete: true, Scope: "preflight", Checks: []doctorCheck{}}
	var failures []error
	add := func(id, status, message, remediation string, kind error) {
		report.Checks = append(report.Checks, doctorCheck{
			ID: id, Status: status, Message: message, Remediation: remediation,
		})
		if status == "failure" {
			report.Status = "failure"
			failures = append(failures, fmt.Errorf("%w: %s", kind, id))
		}
	}
	checkDir := func(id, path string) {
		info, err := os.Stat(path)
		if err != nil {
			add(id, "failure", fmt.Sprintf("%s: %v", path, err), "Choose an existing directory with --path or correct paths/clusters in the config.", ErrUserInput)
		} else if !info.IsDir() {
			add(id, "failure", path+" is not a directory", "Choose a directory, not a manifest file.", ErrUserInput)
		} else {
			add(id, "success", path+" exists (contents not validated)", "", nil)
		}
	}
	checkDir("repository", repo)
	cfg, err := loadConfigForRepo(repo, configFile)
	if err != nil {
		add("config", "failure", err.Error(), "Fix the YAML or select an existing file with --config.", ErrUserInput)
	} else if cfg == nil {
		add("config", "warning", "No repository config discovered", "Create .gitops-preview.yaml with paths: [clusters/production], or pass --path.", nil)
	} else {
		add("config", "success", "Loaded "+cfg.SourcePath, "", nil)
	}
	paths := kustomizations
	if len(paths) == 0 && cfg != nil {
		paths = cfg.Paths
	}
	// Match buildOptsWithFilters: configured cluster roots take precedence over
	// flat paths (including --path). Preserve path order, sort map keys.
	roots := map[string][]string{}
	if cfg != nil {
		roots = cfg.ClusterPaths()
	}
	if len(roots) == 0 {
		roots = map[string][]string{}
		for _, path := range paths {
			cluster, path := config.ParseClusterPath(path)
			roots[cluster] = append(roots[cluster], path)
		}
	}
	clusters := make([]string, 0, len(roots))
	for cluster := range roots {
		clusters = append(clusters, cluster)
	}
	sort.Strings(clusters)
	count := 0
	for _, cluster := range clusters {
		for _, path := range roots[cluster] {
			count++
			id := fmt.Sprintf("root[%s:%d]", cluster, count)
			if path == "" {
				add(id, "failure", "Empty render root", "Set a non-empty paths/clusters entry or --path.", ErrUserInput)
				continue
			}
			checkDir(id, filepath.Join(repo, path))
		}
	}
	if count == 0 {
		add("roots", "failure", "No render roots configured; repository argument is not a render root", "Set paths/clusters in .gitops-preview.yaml or pass -k clusters/production (use -k . for the repository root).", ErrUserInput)
	}
	commands, err := runtimePluginCommands(cfg, runtimeSettings{
		enabled: crossplaneEnabled, engine: crossplaneEngine, functions: crossplaneFunctions,
		flux: fluxPlugin, crossplane: crossplanePlugin,
	})
	if err != nil {
		add("runtime", "failure", err.Error(), "Pass --crossplane with Crossplane runtime overrides; use function=host:port targets.", ErrUserInput)
	}
	for _, command := range commands {
		id := "plugin." + command.Name
		path, err := exec.LookPath(command.Command)
		if err != nil {
			add(id, "failure", fmt.Sprintf("Cannot find executable %q: %v", command.Command, err), "Install host and sibling plugins from the same release together on PATH, or select a trusted --"+command.Name+"-plugin executable.", ErrDependency)
			continue
		}
		command.Command = path
		client, err := plugin.StartWithOptions(ctx, command, plugin.Options{StartupTimeout: 5 * time.Second, ShutdownTimeout: time.Second})
		if err != nil {
			add(id, "failure", fmt.Sprintf("%s: %v", path, err), "Install matching host/plugin releases; check executable permissions and protocol compatibility.", ErrDependency)
			continue
		}
		if err := client.Close(); err != nil {
			add(id, "failure", fmt.Sprintf("%s: shutdown: %v", path, err), "Check plugin shutdown behavior; reinstall matching host/plugin releases.", ErrDependency)
			continue
		}
		add(id, "success", fmt.Sprintf("%s: name %s, protocol %d compatible (Describe only)", path, command.Name, plugin.ProtocolVersion), "", nil)
	}
	if crossplaneEnabled {
		engine := crossplaneEngine
		if engine == "" {
			engine = "crossplane-core"
		}
		path, err := exec.LookPath(engine)
		if err != nil {
			add("crossplane.engine", "failure", fmt.Sprintf("Cannot find controller engine %q: %v", engine, err), "Install the Crossplane controller v2.4.2 (not the CLI); select it with --crossplane-engine. See README prerequisites.", ErrDependency)
		} else {
			versionCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			versionCmd := exec.CommandContext(versionCtx, path, "--version")
			versionCmd.WaitDelay = time.Second
			out, err := versionCmd.Output()
			cancel()
			// Keep the lightweight host independent of renderer dependencies. This
			// version matches pkg/crossplanerender's pinned controller contract.
			if err != nil || strings.TrimSpace(string(out)) != "v2.4.2" {
				add("crossplane.engine", "failure", fmt.Sprintf("%s --version: got %q, error %v; expected v2.4.2", path, strings.TrimSpace(string(out)), err), "Install the pinned controller v2.4.2 and pass its trusted executable path.", ErrDependency)
			} else {
				add("crossplane.engine", "success", path+": controller v2.4.2", "", nil)
			}
		}
		add("crossplane.runtime", "warning", "Docker access, function images and development target reachability are not checked", "Default function runtime needs Docker access. Trusted --crossplane-function overrides use Development per function. Run test/render --crossplane to validate actual inputs (executes functions).", nil)
	} else {
		add("crossplane", "skipped", "Not explicitly enabled; repository intent does not grant execution", "Use --crossplane only from a trusted invocation to check its prerequisites.", nil)
	}
	add("render-validation", "skipped", "Preflight only: no render session, source/chart acquisition, input support validation or composition functions", "Run gitops-preview test <repository> or render <repository> with the same --config/--path/runtime flags for deeper validation; these may acquire sources and execute enabled functions.", nil)
	return report, errors.Join(failures...)
}

func writeDoctorText(out io.Writer, report doctorReport) error {
	if _, err := fmt.Fprintf(out, "gitops-preview doctor: %s (preflight only)\n", report.Status); err != nil {
		return err
	}
	for _, check := range report.Checks {
		if _, err := fmt.Fprintf(out, "[%s] %s: %s\n", check.Status, check.ID, check.Message); err != nil {
			return err
		}
		if check.Remediation != "" {
			if _, err := fmt.Fprintf(out, "  Next: %s\n", check.Remediation); err != nil {
				return err
			}
		}
	}
	return nil
}
