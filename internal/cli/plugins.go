package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	"github.com/tobiash/gitops-preview-toolkit/pkg/preview"
)

var (
	crossplaneEnabled   bool
	crossplaneEngine    string
	crossplaneFunctions []string
	fluxPlugin          string
	crossplanePlugin    string
)

type runtimeSettings struct {
	enabled    bool
	engine     string
	functions  []string
	flux       string
	crossplane string
}

func runtimePluginOpts(cfg *config.Config) ([]preview.Opt, error) {
	return runtimePluginOptions(cfg, runtimeSettings{
		enabled: crossplaneEnabled, engine: crossplaneEngine, functions: crossplaneFunctions,
		flux: fluxPlugin, crossplane: crossplanePlugin,
	})
}

func runtimePluginOptions(cfg *config.Config, settings runtimeSettings) ([]preview.Opt, error) {
	commands, err := runtimePluginCommands(cfg, settings)
	if err != nil {
		return nil, err
	}
	return []preview.Opt{preview.WithPlugins(commands)}, nil
}

func runtimePluginCommands(cfg *config.Config, settings runtimeSettings) ([]plugin.Command, error) {
	commands := []plugin.Command{{Name: "flux", Command: pluginExecutable(settings.flux, "gitops-preview-flux")}}
	if !settings.enabled {
		if settings.engine != "" || len(settings.functions) > 0 || settings.crossplane != "" {
			return nil, fmt.Errorf("%w: Crossplane runtime options require --crossplane (Action input crossplane: true)", ErrUserInput)
		}
		return commands, nil
	}
	// Repository preferences cannot grant process execution or network targets.
	configuration := map[string]any{}
	if cfg != nil && cfg.Crossplane != nil {
		if cfg.Crossplane.Timeout != "" {
			configuration["timeout"] = cfg.Crossplane.Timeout
		}
		if cfg.Crossplane.MaxFunctions != 0 {
			configuration["maxFunctions"] = cfg.Crossplane.MaxFunctions
		}
	}
	if settings.engine != "" {
		configuration["engineBinary"] = settings.engine
	}
	targets := map[string]string{}
	for _, entry := range settings.functions {
		name, target, ok := strings.Cut(entry, "=")
		host, port, err := net.SplitHostPort(target)
		number, portErr := strconv.Atoi(port)
		if !ok || name == "" || err != nil || host == "" || portErr != nil || number < 1 || number > 65535 {
			return nil, fmt.Errorf("%w: --crossplane-function must be function=host:port, got %q", ErrUserInput, entry)
		}
		if _, exists := targets[name]; exists {
			return nil, fmt.Errorf("%w: duplicate Crossplane function %q", ErrUserInput, name)
		}
		targets[name] = target
	}
	if len(targets) > 0 {
		configuration["developmentTargets"] = targets
	}
	raw, err := json.Marshal(configuration)
	if err != nil {
		return nil, err
	}
	commands = append(commands, plugin.Command{
		Name: "crossplane", Command: pluginExecutable(settings.crossplane, "gitops-preview-crossplane"), Config: raw,
	})
	return commands, nil
}

// Explicit executables are trusted invocation settings. Defaults prefer siblings
// of the host binary and otherwise use normal PATH lookup by the transport.
func pluginExecutable(explicit, name string) string {
	if explicit != "" {
		return explicit
	}
	if executable, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(executable), name)
		if info, err := os.Stat(sibling); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return sibling
		}
	}
	return name
}

// Expose only the trusted installed host directory to child processes, never a
// repository/config directory.
func exposeSiblingPlugins() func() {
	previous := os.Getenv("PATH")
	if executable, err := os.Executable(); err == nil {
		_ = os.Setenv("PATH", filepath.Dir(executable)+string(os.PathListSeparator)+previous)
	}
	return func() { _ = os.Setenv("PATH", previous) }
}
