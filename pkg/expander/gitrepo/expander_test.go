package gitrepo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fluxgit "github.com/fluxcd/pkg/git"
	gitrepository "github.com/fluxcd/pkg/git/repository"
	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"sigs.k8s.io/kustomize/api/hasher"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/resource"
)

func TestExpand_UsesCurrentRepoForMatchingRemote(t *testing.T) {
	repo := t.TempDir()
	gitRun(t, repo, "init")
	gitRun(t, repo, "remote", "add", "origin", "https://github.com/tobiash/kube.git")

	exp, err := NewExpander(logr.Discard())
	if err != nil {
		t.Fatalf("NewExpander() error = %v", err)
	}
	defer exp.Cleanup()

	r := newRenderFromYAML(t, `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: flux-system
  namespace: flux-system
spec:
  url: ssh://git@github.com/tobiash/kube.git
`)

	if _, err := exp.WithSourceRoot(repo).Expand(context.TODO(), r); err != nil {
		t.Fatalf("Expand() error = %v", err)
	}

	path, ok := exp.WithSourceRoot(repo).ResolvePath("flux-system", "flux-system")
	if ok {
		t.Fatalf("unexpected resolve path from fresh scoped expander: %q", path)
	}

	scoped := exp.WithSourceRoot(repo)
	if _, err := scoped.Expand(context.TODO(), r); err != nil {
		t.Fatalf("Expand() error = %v", err)
	}
	path, ok = scoped.ResolvePath("flux-system", "flux-system")
	if !ok {
		t.Fatal("expected current repo to resolve without cloning")
	}
	if path != repo {
		t.Fatalf("resolved path = %q, want %q", path, repo)
	}
}

func TestExpand_ConcurrentSharedCloneUsesSingleClone(t *testing.T) {
	exp, err := NewExpander(logr.Discard())
	if err != nil {
		t.Fatalf("NewExpander() error = %v", err)
	}
	defer exp.Cleanup()

	r := newRenderFromYAML(t, `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: local-path-provisioner
  namespace: flux-system
spec:
  url: https://github.com/rancher/local-path-provisioner
`)

	originalClone := gitCloneFunc
	defer func() { gitCloneFunc = originalClone }()
	var cloneCalls atomic.Int32
	gitCloneFunc = func(_ context.Context, _ string, dest string, _ gitrepository.CloneConfig) error {
		cloneCalls.Add(1)
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return err
		}
		time.Sleep(25 * time.Millisecond)
		return os.WriteFile(filepath.Join(dest, "README"), []byte("ok\n"), 0o644)
	}

	scopedA := exp.WithSourceRoot(t.TempDir())
	scopedB := exp.WithSourceRoot(t.TempDir())

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for _, scoped := range []*Expander{scopedA, scopedB} {
		wg.Add(1)
		go func(scoped *Expander) {
			defer wg.Done()
			_, err := scoped.Expand(context.TODO(), r)
			errCh <- err
		}(scoped)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("Expand() error = %v", err)
		}
	}

	if got := cloneCalls.Load(); got != 1 {
		t.Fatalf("gitCloneFunc called %d times, want 1", got)
	}
	if _, ok := scopedA.ResolvePath("flux-system", "local-path-provisioner"); !ok {
		t.Fatal("expected first scoped expander to resolve shared clone")
	}
	if _, ok := scopedB.ResolvePath("flux-system", "local-path-provisioner"); !ok {
		t.Fatal("expected second scoped expander to resolve shared clone")
	}
}

func TestExpand_PassesRefAndSparseCheckoutToClone(t *testing.T) {
	exp, err := NewExpander(logr.Discard())
	if err != nil {
		t.Fatalf("NewExpander() error = %v", err)
	}
	defer exp.Cleanup()

	r := newRenderFromYAML(t, `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: app
  namespace: flux-system
spec:
  url: https://github.com/example/app
  recurseSubmodules: true
  sparseCheckout:
  - charts/app
  - deploy
  ref:
    branch: main
    commit: abc123
`)

	originalClone := gitCloneFunc
	defer func() { gitCloneFunc = originalClone }()
	called := make(chan gitrepository.CloneConfig, 1)
	gitCloneFunc = func(_ context.Context, _ string, dest string, cfg gitrepository.CloneConfig) error {
		called <- cfg
		return os.MkdirAll(dest, 0o755)
	}

	if _, err := exp.WithSourceRoot(t.TempDir()).Expand(context.Background(), r); err != nil {
		t.Fatalf("Expand() error = %v", err)
	}

	select {
	case cfg := <-called:
		if got, want := cfg.Branch, "main"; got != want {
			t.Fatalf("Branch = %q, want %q", got, want)
		}
		if !cfg.ShallowClone {
			t.Fatal("expected ShallowClone to be true")
		}
		if got, want := cfg.Commit, "abc123"; got != want {
			t.Fatalf("Commit = %q, want %q", got, want)
		}
		if !cfg.RecurseSubmodules {
			t.Fatal("expected RecurseSubmodules to be true")
		}
		if got, want := cfg.SparseCheckoutDirectories, []string{"charts/app", "deploy"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("SparseCheckoutDirectories = %#v, want %#v", got, want)
		}
	default:
		t.Fatal("expected clone config to be passed to gitCloneFunc")
	}
}

func TestCloneConfigForSpec_MapsReferenceFields(t *testing.T) {
	spec := map[string]any{
		"ref": map[string]any{
			"tag":    "v1.2.3",
			"semver": ">=1.0.0 <2.0.0",
			"name":   "refs/pull/1/head",
		},
	}

	cfg, err := cloneConfigForSpec(spec)
	if err != nil {
		t.Fatalf("cloneConfigForSpec() error = %v", err)
	}
	if got, want := cfg.Tag, "v1.2.3"; got != want {
		t.Fatalf("Tag = %q, want %q", got, want)
	}
	if got, want := cfg.SemVer, ">=1.0.0 <2.0.0"; got != want {
		t.Fatalf("SemVer = %q, want %q", got, want)
	}
	if got, want := cfg.RefName, "refs/pull/1/head"; got != want {
		t.Fatalf("RefName = %q, want %q", got, want)
	}
	if !cfg.ShallowClone {
		t.Fatal("expected ShallowClone to be true")
	}
}

func TestGitClone_UsesConfiguredClientFactory(t *testing.T) {
	originalFactory := newCloneClient
	defer func() { newCloneClient = originalFactory }()

	stub := &stubCloneClient{}
	newCloneClient = func(dest string, authOpts *fluxgit.AuthOptions) (cloneClient, error) {
		if dest == "" {
			t.Fatal("expected destination path")
		}
		if authOpts == nil {
			t.Fatal("expected auth options")
		}
		return stub, nil
	}

	cloneCfg := gitrepository.CloneConfig{CheckoutStrategy: gitrepository.CheckoutStrategy{Branch: "main"}}
	if err := gitClone(context.Background(), "https://github.com/example/app", t.TempDir(), cloneCfg); err != nil {
		t.Fatalf("gitClone() error = %v", err)
	}
	if !stub.closed {
		t.Fatal("expected clone client to be closed")
	}
	if got, want := stub.url, "https://github.com/example/app"; got != want {
		t.Fatalf("Clone url = %q, want %q", got, want)
	}
	if got, want := stub.cfg.Branch, "main"; got != want {
		t.Fatalf("Clone branch = %q, want %q", got, want)
	}
}

func TestGitClone_PropagatesClientCreationErrors(t *testing.T) {
	originalFactory := newCloneClient
	defer func() { newCloneClient = originalFactory }()

	newCloneClient = func(string, *fluxgit.AuthOptions) (cloneClient, error) {
		return nil, errors.New("boom")
	}

	err := gitClone(context.Background(), "https://github.com/example/app", t.TempDir(), gitrepository.CloneConfig{})
	if err == nil || !strings.Contains(err.Error(), "creating git client") {
		t.Fatalf("gitClone() error = %v, want client creation error", err)
	}
}

func newRenderFromYAML(t *testing.T, yaml string) *render.Render {
	t.Helper()
	r := render.NewDefaultRender(logr.Discard())
	factory := resmap.NewFactory(resource.NewFactory(&hasher.Hasher{}))
	rm, err := factory.NewResMapFromBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("NewResMapFromBytes() error = %v", err)
	}
	if err := r.AbsorbAll(rm); err != nil {
		t.Fatalf("AbsorbAll() error = %v", err)
	}
	return r
}

func gitRun(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmdArgs := append([]string{"-C", repo}, args...)
	cmd := exec.Command("git", cmdArgs...)
	cmd.Env = os.Environ()
	for i := len(cmd.Env) - 1; i >= 0; i-- {
		if strings.HasPrefix(cmd.Env[i], "GIT_DIR=") || strings.HasPrefix(cmd.Env[i], "GIT_WORK_TREE=") || strings.HasPrefix(cmd.Env[i], "GIT_INDEX_FILE=") {
			cmd.Env = append(cmd.Env[:i], cmd.Env[i+1:]...)
		}
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}

type stubCloneClient struct {
	url    string
	cfg    gitrepository.CloneConfig
	closed bool
}

func (s *stubCloneClient) Clone(_ context.Context, url string, cfg gitrepository.CloneConfig) (*fluxgit.Commit, error) {
	s.url = url
	s.cfg = cfg
	return nil, nil
}

func (s *stubCloneClient) Close() {
	s.closed = true
}
