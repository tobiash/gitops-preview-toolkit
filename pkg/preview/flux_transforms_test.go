package preview

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFluxTransformsThroughPreview(t *testing.T) {
	for _, recursive := range []bool{false, true} {
		t.Run(fmt.Sprintf("recursive=%t", recursive), func(t *testing.T) {
			root := t.TempDir()
			writePreviewFile(t, root, "root/ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: {name: app, namespace: flux}
spec:
  path: app
  targetNamespace: apps
  namePrefix: flux-
  nameSuffix: -prod
  components: [../component]
  images: [{name: app, newName: registry/app, newTag: v2}]
  patches:
  - target: {kind: Deployment, labelSelector: app=app}
    patch: |-
      - op: replace
        path: /spec/replicas
        value: 3
  commonMetadata:
    labels: {app: overridden, owner: flux}
    annotations: {after-substitution: '${VALUE}'}
  postBuild:
    substitute: {VALUE: substituted}
`)
			writePreviewFile(t, root, "app/nested/deployment.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  labels: {app: app}
  annotations: {before-substitution: '${VALUE}'}
spec:
  replicas: 1
  selector: {matchLabels: {app: app}}
  template:
    metadata: {labels: {app: app}}
    spec:
      containers: [{name: app, image: 'app:v1'}]
`)
			writePreviewFile(t, root, "component/kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1alpha1
kind: Component
configMapGenerator:
- name: component
  literals: [key=value]
`)
			p, err := newTestPreview(t, WithLocalOnly(), WithPaths([]string{"root"}, recursive), WithFluxKS())
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := p.RenderSnapshot(t.Context(), root)
			if err != nil || !snapshot.Complete {
				t.Fatalf("RenderSnapshot(Flux transforms) = %#v, %v, want complete", snapshot, err)
			}
			r := snapshot.Clusters[""]
			count := 0
			for _, res := range r.Resources() {
				if res.GetKind() == "Kustomization" {
					continue
				}
				count++
				if got := r.ProvenanceForID(res.CurId()).String(); got != "Kustomization flux/app" {
					t.Errorf("RenderSnapshot(transforms) %s provenance = %q, want Kustomization flux/app", res.GetKind(), got)
				}
				if res.GetNamespace() != "apps" || res.GetLabels()["app"] != "overridden" {
					t.Errorf("RenderSnapshot(metadata) %s = %s, want apps namespace and overridden label", res.GetKind(), res.MustYaml())
				}
				if got := res.GetAnnotations()["after-substitution"]; got != "${VALUE}" {
					t.Errorf("RenderSnapshot(commonMetadata timing) = %q, want literal ${VALUE}", got)
				}
				if res.GetKind() != "Deployment" {
					continue
				}
				if res.GetName() != "flux-app-prod" || res.GetAnnotations()["before-substitution"] != "substituted" {
					t.Errorf("RenderSnapshot(name/substitution) = %s, want flux-app-prod and substituted annotation", res.MustYaml())
				}
				for _, want := range []string{"replicas: 3", "image: registry/app:v2", "matchLabels:\n      app: app", "labels:\n        app: app"} {
					if !strings.Contains(res.MustYaml(), want) {
						t.Errorf("RenderSnapshot(patch/image/non-propagating metadata) missing %q in:\n%s", want, res.MustYaml())
					}
				}
			}
			if count != 2 {
				t.Errorf("RenderSnapshot(Flux transforms) generated resources = %d, want 2", count)
			}
			if _, err := os.Stat(filepath.Join(root, "app/kustomization.yaml")); !os.IsNotExist(err) {
				t.Errorf("RenderSnapshot(source preservation) stat = %v, want no generated file", err)
			}
		})
	}
}

func TestFluxTransformsReenterDiscovery(t *testing.T) {
	root := t.TempDir()
	writePreviewFile(t, root, "root/ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: {name: parent, namespace: flux}
spec:
  path: parent
  namePrefix: parent-
  patches:
  - target: {kind: Kustomization, name: child}
    patch: |-
      - op: add
        path: /spec/namePrefix
        value: child-
`)
	writePreviewFile(t, root, "parent/ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: {name: child, namespace: flux}
spec:
  path: child
`)
	writePreviewFile(t, root, "child/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: settings}\n")
	p, err := newTestPreview(t, WithLocalOnly(), WithPaths([]string{"root"}, false), WithFluxKS())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := p.RenderSnapshot(t.Context(), root)
	if err != nil || !snapshot.Complete {
		t.Fatalf("RenderSnapshot(nested transform discovery) = %#v, %v, want complete", snapshot, err)
	}
	for _, res := range snapshot.Clusters[""].Resources() {
		if res.GetKind() == "ConfigMap" {
			got := snapshot.Clusters[""].ProvenanceForID(res.CurId()).String()
			if res.GetName() != "child-settings" || got != "Kustomization flux/parent-child" {
				t.Errorf("RenderSnapshot(nested transform discovery) = %q, provenance %q, want child-settings from Kustomization flux/parent-child", res.GetName(), got)
			}
			return
		}
	}
	t.Fatal("RenderSnapshot(nested transform discovery) omitted the child's output")
}

func TestFluxTransformFailuresLeavePreviewIncomplete(t *testing.T) {
	for _, spec := range []string{
		"  patches: [{patch: 'not a patch'}]\n",
		"  components: [missing]\n",
		"  components: [https://example.invalid/component]\n",
	} {
		t.Run(spec, func(t *testing.T) {
			root := t.TempDir()
			writePreviewFile(t, root, "root/ks.yaml", "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata: {name: app, namespace: flux}\nspec:\n  path: app\n"+spec)
			writePreviewFile(t, root, "app/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: app}\n")
			p, err := newTestPreview(t, WithLocalOnly(), WithPaths([]string{"root"}, false), WithFluxKS())
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := p.RenderSnapshot(t.Context(), root)
			if err == nil || snapshot.Complete {
				t.Errorf("RenderSnapshot(%s) = %#v, %v, want incomplete with diagnostic", spec, snapshot, err)
			}
		})
	}
}

func TestFluxTransformContextsAreIndependent(t *testing.T) {
	root := t.TempDir()
	writePreviewFile(t, root, "app/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: settings}\n")
	for _, name := range []string{"first", "second"} {
		writePreviewFile(t, root, "root/"+name+".yaml", fmt.Sprintf("apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata: {name: %s, namespace: flux}\nspec:\n  path: app\n  namePrefix: %s-\n", name, name))
	}
	p, err := newTestPreview(t, WithLocalOnly(), WithPaths([]string{"root"}, false), WithFluxKS())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := p.RenderSnapshot(t.Context(), root)
	if err != nil || !snapshot.Complete {
		t.Fatalf("RenderSnapshot(independent transform contexts) = %#v, %v, want complete", snapshot, err)
	}
	names := map[string]bool{}
	for _, res := range snapshot.Clusters[""].Resources() {
		if res.GetKind() == "ConfigMap" {
			names[res.GetName()] = true
		}
	}
	if len(names) != 2 || !names["first-settings"] || !names["second-settings"] {
		t.Errorf("RenderSnapshot(independent transform contexts) names = %v, want first-settings and second-settings", names)
	}
}
