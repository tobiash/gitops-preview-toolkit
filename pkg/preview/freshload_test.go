package preview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-logr/logr"
)

func TestFreshLoadRepo_PreservesGitRepositoryResolver(t *testing.T) {
	mainRepo := t.TempDir()
	externalRepo := t.TempDir()

	writePreviewFile(t, externalRepo, "apps/podinfo/configmap.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: from-external
data:
  key: value
`)
	writePreviewFile(t, mainRepo, "sources.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: podinfo
  namespace: default
spec:
  url: file://`+externalRepo+`
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: podinfo
  namespace: default
spec:
  path: ./apps/podinfo
  sourceRef:
    kind: GitRepository
    name: podinfo
`)

	p, err := newTestPreview(t,
		WithLogger(logr.Discard()),
		WithPaths([]string{"."}, false),
		WithGitRepo(),
		WithFluxKS(),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	results, err := p.freshLoadRepo(context.Background(), mainRepo)
	if err != nil {
		t.Fatalf("freshLoadRepo() error = %v", err)
	}

	result := results[""]
	yaml, err := result.render.AsYaml()
	if err != nil {
		t.Fatalf("AsYaml() error = %v", err)
	}
	if !strings.Contains(string(yaml), "name: from-external") {
		t.Fatalf("expected fresh load to include external GitRepository content, got:\n%s", string(yaml))
	}
}

func writePreviewFile(t *testing.T, dir, name, content string) {
	t.Helper()
	fullPath := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("creating directory for %s: %v", name, err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}
