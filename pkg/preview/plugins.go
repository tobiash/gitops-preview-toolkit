package preview

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	"github.com/tobiash/gitops-preview-toolkit/pkg/pluginhost"
)

// WithPluginHost borrows persistent processes from the caller. Each render still
// opens and closes fresh sessions, and Close never closes the borrowed host.
// The caller owns host shutdown and must keep it alive throughout rendering.
// Executable selection belongs to the host; WithPlugins and WithCrossplane
// cannot be combined with this option.
func WithPluginHost(host *pluginhost.Host) Opt {
	return func(p *Preview) error {
		if host == nil {
			return fmt.Errorf("plugin host is nil")
		}
		p.host, p.borrowedHost = host, true
		return nil
	}
}

// WithPlugins selects executables from trusted invocation configuration. It
// replaces the default Flux command; it is never inferred from resource metadata.
func WithPlugins(commands []plugin.Command) Opt {
	return func(p *Preview) error {
		if len(commands) == 0 {
			return fmt.Errorf("at least one render plugin is required")
		}
		p.commands = cloneCommands(commands)
		return nil
	}
}

// WithCrossplane enables the Crossplane plugin with trusted runtime configuration.
// A nil configuration uses plugin defaults. Local-only restrictions still apply.
func WithCrossplane(configuration json.RawMessage) Opt {
	return func(p *Preview) error {
		if len(configuration) != 0 && !json.Valid(configuration) {
			return fmt.Errorf("invalid Crossplane plugin configuration")
		}
		p.crossplane = &plugin.Command{Name: "crossplane", Command: "gitops-preview-crossplane", Config: slices.Clone(configuration)}
		return nil
	}
}

// fluxConfiguration is serialized into the Flux command's Config. OpenRequest
// carries source, cluster, strict-input and acquisition policy separately.
type fluxConfiguration struct {
	FluxKS       bool                 `json:"fluxKS"`
	ResolveGit   bool                 `json:"resolveGit"`
	Helm         bool                 `json:"helm"`
	HelmSettings *config.HelmSettings `json:"helmSettings,omitempty"`
}

func (p *Preview) fluxConfig() (json.RawMessage, error) {
	settings := p.helmSettings
	if settings != nil && *settings == (config.HelmSettings{}) {
		// Enabling Helm does not replace trusted acquisition settings on a
		// borrowed host with an empty operation override.
		settings = nil
	}
	return json.Marshal(fluxConfiguration{
		FluxKS: p.fluxKSEnabled, ResolveGit: p.resolveGit,
		Helm: p.helmSettings != nil, HelmSettings: settings,
	})
}

// Only neutral operation preferences override the shared host's startup config.
// Runtime configuration for other engines remains exclusively host-owned.
func (p *Preview) sessionConfig() (json.RawMessage, error) {
	flux, err := p.fluxConfig()
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]json.RawMessage{"flux": flux})
}

func (p *Preview) pluginCommands() ([]plugin.Command, error) {
	commands := cloneCommands(p.commands)
	if len(commands) == 0 {
		commands = []plugin.Command{{Name: "flux", Command: "gitops-preview-flux"}}
	}
	for i := range commands {
		if commands[i].Name != "flux" {
			continue
		}
		cfg := map[string]json.RawMessage{}
		if len(commands[i].Config) != 0 {
			if err := json.Unmarshal(commands[i].Config, &cfg); err != nil || cfg == nil {
				return nil, fmt.Errorf("flux plugin configuration must be a JSON object")
			}
		}
		data, err := p.fluxConfig()
		if err != nil {
			return nil, err
		}
		var options map[string]json.RawMessage
		if err := json.Unmarshal(data, &options); err != nil {
			return nil, err
		}
		for key, value := range options {
			cfg[key] = value
		}
		commands[i].Config, err = json.Marshal(cfg)
		if err != nil {
			return nil, err
		}
	}
	if p.crossplane != nil {
		for _, command := range commands {
			if command.Name == p.crossplane.Name {
				return nil, fmt.Errorf("crossplane plugin already configured in WithPlugins")
			}
		}
		commands = append(commands, cloneCommands([]plugin.Command{*p.crossplane})[0])
	}
	return commands, nil
}

func cloneCommands(commands []plugin.Command) []plugin.Command {
	cloned := slices.Clone(commands)
	for i := range cloned {
		cloned[i].Args = slices.Clone(cloned[i].Args)
		cloned[i].Config = slices.Clone(cloned[i].Config)
	}
	return cloned
}
