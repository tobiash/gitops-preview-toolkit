package build

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	fluxkustomize "github.com/fluxcd/pkg/apis/kustomize"
	"github.com/go-logr/logr"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

func TestFluxOverlayUsesKustomizeTransforms(t *testing.T) {
	fs := fluxTestFS(t, map[string]string{
		"/source/app/kustomization.yaml": `resources: [deployment.yaml]
namePrefix: old-
nameSuffix: -old
namespace: old
components: [../existing]
images:
- name: app
  newName: old-registry/app
  digest: sha256:old
patches:
- target: {kind: Deployment, name: app}
  patch: |-
    - op: replace
      path: /spec/replicas
      value: 2
`,
		"/source/app/deployment.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  labels: {app: example}
  annotations: {select: me}
spec:
  replicas: 1
  selector: {matchLabels: {app: example}}
  template:
    metadata: {labels: {app: example}}
    spec:
      containers:
      - name: app
        image: app:original
        envFrom:
        - configMapRef: {name: settings}
`,
		"/source/existing/kustomization.yaml": `apiVersion: kustomize.config.k8s.io/v1alpha1
kind: Component
configMapGenerator:
- name: settings
  literals: [key=value]
`,
		"/source/extra/kustomization.yaml": `apiVersion: kustomize.config.k8s.io/v1alpha1
kind: Component
patches:
- target: {kind: Deployment, name: app}
  patch: |-
    - op: add
      path: /metadata/annotations/component
      value: included
`,
	})
	b := New(logr.Discard())
	b.SetLocalOnly("/source")
	opts := FluxOptions{
		Namespace: "apps", NamePrefix: "new-", NameSuffix: "-new",
		Components: []string{"../extra"},
		Images:     []fluxkustomize.Image{{Name: "app", NewTag: "v2"}},
		Patches: []fluxkustomize.Patch{
			{Target: &fluxkustomize.Selector{Kind: "Deployment", LabelSelector: "app=example", AnnotationSelector: "select=me"},
				Patch: "- op: replace\n  path: /spec/replicas\n  value: 3"},
			{Patch: "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: app\nspec:\n  template:\n    spec:\n      containers:\n      - name: app\n        env:\n        - name: PATCHED\n          value: yes"},
		},
	}
	if err := b.AddFluxPathWithProducer(fs, "/source/app", "Kustomization flux/app", opts); err != nil {
		t.Fatalf("AddFluxPathWithProducer(combined transforms) = %v, want success", err)
	}
	var configMapName string
	for _, res := range b.Resources() {
		if res.GetKind() == "ConfigMap" {
			configMapName = res.GetName()
		}
	}
	if !strings.HasPrefix(configMapName, "new-settings-new-") {
		t.Fatalf("AddFluxPathWithProducer(component generator) name = %q, want prefixed/suffixed hashed name", configMapName)
	}
	for _, res := range b.Resources() {
		if res.GetNamespace() != "apps" {
			t.Errorf("AddFluxPathWithProducer(namespace) %s namespace = %q, want apps", res.GetKind(), res.GetNamespace())
		}
		if res.GetKind() != "Deployment" {
			continue
		}
		if res.GetName() != "new-app-new" {
			t.Errorf("AddFluxPathWithProducer(name overrides) name = %q, want new-app-new", res.GetName())
		}
		for _, want := range []string{"replicas: 3", "image: app:v2", "component: included", "name: PATCHED", "name: " + configMapName} {
			if !strings.Contains(res.MustYaml(), want) {
				t.Errorf("AddFluxPathWithProducer(combined transforms) missing %q in:\n%s", want, res.MustYaml())
			}
		}
	}
	data, err := fs.ReadFile("/source/app/kustomization.yaml")
	if err != nil || !strings.Contains(string(data), "namePrefix: old-") || strings.Contains(string(data), "new-") {
		t.Errorf("AddFluxPathWithProducer(source preservation) file = %s, %v, want original configuration", data, err)
	}
}

func TestFluxOverlayGeneratesRecursiveRawManifestConfiguration(t *testing.T) {
	fs := fluxTestFS(t, map[string]string{
		"/source/app/cm.yaml":                     "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: root}\n",
		"/source/app/nested/cm.yml":               "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: nested}\n",
		"/source/app/base/kustomization.yaml":     "resources: [cm.yaml]\nnameSuffix: -base\n",
		"/source/app/base/cm.yaml":                "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: child}\n",
		"/source/app/base/unreferenced/skip.yaml": "not a Kubernetes resource",
	})
	b := New(logr.Discard())
	b.SetLocalOnly("/source")
	if err := b.AddFluxPathWithProducer(fs, "/source/app", "Kustomization flux/app", FluxOptions{NamePrefix: "flux-"}); err != nil {
		t.Fatalf("AddFluxPathWithProducer(raw manifests) = %v, want success", err)
	}
	got := map[string]bool{}
	for _, res := range b.Resources() {
		got[res.GetName()] = true
	}
	for _, name := range []string{"flux-root", "flux-nested", "flux-child-base"} {
		if !got[name] {
			t.Errorf("AddFluxPathWithProducer(raw manifests) names = %v, want %s", got, name)
		}
	}
	if len(got) != 3 || fs.Exists("/source/app/kustomization.yaml") {
		t.Errorf("AddFluxPathWithProducer(raw manifests) names = %v, generated source file = %t, want 3 resources and no writes", got, fs.Exists("/source/app/kustomization.yaml"))
	}
}

func TestFluxOverlayRejectsInvalidInputs(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts FluxOptions
	}{
		{name: "invalid patch", opts: FluxOptions{Patches: []fluxkustomize.Patch{{Patch: "this is not a patch"}}}},
		{name: "unmatched strategic patch", opts: FluxOptions{Patches: []fluxkustomize.Patch{{Patch: "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: missing}\ndata: {value: patched}"}}}},
		{name: "invalid JSON operation", opts: FluxOptions{Patches: []fluxkustomize.Patch{{Target: &fluxkustomize.Selector{Kind: "ConfigMap"}, Patch: "- op: remove\n  path: /missing"}}}},
		{name: "missing component", opts: FluxOptions{Components: []string{"missing"}}},
		{name: "not a component", opts: FluxOptions{Components: []string{"../ordinary"}}},
		{name: "remote component", opts: FluxOptions{Components: []string{"https://example.invalid/component"}}},
		{name: "component escape", opts: FluxOptions{Components: []string{"../../outside"}}},
		{name: "component with remote resource", opts: FluxOptions{Components: []string{"../remote"}}},
		{name: "component exec", opts: FluxOptions{Components: []string{"../exec"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := fluxTestFS(t, map[string]string{
				"/source/app/cm.yaml":                 "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: app}\n",
				"/source/ordinary/kustomization.yaml": "resources: []\n",
				"/source/remote/kustomization.yaml":   "apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\nresources: [https://example.invalid/remote.yaml]\n",
				"/source/exec/kustomization.yaml":     "apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\ntransformers: [exec.yaml]\n",
				"/outside/kustomization.yaml":         "apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\nresources: []\n",
			})
			b := New(logr.Discard())
			b.SetLocalOnly("/source")
			if err := b.AddFluxPathWithProducer(fs, "/source/app", "Kustomization flux/app", tc.opts); err == nil {
				t.Errorf("AddFluxPathWithProducer(%s) = nil, want failure", tc.name)
			}
			if b.Size() != 0 {
				t.Errorf("AddFluxPathWithProducer(%s) size = %d, want no partial output", tc.name, b.Size())
			}
		})
	}
}

func TestFluxOverlayAllowsSiblingFilesWithinSource(t *testing.T) {
	fs := fluxTestFS(t, map[string]string{
		"/source/app/kustomization.yaml": "resources: [../cm.yaml]\n",
		"/source/cm.yaml":                "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: shared}\n",
	})
	b := New(logr.Discard())
	b.SetLocalOnly("/source")
	if err := b.AddFluxPathWithProducer(fs, "/source/app", "Kustomization flux/app", FluxOptions{NamePrefix: "flux-"}); err != nil {
		t.Fatalf("AddFluxPathWithProducer(in-source sibling file) = %v, want success", err)
	}
	if b.Size() != 1 || b.Resources()[0].GetName() != "flux-shared" {
		t.Errorf("AddFluxPathWithProducer(in-source sibling file) = %v, want flux-shared", b.Resources())
	}
}

func TestFluxOverlaySymlinkBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, link, target string
		wantError          bool
	}{
		{name: "in-source build alias", link: "alias", target: "app"},
		{name: "component escape", link: "component", target: "outside", wantError: true},
		{name: "configuration escape", link: "app/kustomization.yaml", target: "outside/kustomization.yaml", wantError: true},
		{name: "resource escape", link: "app/outside.yaml", target: "outside/cm.yaml", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			for _, dir := range []string{filepath.Join(root, "app"), filepath.Join(root, "component")} {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for path, data := range map[string]string{
				filepath.Join(root, "app/cm.yaml"):                  "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: app}\n",
				filepath.Join(root, "component/kustomization.yaml"): "apiVersion: kustomize.config.k8s.io/v1alpha1\nkind: Component\nnameSuffix: -component\n",
				filepath.Join(outside, "kustomization.yaml"):        "resources: [cm.yaml]\n",
				filepath.Join(outside, "cm.yaml"):                   "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: outside}\n",
			} {
				if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			target := filepath.Join(root, tc.target)
			if strings.HasPrefix(tc.target, "outside") {
				target = filepath.Join(outside, strings.TrimPrefix(tc.target, "outside"))
			}
			link := filepath.Join(root, tc.link)
			if tc.link == "component" {
				// Replace only this test's emptyable component fixture, not user data.
				if err := os.Remove(filepath.Join(link, "kustomization.yaml")); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "app")
			if tc.link == "alias" {
				path = link
			}
			b := New(logr.Discard())
			b.SetLocalOnly(root)
			err := b.AddFluxPathWithProducer(filesys.MakeFsOnDisk(), path, "Kustomization flux/app", FluxOptions{
				NamePrefix: "flux-", Components: []string{"../component"},
			})
			if (err != nil) != tc.wantError {
				t.Fatalf("AddFluxPathWithProducer(%s) = %v, want error=%t", tc.name, err, tc.wantError)
			}
			if !tc.wantError && (b.Size() != 1 || b.Resources()[0].GetName() != "flux-app-component") {
				t.Errorf("AddFluxPathWithProducer(in-source alias) = %v, want transformed flux-app-component", b.Resources())
			}
		})
	}
}

func fluxTestFS(t *testing.T, files map[string]string) filesys.FileSystem {
	t.Helper()
	fs := filesys.MakeFsInMemory()
	for path, data := range files {
		if err := fs.WriteFile(path, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	return fs
}
