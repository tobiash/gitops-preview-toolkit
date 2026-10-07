package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tobiash/gitops-preview-toolkit/pkg/agent"
	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
)

func TestCrossplaneRuntimeRequiresInvocationGrant(t *testing.T) {
	cfg := &config.Config{Crossplane: &config.CrossplaneSettings{Enabled: config.BoolPtr(true)}}
	for _, tc := range []struct {
		name      string
		settings  runtimeSettings
		wantError bool
	}{
		{name: "repository intent alone"},
		{name: "engine without grant", settings: runtimeSettings{engine: "/trusted/engine"}, wantError: true},
		{name: "endpoint without grant", settings: runtimeSettings{functions: []string{"fn=localhost:9443"}}, wantError: true},
		{name: "plugin without grant", settings: runtimeSettings{crossplane: "/trusted/plugin"}, wantError: true},
		{name: "explicit opt-in", settings: runtimeSettings{enabled: true}},
		{name: "trusted endpoint", settings: runtimeSettings{enabled: true, functions: []string{"fn=[::1]:9443"}}},
		{name: "invalid endpoint", settings: runtimeSettings{enabled: true, functions: []string{"fn=localhost"}}, wantError: true},
		{name: "invalid port", settings: runtimeSettings{enabled: true, functions: []string{"fn=localhost:65536"}}, wantError: true},
		{name: "duplicate target", settings: runtimeSettings{enabled: true, functions: []string{"fn=localhost:9443", "fn=localhost:9444"}}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runtimePluginOptions(cfg, tc.settings)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v, wantError=%v", err, tc.wantError)
			}
			if err != nil && !errors.Is(err, ErrUserInput) {
				t.Fatalf("error does not classify invalid invocation: %v", err)
			}
		})
	}
}

func TestBothEntryPointsUseSharedCLIAndSiblingPlugin(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, "manifests/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: shared\n")
	writeFile(t, repo, ".gitops-preview.yaml", `paths: [manifests]
crossplane:
  enabled: true
  engine-binary: /untrusted/repository-engine
  development-targets:
    fn: untrusted.example:9443
plugins:
  - command: /untrusted/repository-plugin
`)
	var first map[string]any
	for _, name := range []string{"fmp", "gitops-preview"} {
		t.Run(name, func(t *testing.T) {
			binary := filepath.Join(testBinaryDir, name)
			cmd := exec.Command(binary, "version")
			out, err := cmd.CombinedOutput()
			if err != nil || string(out) != name+" dev\n" {
				t.Fatalf("version=%q error=%v", out, err)
			}
			cmd = exec.Command(binary, "render", repo, "-o", "json")
			// No plugin on PATH: the executable must find its installed sibling.
			cmd.Env = append(envWithout("PATH"), "PATH=/usr/bin:/bin")
			out, err = cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("render=%s error=%v", out, err)
			}
			var doc map[string]any
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(out, []byte(`"shared"`)) {
				t.Fatalf("missing rendered resource: %s", out)
			}
			if first == nil {
				first = doc
			} else if !reflect.DeepEqual(first, doc) {
				t.Fatalf("compatibility output differs: %#v versus %#v", first, doc)
			}
		})
	}
}

func TestPluginExecutableUsesTrustedExplicitSelection(t *testing.T) {
	if got := pluginExecutable("/trusted/plugin", "gitops-preview-flux"); got != "/trusted/plugin" {
		t.Fatalf("explicit executable=%q", got)
	}
}

func TestAgentStartupPluginCommands(t *testing.T) {
	resetGlobals()
	t.Cleanup(resetGlobals)
	crossplaneEnabled = true
	crossplaneEngine = "/trusted/engine"
	crossplaneFunctions = []string{"fn-a=localhost:9443", "fn-b=[::1]:9444"}
	fluxPlugin = "/trusted/flux"
	crossplanePlugin = "/trusted/crossplane"
	var opts agent.Options
	if err := configureAgentPlugins(&opts); !errors.Is(err, ErrUserInput) {
		t.Fatalf("Crossplane without --trusted: %v", err)
	}
	opts.Trusted = true
	if err := configureAgentPlugins(&opts); err != nil {
		t.Fatal(err)
	}
	if len(opts.PluginCommands) != 2 || opts.PluginCommands[0].Name != "flux" || opts.PluginCommands[0].Command != fluxPlugin {
		t.Fatalf("startup commands=%#v", opts.PluginCommands)
	}
	command := opts.PluginCommands[1]
	if command.Name != "crossplane" || command.Command != crossplanePlugin || opts.Crossplane || len(opts.CrossplaneConfig) != 0 {
		t.Fatalf("Crossplane command duplicated or executable lost: %#v", opts)
	}
	var cfg struct {
		EngineBinary string            `json:"engineBinary"`
		Targets      map[string]string `json:"developmentTargets"`
	}
	if err := json.Unmarshal(command.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.EngineBinary != crossplaneEngine || cfg.Targets["fn-a"] != "localhost:9443" || cfg.Targets["fn-b"] != "[::1]:9444" {
		t.Fatalf("trusted runtime settings lost: %s", command.Config)
	}
	// Reconfiguring a fresh invocation must remove earlier grants and commands.
	resetGlobals()
	if err := configureAgentPlugins(&opts); err != nil {
		t.Fatal(err)
	}
	if len(opts.PluginCommands) != 1 || opts.PluginCommands[0].Name != "flux" {
		t.Fatalf("earlier Crossplane grant leaked: %#v", opts)
	}
}

func TestJSONFailurePreservesStructuredWarnings(t *testing.T) {
	var out bytes.Buffer
	writeJSONFailure(&out, []byte(`{"warnings":[{"message":"warning"}],"errors":[{"message":"failed"}]}`), errors.New("failed"))
	var doc struct {
		Warnings []struct{ Message string }
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Warnings) != 1 || doc.Warnings[0].Message != "warning" {
		t.Fatalf("structured warnings changed: %s", &out)
	}
}

func TestHelmRepositorySettingsDoNotLeakBetweenCommands(t *testing.T) {
	resetGlobals()
	t.Cleanup(resetGlobals)
	repo := t.TempDir()
	writeFile(t, repo, ".gitops-preview.yaml", "helm-settings:\n  repository-cache: /repo/cache\n")
	if _, err := buildOpts(nil, cliLogger(), repo); err != nil {
		t.Fatal(err)
	}
	if settings := helmSettings(); settings.RepositoryCache != "" {
		t.Fatalf("repository settings leaked into invocation defaults: %#v", settings)
	}
	if err := os.Remove(filepath.Join(repo, ".gitops-preview.yaml")); err != nil {
		t.Fatal(err)
	}
}
