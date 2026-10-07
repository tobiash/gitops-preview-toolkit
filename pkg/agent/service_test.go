package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tobiash/gitops-preview-toolkit/pkg/preview"
)

func writeFixture(t *testing.T, root, name, data string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newTestService(t *testing.T, root string, opts Options) *Service {
	t.Helper()
	s, err := New(root, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func execute(t *testing.T, s *Service, req Request) Response {
	t.Helper()
	r := s.Execute(context.Background(), req)
	if r.Status != "success" || !r.Complete || r.Error != nil {
		t.Fatalf("Execute(%+v) = %+v, want complete success", req, r)
	}
	b, err := json.Marshal(r)
	if err != nil || len(b) > maxResponseBytes {
		t.Fatalf("response encoding: bytes=%d error=%v", len(b), err)
	}
	return r
}

func renderID(t *testing.T, s *Service) string {
	t.Helper()
	return execute(t, s, Request{Operation: "render", Paths: []string{"manifests"}}).Data.(RenderData).ID
}

func expectError(t *testing.T, s *Service, req Request, code string) {
	t.Helper()
	r := s.Execute(context.Background(), req)
	if r.Error == nil || r.Error.Code != code || r.Status != "failure" || r.Complete {
		t.Fatalf("Execute(%+v) = %+v, want %s failure", req, r, code)
	}
}

const configMap = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\n  labels:\n    app: test\ndata:\n  value: original\n  password: super-private\n"

func TestImmutableRenderQueryInspectCompare(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "manifests/config.yaml", configMap)
	s := newTestService(t, root, Options{})
	before := renderID(t, s)
	page := execute(t, s, Request{Operation: "query", ID: before}).Data.(QueryData)
	if len(page.Items) != 1 || page.Total != 1 {
		t.Fatalf("page = %+v", page)
	}
	resourceID := page.Items[0].ResourceID
	page.Items[0].AfterOrigin.Text = "caller mutation"
	writeFixture(t, root, "manifests/config.yaml", strings.Replace(configMap, "original", "updated", 1))
	old := execute(t, s, Request{Operation: "inspect", ID: before, ResourceID: resourceID}).Data.(InspectData)
	if old.New["data"].(map[string]any)["value"] != "original" {
		t.Fatal("snapshot followed mutable worktree")
	}
	if old.AfterOrigin.Text == "caller mutation" {
		t.Fatal("query leaked cached pointer")
	}
	old.New["data"].(map[string]any)["value"] = "caller mutation"
	after := renderID(t, s)
	comparison := execute(t, s, Request{Operation: "compare", BeforeID: before, AfterID: after}).Data.(PreviewData)
	if comparison.Summary.Modified != 1 || !comparison.Changed {
		t.Fatalf("comparison = %+v", comparison)
	}
	changes := execute(t, s, Request{Operation: "query", ID: comparison.ID, Query: Query{Action: "modified", Labels: "app=test"}}).Data.(QueryData)
	inspection := execute(t, s, Request{Operation: "inspect", ID: comparison.ID, ResourceID: changes.Items[0].ResourceID}).Data.(InspectData)
	if inspection.Old["data"].(map[string]any)["value"] != "original" || inspection.New["data"].(map[string]any)["value"] != "updated" {
		t.Fatalf("diff was mutated: %+v", inspection)
	}
	b, _ := json.Marshal(inspection)
	if strings.Contains(string(b), "super-private") {
		t.Fatal("credential leaked")
	}
}

func TestSecretRedactionAllProjections(t *testing.T) {
	root := t.TempDir()
	secret := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: private\n  annotations:\n    kubectl.kubernetes.io/last-applied-configuration: raw-credential\ndata:\n  harmless: c2VjcmV0\nstringData:\n  harmless: raw-credential\n"
	writeFixture(t, root, "manifests/secret.yaml", secret)
	s := newTestService(t, root, Options{})
	a := renderID(t, s)
	writeFixture(t, root, "manifests/secret.yaml", strings.ReplaceAll(secret, "raw-credential", "new-credential"))
	b := renderID(t, s)
	d := execute(t, s, Request{Operation: "compare", BeforeID: a, AfterID: b}).Data.(PreviewData)
	if !d.Changed {
		t.Fatal("raw secret changes must still be detected")
	}
	for _, id := range []string{a, b, d.ID} {
		page := execute(t, s, Request{Operation: "query", ID: id}).Data.(QueryData)
		for _, fields := range [][]string{nil, {""}, {"/data", "/stringData/harmless", "/metadata/annotations"}} {
			r := execute(t, s, Request{Operation: "inspect", ID: id, ResourceID: page.Items[0].ResourceID, Fields: fields})
			encoded, _ := json.Marshal(r)
			for _, secret := range []string{"raw-credential", "new-credential", "c2VjcmV0"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("secret leaked in %s", encoded)
				}
			}
		}
	}
}

func TestPaginationClusterIdentityAndFilters(t *testing.T) {
	root := t.TempDir()
	for i := range 25 {
		writeFixture(t, root, fmt.Sprintf("manifests/%02d.yaml", i), strings.Replace(configMap, "name: settings", fmt.Sprintf("name: settings-%02d", i), 1))
	}
	writeFixture(t, root, ".fmp.yaml", "clusters:\n  east: manifests\n  west: manifests\n")
	s := newTestService(t, root, Options{})
	id := execute(t, s, Request{Operation: "render"}).Data.(RenderData).ID
	p1 := execute(t, s, Request{Operation: "query", ID: id}).Data.(QueryData)
	if len(p1.Items) != 20 || p1.Total != 50 || p1.NextOffset == nil || *p1.NextOffset != 20 {
		t.Fatalf("page = %+v", p1)
	}
	p2 := execute(t, s, Request{Operation: "query", ID: id, Offset: *p1.NextOffset, Limit: 100}).Data.(QueryData)
	if len(p2.Items) != 30 || p2.Truncated {
		t.Fatalf("page2 = %+v", p2)
	}
	seen := map[string]bool{}
	for _, item := range append(p1.Items, p2.Items...) {
		if seen[item.ResourceID] {
			t.Fatal("cluster identity collision")
		}
		seen[item.ResourceID] = true
	}
	filtered := execute(t, s, Request{Operation: "query", ID: id, Query: Query{Cluster: "east", Name: "settings-00", Kind: "configmap", Labels: "app=test"}}).Data.(QueryData)
	if len(filtered.Items) != 1 {
		t.Fatalf("filtered = %+v", filtered)
	}
	expectError(t, s, Request{Operation: "query", ID: id, Limit: 101}, "InvalidInput")
	expectError(t, s, Request{Operation: "check", ID: id, Query: Query{Labels: "bad in ("}}, "InvalidInput")
	expectError(t, s, Request{Operation: "check", ID: id, Fields: []string{"/spec"}}, "InvalidInput")
	expectError(t, s, Request{Operation: "inspect", ID: id, ResourceID: p1.Items[0].ResourceID, Fields: []string{"/~2"}}, "InvalidInput")
}

func TestCaptureIsolationSymlinksAndBounds(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "manifests/config.yaml", configMap)
	writeFixture(t, root, "node_modules/large", strings.Repeat("x", 4096))
	s := newTestService(t, root, Options{MaxBytes: 1024})
	captured, diagnostics, err := s.capture(context.Background(), "worktree")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(captured); err != nil {
			t.Error(err)
		}
	})
	if len(diagnostics) != 1 {
		t.Fatalf("skips not reported: %+v", diagnostics)
	}
	writeFixture(t, root, "manifests/config.yaml", "changed")
	b, err := os.ReadFile(filepath.Join(captured, "manifests/config.yaml"))
	if err != nil || string(b) != configMap {
		t.Fatalf("capture changed: %s %v", b, err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	expectError(t, s, Request{Operation: "render", Paths: []string{"manifests"}}, "InvalidInput")
	expectError(t, s, Request{Operation: "render", Source: "path:../outside", Paths: []string{"."}}, "InvalidInput")
	expectError(t, s, Request{Operation: "render", Source: "path:escape", Paths: []string{"."}}, "InvalidInput")
}

func TestCapacityExpiryReleaseAndCancel(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "manifests/config.yaml", configMap)
	s := newTestService(t, root, Options{MaxSnapshots: 1})
	id := renderID(t, s)
	expectError(t, s, Request{Operation: "render", Paths: []string{"manifests"}}, "CapacityExceeded")
	execute(t, s, Request{Operation: "release", ID: id})
	expectError(t, s, Request{Operation: "query", ID: id}, "NotFound")
	id = renderID(t, s)
	s.entries[id].created = time.Now().Add(-s.opts.TTL)
	expectError(t, s, Request{Operation: "query", ID: id}, "NotFound")
	s.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := s.Execute(ctx, Request{Operation: "discover"})
	<-s.gate
	if r.Error == nil || r.Error.Code != "Canceled" {
		t.Fatalf("cancel response = %+v", r)
	}
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() { _ = s.Close() })
	}
	wg.Wait()
	expectError(t, s, Request{Operation: "discover"}, "Closed")
}

func TestDiscoverPermissionsAndIncomplete(t *testing.T) {
	root := t.TempDir()
	s := newTestService(t, root, Options{})
	d := execute(t, s, Request{Operation: "discover"}).Data.(DiscoverData)
	if d.Profile != "local" || d.ConfigFound || len(d.Operations) != 8 {
		t.Fatalf("discover = %+v", d)
	}
	expectError(t, s, Request{Operation: "render"}, "InvalidInput")
	writeFixture(t, root, ".fmp.yaml", "paths: [manifests]\nsops-decrypt: true\nai:\n  enabled: true\n")
	r := execute(t, s, Request{Operation: "discover"})
	if len(r.Diagnostics) != 2 {
		t.Fatalf("ignored sensitive config not reported: %+v", r)
	}
	expectError(t, s, Request{Operation: "render"}, "PermissionDenied")
	writeFixture(t, root, ".fmp.yaml", "paths: [manifests]\n")
	writeFixture(t, root, "manifests/bad.yaml", "apiVersion: [secret-error-value\n")
	r = s.Execute(context.Background(), Request{Operation: "render"})
	if r.Error == nil || r.Error.Code != "Incomplete" || len(s.entries) != 0 {
		t.Fatalf("incomplete = %+v", r)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "secret-error-value") {
		t.Fatal("raw error leaked")
	}
	writeFixture(t, root, ".fmp.yaml", "paths: [manifests]\npolicies:\n  inline: ['package fmp']\n")
	expectError(t, s, Request{Operation: "discover"}, "PermissionDenied")
	expectError(t, s, Request{Operation: "render"}, "PermissionDenied")
}

func TestChecksBudgetsAndReplicaLimit(t *testing.T) {
	root := t.TempDir()
	deployment := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  replicas: 2\n  template:\n    spec:\n      containers:\n      - name: app\n        image: app:1\n        resources:\n          requests:\n            cpu: 100m\n            memory: 64Mi\n"
	writeFixture(t, root, "manifests/app.yaml", deployment)
	s := newTestService(t, root, Options{})
	id := renderID(t, s)
	check := execute(t, s, Request{Operation: "check", ID: id, MaxCPURequests: "100m"}).Data.(CheckData)
	if check.Verdict != "failed" || check.Resources.Requests.CPU != "200m" || check.Count.Count != 1 {
		t.Fatalf("check = %+v", check)
	}
	expectError(t, s, Request{Operation: "check", ID: id, MaxCPURequests: "garbage"}, "InvalidInput")
	check = execute(t, s, Request{Operation: "check", ID: id, RequireLimits: true}).Data.(CheckData)
	if check.Verdict != "failed" {
		t.Fatal("missing limits passed")
	}
	writeFixture(t, root, "manifests/app.yaml", strings.Replace(deployment, "replicas: 2", "replicas: 9223372036854775807", 1))
	id = renderID(t, s)
	expectError(t, s, Request{Operation: "check", ID: id}, "AnalysisLimit")
}

func TestResponseLimitProjection(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "manifests/config.yaml", strings.Replace(configMap, "original", strings.Repeat("x", maxResponseBytes), 1))
	s := newTestService(t, root, Options{})
	id := renderID(t, s)
	page := execute(t, s, Request{Operation: "query", ID: id}).Data.(QueryData)
	expectError(t, s, Request{Operation: "inspect", ID: id, ResourceID: page.Items[0].ResourceID}, "ResponseTooLarge")
	execute(t, s, Request{Operation: "inspect", ID: id, ResourceID: page.Items[0].ResourceID, Fields: []string{"/metadata/name"}})
}

func TestPreviewPinnedGitAndBaseConfig(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, b, err)
		}
	}
	git("init", "--quiet")
	writeFixture(t, root, ".fmp.yaml", "paths: [manifests]\n")
	writeFixture(t, root, "manifests/config.yaml", configMap)
	git("add", ".")
	git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--quiet", "-m", "fixture")
	writeFixture(t, root, "manifests/config.yaml", strings.Replace(configMap, "original", "updated", 1))
	writeFixture(t, root, ".fmp.yaml", "paths: [nonexistent]\n")
	s := newTestService(t, root, Options{})
	d := execute(t, s, Request{Operation: "preview"}).Data.(PreviewData)
	if d.Summary.Modified != 1 || len(s.entries) != 3 {
		t.Fatalf("preview = %+v, handles %d", d, len(s.entries))
	}
	execute(t, s, Request{Operation: "check", ID: d.ID})
	expectError(t, s, Request{Operation: "render", Source: "git:--help", Paths: []string{"manifests"}}, "InvalidInput")
}

func TestBuiltinPolicyAndTrustedFrozenModules(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		t.Run(fmt.Sprintf("trusted=%t", trusted), func(t *testing.T) {
			root := t.TempDir()
			configuration := "paths: [manifests]\npolicies:\n  builtin: [secret_change]\n  fail-on: [secret_change]\n"
			if trusted {
				configuration = "paths: [manifests]\npolicies:\n  modules: [rules.rego]\n  fail-on: [custom]\n"
				writeFixture(t, root, "rules.rego", "package fmp\nimport rego.v1\nviolations contains {\"id\": \"custom\", \"message\": \"private policy value\"} if { count(input.changes) > 0 }\n")
			}
			writeFixture(t, root, ".fmp.yaml", configuration)
			writeFixture(t, root, "manifests/ns.yaml", "apiVersion: v1\nkind: Secret\nmetadata:\n  name: removed\n  namespace: default\nstringData:\n  key: private\n")
			s := newTestService(t, root, Options{Trusted: trusted})
			before := renderID(t, s)
			writeFixture(t, root, "manifests/ns.yaml", strings.Replace(configMap, "name: settings", "name: settings\n  namespace: default", 1))
			after := renderID(t, s)
			d := execute(t, s, Request{Operation: "compare", BeforeID: before, AfterID: after}).Data.(PreviewData)
			if trusted {
				writeFixture(t, root, "rules.rego", "invalid now")
			}
			r := execute(t, s, Request{Operation: "check", ID: d.ID})
			if r.Data.(CheckData).Verdict != "failed" {
				t.Fatalf("check = %+v", r)
			}
			b, _ := json.Marshal(r)
			if strings.Contains(string(b), "private policy value") {
				t.Fatal("policy message leaked")
			}
		})
	}
}

func TestSourceAndAggregateCapacity(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "manifests/config.yaml", configMap)
	s := newTestService(t, root, Options{MaxBytes: 64})
	expectError(t, s, Request{Operation: "render", Paths: []string{"manifests"}}, "CapacityExceeded")
	s = newTestService(t, root, Options{})
	id := renderID(t, s)
	s.opts.MaxBytes = s.bytes
	expectError(t, s, Request{Operation: "render", Paths: []string{"manifests"}}, "CapacityExceeded")
	if len(s.entries) != 1 || s.entries[id] == nil {
		t.Fatal("capacity rejection evicted existing handle")
	}
}

func TestLocalOnlyCannotFetchRemoteBases(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "manifests/kustomization.yaml", "resources:\n- https://example.invalid/secret-path\n")
	s := newTestService(t, root, Options{})
	expectError(t, s, Request{Operation: "render", Paths: []string{"manifests"}}, "Incomplete")
}

func TestRequestAndAdaptivePageBounds(t *testing.T) {
	root := t.TempDir()
	s := newTestService(t, root, Options{})
	expectError(t, s, Request{Operation: "query", ID: strings.Repeat("x", maxRequestBytes)}, "RequestTooLarge")
	// Long identity metadata can fill a response even without full objects.
	e := &entry{snapshot: &preview.Snapshot{Complete: true}}
	for i := range 20 {
		e.records = append(e.records, record{Summary: ResourceSummary{ResourceID: fmt.Sprint(i), Name: strings.Repeat("n", 8000)}})
	}
	if err := s.store(e); err != nil {
		t.Fatal(err)
	}
	p := execute(t, s, Request{Operation: "query", ID: e.id}).Data.(QueryData)
	if len(p.Items) >= 20 || len(p.Items) == 0 || p.NextOffset == nil || *p.NextOffset != len(p.Items) || !p.Truncated {
		t.Fatalf("adaptive page = %+v", p)
	}
}

func TestClusterScopedPolicyCannotSilentlyPass(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, ".fmp.yaml", "paths: [manifests]\npolicies:\n  builtin: [namespace_delete]\n  fail-on: [namespace_delete]\n")
	writeFixture(t, root, "manifests/ns.yaml", "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: removed\n")
	s := newTestService(t, root, Options{})
	before := renderID(t, s)
	writeFixture(t, root, "manifests/ns.yaml", configMap)
	after := renderID(t, s)
	d := execute(t, s, Request{Operation: "compare", BeforeID: before, AfterID: after}).Data.(PreviewData)
	check := execute(t, s, Request{Operation: "check", ID: d.ID}).Data.(CheckData)
	if check.Verdict != "failed" || check.ClassificationCount != 1 {
		t.Fatalf("namespace deletion check = %+v, want failed with one classification", check)
	}
}
