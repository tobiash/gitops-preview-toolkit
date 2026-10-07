package fluxks

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"sigs.k8s.io/kustomize/api/hasher"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/resource"
)

func TestExpand_UsesTypedKustomizationFields(t *testing.T) {
	r := renderFromYAML(t, `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: apps
  namespace: flux-system
spec:
  path: ./clusters/prod
  prune: true
  targetNamespace: apps
  postBuild:
    substitute:
      APP: podinfo
  sourceRef:
    kind: GitRepository
    name: flux-system
`)

	exp := NewExpander(logr.Discard())
	result, err := exp.Expand(context.TODO(), r)
	if err != nil {
		t.Fatalf("Expand() error = %v", err)
	}
	if len(result.DiscoveredPaths) != 1 {
		t.Fatalf("expected one discovered path, got %d", len(result.DiscoveredPaths))
	}
	dp := result.DiscoveredPaths[0]
	if got, want := dp.Path, "clusters/prod"; got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
	if got, want := dp.Namespace, "apps"; got != want {
		t.Fatalf("Namespace = %q, want %q", got, want)
	}
	if got, want := dp.Producer, "Kustomization flux-system/apps"; got != want {
		t.Fatalf("Producer = %q, want %q", got, want)
	}
	if got, want := dp.Substitutions["APP"], "podinfo"; got != want {
		t.Fatalf("Substitutions[APP] = %q, want %q", got, want)
	}
}

func TestExpand_ResolvesGitRepositorySourceRef(t *testing.T) {
	r := renderFromYAML(t, `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: apps
  namespace: flux-system
spec:
  path: ./apps
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
`)

	exp := NewExpanderWithResolver(logr.Discard(), stubResolver{"flux-system/flux-system": "/tmp/source"})
	result, err := exp.Expand(context.TODO(), r)
	if err != nil {
		t.Fatalf("Expand() error = %v", err)
	}
	if len(result.DiscoveredPaths) != 1 {
		t.Fatalf("expected one discovered path, got %d", len(result.DiscoveredPaths))
	}
	if got, want := result.DiscoveredPaths[0].BaseDir, "/tmp/source"; got != want {
		t.Fatalf("BaseDir = %q, want %q", got, want)
	}
}

type stubResolver map[string]string

func (s stubResolver) ResolvePath(namespace, name string) (string, bool) {
	path, ok := s[namespace+"/"+name]
	return path, ok
}

func renderFromYAML(t *testing.T, yaml string) *render.Render {
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
