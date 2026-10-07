package fluxrender

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
)

func TestHelmRangeResolutionEpoch(t *testing.T) {
	t.Parallel()
	archive := func(version string) []byte {
		t.Helper()
		var buffer bytes.Buffer
		gz := gzip.NewWriter(&buffer)
		tw := tar.NewWriter(gz)
		files := map[string]string{
			"example/Chart.yaml":            "apiVersion: v2\nname: example\nversion: " + version + "\n",
			"example/templates/config.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: selected\ndata:\n  version: " + version + "\n",
		}
		for name, data := range files {
			if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data))}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(data)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		return buffer.Bytes()
	}
	archives := [][]byte{archive("0.1.0"), archive("0.1.1")}
	versions := []string{"0.1.0", "0.1.1"}
	var selected, downloads, indexes atomic.Int32
	var unavailable atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if unavailable.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/index.yaml" {
			indexes.Add(1)
			v := versions[selected.Load()]
			if _, err := fmt.Fprintf(w, "apiVersion: v1\nentries:\n  example:\n  - apiVersion: v2\n    name: example\n    version: %s\n    urls:\n    - example-%s.tgz\n", v, v); err != nil {
				t.Errorf("write chart index: %v", err)
			}
			return
		}
		for i, version := range versions {
			if r.URL.Path == "/example-"+version+".tgz" {
				downloads.Add(1)
				_, _ = w.Write(archives[i])
				return
			}
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	s := newService(t)
	cache := t.TempDir()
	writeFixture(t, cache, "repositories.yaml", "apiVersion: v1\nrepositories: []\n")
	config, err := json.Marshal(Config{Helm: true, HelmSettings: HelmSettings{
		RepositoryCache: cache, RepositoryConfig: filepath.Join(cache, "repositories.yaml"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	repo := input(t, fmt.Sprintf("apiVersion: source.toolkit.fluxcd.io/v1\nkind: HelmRepository\nmetadata:\n  name: source\n  namespace: flux-system\nspec:\n  url: %s\n", server.URL))
	hr := input(t, strings.ReplaceAll(strings.ReplaceAll(helmInput, "kind: GitRepository", "kind: HelmRepository"),
		"chart: ./chart", "chart: example\n      version: '>=0.1.0 <0.2.0'"))
	render := func(runID, version string) {
		t.Helper()
		opened, err := s.OpenRender(t.Context(), &plugin.OpenRequest{Root: t.TempDir(), RunID: runID, Config: config})
		if err != nil {
			t.Fatal(err)
		}
		for range 2 {
			r := expandSession(t, s, opened.Session, repo, hr)
			requireClean(t, r)
			if len(outputs(r)) != 1 || !strings.Contains(outputs(r)[0].YAML, "version: "+version) {
				t.Fatalf("want selected chart %s, got %+v", version, r)
			}
		}
		if _, err := s.CloseRender(t.Context(), &plugin.CloseRequest{Session: opened.Session}); err != nil {
			t.Fatal(err)
		}
	}
	render("review-1", "0.1.0")
	selected.Store(1)
	render("review-1", "0.1.0")
	if indexes.Load() != 1 || downloads.Load() != 1 {
		t.Fatalf("same run reacquired selector: indexes=%d downloads=%d", indexes.Load(), downloads.Load())
	}
	render("review-2", "0.1.1")
	if indexes.Load() != 2 || downloads.Load() != 2 {
		t.Fatalf("new run failed to resolve selector: indexes=%d downloads=%d", indexes.Load(), downloads.Load())
	}
	unavailable.Store(true)
	opened, err := s.OpenRender(t.Context(), &plugin.OpenRequest{Root: t.TempDir(), RunID: "review-3", Config: config})
	if err != nil {
		t.Fatal(err)
	}
	failed := expandSession(t, s, opened.Session, repo, hr)
	if len(outputs(failed)) != 0 || len(failed.Diagnostics) == 0 {
		t.Fatalf("expected acquisition failure: %+v", failed)
	}
	unavailable.Store(false)
	recovered := expandSession(t, s, opened.Session, repo, hr)
	requireClean(t, recovered)
	if len(outputs(recovered)) != 1 {
		t.Fatalf("transient error was pinned in the session memo: %+v", recovered)
	}
}

func TestGitBranchResolutionEpochAndCleanup(t *testing.T) {
	t.Parallel()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	remoteRoot, work := t.TempDir(), t.TempDir()
	command := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), git, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
	}
	command(remoteRoot, "init", "--bare", "--initial-branch=main", "remote.git")
	command(work, "init", "--initial-branch=main")
	command(work, "remote", "add", "origin", filepath.Join(remoteRoot, "remote.git"))
	publish := func(value string) {
		t.Helper()
		writeFixture(t, work, "manifests/object.yaml", fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: branch\ndata:\n  value: %s\n", value))
		command(work, "add", "manifests/object.yaml")
		command(work, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", value)
		command(work, "push", "origin", "main")
	}
	publish("before")
	server := httptest.NewServer(&cgi.Handler{Path: git, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + remoteRoot, "GIT_HTTP_EXPORT_ALL=1"}})
	defer server.Close()
	s := newService(t)
	config := json.RawMessage(`{"resolveGit":true}`)
	repo := input(t, fmt.Sprintf(`apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: moving
  namespace: flux-system
spec:
  url: %s/remote.git
  ref:
    branch: main
`, server.URL))
	ks := input(t, `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: branch
  namespace: flux-system
spec:
  path: manifests
  sourceRef:
    kind: GitRepository
    name: moving
`)
	open := func(runID string) string {
		t.Helper()
		response, err := s.OpenRender(t.Context(), &plugin.OpenRequest{Root: t.TempDir(), RunID: runID, Config: config})
		if err != nil {
			t.Fatal(err)
		}
		return response.Session
	}
	closeSession := func(id string) {
		t.Helper()
		for range 2 {
			if _, err := s.CloseRender(t.Context(), &plugin.CloseRequest{Session: id}); err != nil {
				t.Fatal(err)
			}
		}
	}
	render := func(id, value string) string {
		t.Helper()
		r := expandSession(t, s, id, repo, ks)
		requireClean(t, r)
		if len(outputs(r)) != 1 || !strings.Contains(outputs(r)[0].YAML, "value: "+value) {
			t.Fatalf("want branch %s, got %+v", value, r)
		}
		path, found := s.sessions[id].git.ResolvePath("flux-system", "moving")
		if !found {
			t.Fatal("acquired repository was not resolved")
		}
		return path
	}
	before := open("review-1")
	oldPath := render(before, "before")
	closeSession(before)
	publish("head")
	head := open("review-1")
	if path := render(head, "before"); path != oldPath {
		t.Fatal("same run resolved its branch again")
	}
	if _, err := s.OpenRender(t.Context(), &plugin.OpenRequest{Root: t.TempDir(), RunID: "review-2"}); err == nil {
		t.Fatal("overlapping named acquisition epochs accepted")
	}
	closeSession(head)
	next := open("review-2")
	newPath := render(next, "head")
	if newPath == oldPath {
		t.Fatal("new run reused the old selector snapshot")
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("retired epoch clone still exists: %v", err)
	}
	closeSession(next)
	standalone := open("")
	privatePath := render(standalone, "head")
	closeSession(standalone)
	if _, err := os.Stat(privatePath); !os.IsNotExist(err) {
		t.Fatalf("standalone clone was not cleaned on session close: %v", err)
	}
	publish("later")
	standalone = open("")
	render(standalone, "later")
	closeSession(standalone)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("retained run clone was not cleaned on service close: %v", err)
	}
}

func TestHelmMemoSurvivesSweepsWithRelevantInputsOnly(t *testing.T) {
	t.Parallel()
	s := newService(t)
	root := t.TempDir()
	chartFixture(t, root, "default")
	writeFixture(t, root, "chart/templates/result.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: rendered
data:
  value: {{ .Values.message | quote }}
  random: {{ randAlphaNum 64 | quote }}
`)
	id := openSession(t, s, root, nil, false)
	git := input(t, gitInput)
	hr := input(t, helmInput+`  valuesFrom:
    - kind: ConfigMap
      name: values
`)
	values := input(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: values
  namespace: flux-system
data:
  values.yaml: |
    message: first
`)
	first := expandSession(t, s, id, git, hr, values)
	requireClean(t, first)
	want := outputs(first)[0].YAML
	// Unused data keys and reconciliation metadata do not affect Helm values.
	values.YAML += "  unused: ignored\n"
	hr.YAML += "status:\n  observedGeneration: 100\n"
	git.YAML += "status:\n  artifact:\n    revision: metadata-only\n"
	second := expandSession(t, s, id, git, hr, values)
	requireClean(t, second)
	if outputs(second)[0].YAML != want {
		t.Fatal("irrelevant inputs regenerated random Helm output")
	}
	// A response belongs to its caller, not the session memo.
	second.Expansions[0].Resources[0].YAML = "tampered"
	third := expandSession(t, s, id, git, hr, values)
	requireClean(t, third)
	if outputs(third)[0].YAML != want {
		t.Fatal("caller mutated the session's cached output")
	}
	values.YAML = strings.ReplaceAll(values.YAML, "message: first", "message: changed")
	changed := expandSession(t, s, id, git, hr, values)
	requireClean(t, changed)
	if outputs(changed)[0].YAML == want || !strings.Contains(outputs(changed)[0].YAML, "value: changed") {
		t.Fatal("changed selected values failed to invalidate Helm output")
	}
	deleted := expandSession(t, s, id, git, values)
	requireClean(t, deleted)
	if len(deleted.Expansions) != 0 {
		t.Fatal("deleted target retained output")
	}
	values.YAML = strings.ReplaceAll(values.YAML, "message: changed", "message: first")
	recreated := expandSession(t, s, id, git, hr, values)
	requireClean(t, recreated)
	if outputs(recreated)[0].YAML == want {
		t.Fatal("recreated target reused its deleted evaluation memo")
	}
	if _, err := s.CloseRender(context.Background(), &plugin.CloseRequest{Session: id}); err != nil {
		t.Fatal(err)
	}
}
