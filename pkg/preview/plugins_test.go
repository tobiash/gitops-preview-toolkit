package preview

import (
	"encoding/json"
	"testing"

	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
)

func TestPluginOptionsCaptureNeutralConfiguration(t *testing.T) {
	settings := &config.HelmSettings{RepositoryCache: "/trusted/cache"}
	commands := []plugin.Command{{Name: "flux", Command: "/trusted/flux", Config: json.RawMessage(`{"custom":true}`)}}
	p, err := New(WithPlugins(commands), WithFluxKS(), WithGitRepo(), WithHelm(settings), WithCrossplane(json.RawMessage(`{"runtime":"trusted"}`)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	settings.RepositoryCache = "/mutated"
	commands[0].Command = "/mutated"
	got, err := p.pluginCommands()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Command != "/trusted/flux" || got[1].Name != "crossplane" {
		t.Fatalf("commands = %#v", got)
	}
	var cfg struct {
		fluxConfiguration
		Custom bool `json:"custom"`
	}
	if err := json.Unmarshal(got[0].Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.FluxKS || !cfg.ResolveGit || !cfg.Helm || !cfg.Custom || cfg.HelmSettings.RepositoryCache != "/trusted/cache" {
		t.Fatalf("neutral Flux configuration = %#v", cfg)
	}
}

func TestPluginOptionsRejectInvalidConfiguration(t *testing.T) {
	for _, tt := range []struct {
		name string
		opts []Opt
	}{
		{name: "no commands", opts: []Opt{WithPlugins(nil)}},
		{name: "invalid Crossplane JSON", opts: []Opt{WithCrossplane(json.RawMessage(`{`))}},
		{name: "Flux config array", opts: []Opt{WithPlugins([]plugin.Command{{Name: "flux", Config: json.RawMessage(`[]`)}})}},
		{name: "duplicate Crossplane", opts: []Opt{WithPlugins([]plugin.Command{{Name: "crossplane"}}), WithCrossplane(nil)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p, err := New(tt.opts...)
			if p != nil || err == nil {
				t.Fatalf("New = %v, %v; want configuration error", p, err)
			}
		})
	}
}
