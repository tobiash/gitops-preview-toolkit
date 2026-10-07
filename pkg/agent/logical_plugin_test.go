package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
)

// A separately executed engine provides logical outputs through the production
// socket protocol. It also records session and process cleanup.
func runLogicalPluginProcess() bool {
	marker := slices.Index(os.Args, "agent-logical-plugin")
	if marker < 0 {
		return false
	}
	args := os.Args[marker+1:]
	if len(args) != 3 || args[1] != "--socket" {
		os.Exit(91)
	}
	engine := &logicalTestPlugin{marker: args[0], sessions: map[string]string{}}
	if err := plugin.Serve(context.Background(), args[2], engine); err != nil {
		os.Exit(92)
	}
	return true
}

type logicalTestPlugin struct {
	mu       sync.Mutex
	marker   string
	sessions map[string]string
	opens    []logicalOpen
	closed   int
}

type logicalOpen struct {
	PID      int
	Sequence int
	RunID    string
	Session  string
	Config   json.RawMessage
}

func (p *logicalTestPlugin) Describe(context.Context, *plugin.DescribeRequest) (*plugin.DescribeResponse, error) {
	return &plugin.DescribeResponse{ProtocolVersion: plugin.ProtocolVersion, Name: "flux", Version: "test"}, nil
}

func (p *logicalTestPlugin) OpenRender(_ context.Context, req *plugin.OpenRequest) (*plugin.OpenResponse, error) {
	var config struct {
		LocalOnly bool `json:"localOnly"`
	}
	if err := json.Unmarshal(req.Config, &config); err != nil {
		return nil, err
	}
	if req.LocalOnly != config.LocalOnly || !req.StrictInputs {
		return nil, errors.New("startup trust or strict inputs not propagated")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sessions) != 0 {
		return nil, errors.New("previous session not closed")
	}
	id := opaqueID()
	p.sessions[id] = req.Root
	p.opens = append(p.opens, logicalOpen{
		PID: os.Getpid(), Sequence: len(p.opens) + 1, RunID: req.RunID, Session: id, Config: req.Config,
	})
	data, err := json.Marshal(p.opens)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(p.marker+".opens", data, 0o600); err != nil {
		return nil, err
	}
	return &plugin.OpenResponse{Session: id}, nil
}

func (p *logicalTestPlugin) Expand(ctx context.Context, req *plugin.ExpandRequest) (*plugin.ExpandResponse, error) {
	p.mu.Lock()
	root := p.sessions[req.Session]
	p.mu.Unlock()
	value, err := os.ReadFile(filepath.Join(root, "value.txt"))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(value)) == "blocked" {
		if err := os.WriteFile(p.marker+".blocked", []byte("started"), 0o600); err != nil {
			return nil, err
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	resources := logicalSnapshot(strings.TrimSpace(string(value)), "a", "b").Logical["east"]
	result := &plugin.ExpandResponse{
		Expansions: []plugin.Expansion{{ID: "logical-outputs", Trigger: "root", Resources: resources}},
		Evidence:   []json.RawMessage{json.RawMessage(`{"ready":false}`)},
	}
	if strings.TrimSpace(string(value)) == "incomplete" {
		result.Diagnostics = []plugin.Diagnostic{{Code: "Incomplete", Severity: "error", Message: "fixture incomplete"}}
	}
	return result, nil
}

func (p *logicalTestPlugin) CloseRender(_ context.Context, req *plugin.CloseRequest) (*plugin.CloseResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.sessions, req.Session)
	p.closed++
	data, err := json.Marshal(p.closed)
	if err != nil {
		return nil, err
	}
	return &plugin.CloseResponse{}, os.WriteFile(p.marker+".session", data, 0o600)
}

func (p *logicalTestPlugin) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// An uncertain/canceled RPC retires the transport before CloseRender can
	// complete. Process shutdown must also release those outstanding sessions.
	p.sessions = map[string]string{}
	return os.WriteFile(p.marker+".process", []byte("closed"), 0o600)
}

func TestLogicalSubprocessRenderPreviewAndCleanup(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, trusted := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "trusted"}[trusted], func(t *testing.T) {
			root := t.TempDir()
			marker := filepath.Join(t.TempDir(), "cleanup")
			writeFixture(t, root, ".gitops-preview.yaml", "paths: [manifests]\n")
			writeFixture(t, root, "manifests/config.yaml", configMap)
			writeFixture(t, root, "value.txt", "old")
			configuration, err := json.Marshal(map[string]any{"localOnly": !trusted})
			if err != nil {
				t.Fatal(err)
			}
			s := newTestService(t, root, Options{
				Trusted: trusted,
				PluginCommands: []plugin.Command{{
					Name: "flux", Command: executable,
					Args: []string{"-test.run=^$", "--", "agent-logical-plugin", marker}, Config: configuration,
				}},
			})
			before := execute(t, s, Request{Operation: "render"}).Data.(RenderData)
			if before.ResourceCount != 2 {
				t.Fatalf("logical resources missing from render: %+v", before)
			}
			if _, err := os.Stat(marker + ".session"); err != nil {
				t.Fatalf("plugin session cleanup missing: %v", err)
			}
			if _, err := os.Stat(marker + ".process"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("plugin process stopped between operations: %v", err)
			}
			writeFixture(t, root, "value.txt", "new")
			writeFixture(t, root, ".gitops-preview.yaml", "paths: [manifests]\nhelm: false\nresolve-git: true\n")
			after := execute(t, s, Request{Operation: "render"}).Data.(RenderData)
			opens := readLogicalOpens(t, marker)
			if len(opens) != 2 || opens[0].PID != opens[1].PID || opens[1].Sequence != 2 {
				t.Fatalf("plugin process cache not retained: %+v", opens)
			}
			if opens[0].Session == opens[1].Session || opens[0].RunID == "" || opens[0].RunID == opens[1].RunID {
				t.Fatalf("render operations reused sessions or acquisition epochs: %+v", opens)
			}
			for i, open := range opens {
				var prefs struct {
					Helm       bool `json:"helm"`
					ResolveGit bool `json:"resolveGit"`
					FluxKS     bool `json:"fluxKS"`
				}
				if err := json.Unmarshal(open.Config, &prefs); err != nil {
					t.Fatal(err)
				}
				if prefs.Helm != (i == 0) || prefs.ResolveGit != (i == 1) || !prefs.FluxKS {
					t.Fatalf("per-operation preferences lost in shared host: %s", open.Config)
				}
			}
			comparison := execute(t, s, Request{Operation: "compare", BeforeID: before.ID, AfterID: after.ID}).Data.(PreviewData)
			if comparison.Summary.Modified != 2 {
				t.Fatalf("logical comparison: %+v", comparison)
			}
			page := execute(t, s, Request{Operation: "query", ID: comparison.ID}).Data.(QueryData)
			for _, item := range page.Items {
				inspection := execute(t, s, Request{Operation: "inspect", ID: comparison.ID, ResourceID: item.ResourceID}).Data.(InspectData)
				if inspection.LogicalID == "" || inspection.Name != "" || inspection.Old["spec"].(map[string]any)["value"] != "old" || inspection.New["spec"].(map[string]any)["value"] != "new" {
					t.Fatalf("logical diff inspection: %+v", inspection)
				}
			}
			unchanged := execute(t, s, Request{Operation: "preview", Base: "worktree"}).Data.(PreviewData)
			if unchanged.Changed || unchanged.Summary.Total != 0 {
				t.Fatalf("logical preview changed identical sources: %+v", unchanged)
			}
			opens = readLogicalOpens(t, marker)
			if len(opens) != 4 || opens[2].RunID != opens[3].RunID || opens[2].RunID == opens[1].RunID || opens[2].Session == opens[3].Session {
				t.Fatalf("comparison sides must share only an acquisition epoch: %+v", opens)
			}
			retained := len(s.entries)
			writeFixture(t, root, "value.txt", "incomplete")
			expectError(t, s, Request{Operation: "render"}, "Incomplete")
			expectError(t, s, Request{Operation: "preview", Base: "worktree"}, "Incomplete")
			if len(s.entries) != retained {
				t.Fatal("incomplete logical evaluation published handles")
			}
			data, err := os.ReadFile(marker + ".session")
			if err != nil {
				t.Fatal(err)
			}
			var closed int
			if err := json.Unmarshal(data, &closed); err != nil || closed != len(readLogicalOpens(t, marker)) {
				t.Fatalf("sessions leaked: closed=%d, err=%v", closed, err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(marker + ".process"); err != nil {
				t.Fatalf("service shutdown did not terminate plugin: %v", err)
			}
			if s.host != nil || s.bytes != 0 || len(s.entries) != 0 {
				t.Fatal("service shutdown retained host or facts")
			}
			expectError(t, s, Request{Operation: "render"}, "Closed")
		})
	}
}

func readLogicalOpens(t *testing.T, marker string) []logicalOpen {
	t.Helper()
	data, err := os.ReadFile(marker + ".opens")
	if err != nil {
		t.Fatal(err)
	}
	var opens []logicalOpen
	if err := json.Unmarshal(data, &opens); err != nil {
		t.Fatal(err)
	}
	return opens
}

func TestPersistentPluginPreservesStartupHelmSettingsAcrossOperations(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "helm-config")
	writeFixture(t, root, "manifests/config.yaml", configMap)
	writeFixture(t, root, "value.txt", "stable")
	want := config.HelmSettings{
		RegistryConfig:   "/trusted/helm/registry.json",
		RepositoryConfig: "/trusted/helm/repositories.yaml",
		RepositoryCache:  "/trusted/helm/cache",
	}
	startup, err := json.Marshal(map[string]any{"localOnly": false, "helmSettings": want})
	if err != nil {
		t.Fatal(err)
	}
	s := newTestService(t, root, Options{
		Trusted: true,
		PluginCommands: []plugin.Command{{
			Name: "flux", Command: executable,
			Args: []string{"-test.run=^$", "--", "agent-logical-plugin", marker}, Config: startup,
		}},
	})
	for i, tc := range []struct {
		name, helmPreference string
		enabled              bool
	}{
		{name: "default enabled", enabled: true},
		{name: "repeated enabled", helmPreference: "helm: true\n", enabled: true},
		{name: "disabled", helmPreference: "helm: false\n"},
		{name: "reenabled", helmPreference: "helm: true\n", enabled: true},
		{name: "disabled again", helmPreference: "helm: false\n"},
		{name: "default restored", enabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Repository paths cannot replace the trusted startup grants.
			writeFixture(t, root, ".gitops-preview.yaml", "paths: [manifests]\n"+tc.helmPreference+
				"helm-settings:\n  registry-config: /repository/registry.json\n"+
				"  repository-config: /repository/repositories.yaml\n  repository-cache: /repository/cache\n")
			execute(t, s, Request{Operation: "render"})
			opens := readLogicalOpens(t, marker)
			if len(opens) != i+1 || opens[i].Sequence != i+1 || opens[i].PID != opens[0].PID {
				t.Fatalf("operations did not reuse the persistent process: %+v", opens)
			}
			var effective struct {
				Helm         bool                 `json:"helm"`
				HelmSettings *config.HelmSettings `json:"helmSettings"`
			}
			if err := json.Unmarshal(opens[i].Config, &effective); err != nil {
				t.Fatal(err)
			}
			if effective.Helm != tc.enabled {
				t.Fatalf("effective helm=%t, want %t", effective.Helm, tc.enabled)
			}
			if effective.HelmSettings == nil || *effective.HelmSettings != want {
				t.Fatalf("startup Helm settings lost or replaced: got %+v, want %+v", effective.HelmSettings, want)
			}
			for _, previous := range opens[:i] {
				if previous.Session == opens[i].Session || previous.RunID == opens[i].RunID || opens[i].RunID == "" {
					t.Fatalf("operation reused a session or run: %+v", opens)
				}
			}
		})
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker + ".process"); err != nil {
		t.Fatalf("persistent process cleanup missing: %v", err)
	}
}

func TestCloseCancelsPersistentPluginAndWaitsForCleanup(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "cleanup")
	writeFixture(t, root, "manifests/config.yaml", configMap)
	writeFixture(t, root, "value.txt", "blocked")
	s := newTestService(t, root, Options{
		PluginCommands: []plugin.Command{{
			Name: "flux", Command: executable,
			Args:   []string{"-test.run=^$", "--", "agent-logical-plugin", marker},
			Config: json.RawMessage(`{"localOnly":true}`),
		}},
	})
	result := make(chan Response, 1)
	go func() {
		result <- s.Execute(context.Background(), Request{Operation: "render", Paths: []string{"manifests"}})
	}()
	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(marker + ".blocked"); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("plugin did not begin evaluation")
		case <-time.After(time.Millisecond):
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	response := <-result
	if response.Error == nil || response.Error.Code != "Canceled" {
		t.Fatalf("active operation not canceled: %+v", response)
	}
	if _, err := os.Stat(marker + ".process"); err != nil {
		t.Fatalf("Close returned before process cleanup: %v", err)
	}
}
