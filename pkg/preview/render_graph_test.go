package preview

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/go-logr/logr"
)

func TestRenderGraphBuildContexts(t *testing.T) {
	dir := t.TempDir()
	writePreviewFile(t, dir, "apps/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\n  namespace: original\ndata:\n  value: ${VALUE}\n")
	for _, name := range []string{"a", "b"} {
		writePreviewFile(t, dir, "root/"+name+".yaml", fmt.Sprintf("apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: %s\n  namespace: flux\nspec:\n  path: apps\n  targetNamespace: %s\n  postBuild:\n    substitute:\n      VALUE: %s\n", name, name, name))
	}
	p, err := newTestPreview(t, WithLogger(logr.Discard()), WithPaths([]string{"root"}, false), WithFluxKS())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	loaded, err := p.loadRepo(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, res := range loaded[""].render.Resources() {
		if res.GetKind() != "ConfigMap" {
			continue
		}
		count++
		obj, err := res.Map()
		if err != nil {
			t.Fatal(err)
		}
		ns := res.GetNamespace()
		if ns != "a" && ns != "b" {
			t.Errorf("namespace = %q, want a or b", ns)
		}
		if got := obj["data"].(map[string]any)["value"]; got != ns {
			t.Errorf("substitution = %v, want %s", got, ns)
		}
		if got := loaded[""].render.ProvenanceForID(res.CurId()).String(); got != "Kustomization flux/"+ns {
			t.Errorf("provenance = %q", got)
		}
	}
	if count != 2 {
		t.Fatalf("ConfigMaps = %d, want 2 independent builds", count)
	}
}

func TestRenderGraphBootstrapAndAliasesThroughPreview(t *testing.T) {
	for _, tt := range []struct{ self, recursive bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("self=%t/recursive=%t", tt.self, tt.recursive), func(t *testing.T) {
			dir := t.TempDir()
			writePreviewFile(t, dir, "apps/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n")
			if tt.self {
				writePreviewFile(t, dir, "apps/ks.yaml", "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: self\n  namespace: flux-system\nspec:\n  path: ./apps\n")
			}
			p, err := newTestPreview(t, WithLogger(logr.Discard()), WithPaths([]string{"apps", "./apps", "apps/../apps"}, tt.recursive), WithFluxKS())
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := p.Test(context.Background(), dir, &out); err != nil {
				t.Fatalf("Test(bootstrap/aliases) = %v, output %s", err, &out)
			}
			run, err := p.RunDiff(context.Background(), DiffRunOptions{LeftPath: dir, RightPath: dir})
			if err != nil {
				t.Fatalf("RunDiff(bootstrap/aliases) = %v", err)
			}
			if !run.Complete {
				t.Fatal("RunDiff(bootstrap/aliases) incomplete")
			}
		})
	}
}

func TestRenderGraphConflictingProducersRemainIncomplete(t *testing.T) {
	dir := t.TempDir()
	writePreviewFile(t, dir, "apps/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n")
	for _, name := range []string{"first", "second"} {
		writePreviewFile(t, dir, "apps/"+name+".yaml", fmt.Sprintf("apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: %s\n  namespace: flux-system\nspec:\n  path: apps\n", name))
	}
	p, err := newTestPreview(t, WithLogger(logr.Discard()), WithPaths([]string{"apps"}, false), WithFluxKS())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Test(context.Background(), dir, &bytes.Buffer{}); err == nil {
		t.Fatal("Test(conflicting producers) succeeded, want incomplete")
	}
}

func TestRenderGraphRecursiveBootstrapChild(t *testing.T) {
	for _, recursive := range []bool{false, true} {
		t.Run(fmt.Sprintf("recursive=%t", recursive), func(t *testing.T) {
			dir := t.TempDir()
			writePreviewFile(t, dir, "tree/ks.yaml", "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: child\n  namespace: flux-system\nspec:\n  path: tree/apps\n")
			writePreviewFile(t, dir, "tree/apps/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n")
			p, err := newTestPreview(t, WithLogger(logr.Discard()), WithPaths([]string{"tree", "./tree"}, recursive), WithFluxKS())
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Test(context.Background(), dir, &bytes.Buffer{}); err != nil {
				t.Fatalf("Test(recursive child) = %v, want success", err)
			}
			run, err := p.RunDiff(context.Background(), DiffRunOptions{LeftPath: dir, RightPath: dir})
			if err != nil {
				t.Fatal(err)
			}
			if !run.Complete || run.Result.TotalChanged() != 0 || len(run.Warnings) != 0 {
				t.Fatalf("RunDiff(recursive child) = %#v, want complete unchanged result without warnings", run)
			}
		})
	}
}

func TestRenderGraphRecursiveBootstrapContexts(t *testing.T) {
	for _, tt := range []struct {
		name      string
		owners    []string
		context   string
		resource  string
		wantNames []string
		wantError bool
	}{
		{name: "conflicting owners", owners: []string{"first", "second"}, resource: "app", wantError: true},
		{name: "namespace", owners: []string{"child"}, context: "  targetNamespace: apps\n", resource: "app", wantNames: []string{"/app", "apps/app"}},
		{name: "substitution", owners: []string{"child"}, context: "  postBuild:\n    substitute:\n      NAME: resolved\n", resource: "${NAME}", wantNames: []string{"/${NAME}", "/resolved"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, owner := range tt.owners {
				writePreviewFile(t, dir, "tree/"+owner+".yaml", fmt.Sprintf("apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: %s\n  namespace: flux-system\nspec:\n  path: tree/apps\n%s", owner, tt.context))
			}
			writePreviewFile(t, dir, "tree/apps/cm.yaml", fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n", tt.resource))
			p, err := newTestPreview(t, WithLogger(logr.Discard()), WithPaths([]string{"tree"}, true), WithFluxKS())
			if err != nil {
				t.Fatal(err)
			}
			err = p.Test(context.Background(), dir, &bytes.Buffer{})
			if (err != nil) != tt.wantError {
				t.Fatalf("Test(%s) = %v, want error=%t", tt.name, err, tt.wantError)
			}
			if tt.wantError {
				return
			}
			loaded, err := p.loadRepo(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			names := make(map[string]bool)
			for _, res := range loaded[""].render.Resources() {
				if res.GetKind() == "ConfigMap" {
					names[res.GetNamespace()+"/"+res.GetName()] = true
				}
			}
			if len(names) != len(tt.wantNames) {
				t.Fatalf("ConfigMaps = %v, want %v", names, tt.wantNames)
			}
			for _, name := range tt.wantNames {
				if !names[name] {
					t.Errorf("ConfigMaps = %v, missing %s", names, name)
				}
			}
		})
	}
}

func TestRenderGraphRecursiveKustomizeBoundary(t *testing.T) {
	for _, target := range []string{"tree/apps", "tree/apps/raw"} {
		t.Run(target, func(t *testing.T) {
			dir := t.TempDir()
			writePreviewFile(t, dir, "tree/ks.yaml", fmt.Sprintf("apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: child\n  namespace: flux-system\nspec:\n  path: %s\n", target))
			writePreviewFile(t, dir, "tree/apps/kustomization.yaml", "resources:\n- raw/cm.yaml\nnamePrefix: built-\n")
			writePreviewFile(t, dir, "tree/apps/raw/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n")
			p, err := newTestPreview(t, WithLogger(logr.Discard()), WithPaths([]string{"tree"}, true), WithFluxKS())
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Test(context.Background(), dir, &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			loaded, err := p.loadRepo(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			want := 2 // Flux Kustomization plus the Kustomize-transformed ConfigMap.
			if target == "tree/apps/raw" {
				want++ // This directory was not independently built by bootstrap.
			}
			if got := loaded[""].render.Size(); got != want {
				t.Errorf("render size = %d, want %d", got, want)
			}
		})
	}
}

func TestRenderGraphDistinctContextsThroughPreview(t *testing.T) {
	for _, namespaces := range []bool{false, true} {
		t.Run(fmt.Sprintf("namespaces=%t", namespaces), func(t *testing.T) {
			dir := t.TempDir()
			resourceName := "${NAME}"
			if namespaces {
				resourceName = "app"
			}
			writePreviewFile(t, dir, "apps/cm.yaml", fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\ndata:\n  value: ${NAME}\n", resourceName))
			for _, name := range []string{"first", "second"} {
				namespace := ""
				if namespaces {
					namespace = "  targetNamespace: " + name + "\n"
				}
				writePreviewFile(t, dir, "root/"+name+".yaml", fmt.Sprintf("apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: %s\n  namespace: flux-system\nspec:\n  path: apps\n%s  postBuild:\n    substitute:\n      NAME: %s\n", name, namespace, name))
			}
			p, err := newTestPreview(t, WithLogger(logr.Discard()), WithPaths([]string{"root"}, false), WithFluxKS())
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Test(context.Background(), dir, &bytes.Buffer{}); err != nil {
				t.Fatalf("Test(distinct contexts) = %v", err)
			}
			loaded, err := p.loadRepo(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, res := range loaded[""].render.Resources() {
				if res.GetKind() != "ConfigMap" {
					continue
				}
				count++
				identity := res.GetName()
				if namespaces {
					identity = res.GetNamespace()
				}
				if identity != "first" && identity != "second" {
					t.Errorf("ConfigMap identity = %q, want first or second", identity)
				}
				if got := loaded[""].render.ProvenanceForID(res.CurId()).String(); got != "Kustomization flux-system/"+identity {
					t.Errorf("ConfigMap provenance = %q", got)
				}
			}
			if count != 2 {
				t.Errorf("rendered ConfigMaps = %d, want 2", count)
			}
		})
	}
}

func TestRenderGraphRawRootWithToolConfig(t *testing.T) {
	dir := t.TempDir()
	writePreviewFile(t, dir, ".fmp.yaml", "paths:\n- .\nsort: true\n")
	writePreviewFile(t, dir, "cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n")
	p, err := newTestPreview(t, WithLogger(logr.Discard()), WithPaths([]string{"."}, false))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Test(context.Background(), dir, &bytes.Buffer{}); err != nil {
		t.Fatalf("Test(raw root with tool config) = %v", err)
	}
}

func TestRenderGraphLateSourceResolution(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing=%t", missing), func(t *testing.T) {
			dir, source := t.TempDir(), t.TempDir()
			writePreviewFile(t, source, "cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: late-app\n")
			writePreviewFile(t, dir, "root/local.yaml", fmt.Sprintf("apiVersion: source.toolkit.fluxcd.io/v1\nkind: GitRepository\nmetadata:\n  name: local\n  namespace: flux-system\nspec:\n  url: %s\n", dir))
			writePreviewFile(t, dir, "root/sources.yaml", "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: sources\n  namespace: flux-system\nspec:\n  path: sources\n  sourceRef:\n    kind: GitRepository\n    name: local\n")
			writePreviewFile(t, dir, "root/app.yaml", "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: app\n  namespace: flux-system\nspec:\n  path: .\n  sourceRef:\n    kind: GitRepository\n    name: late\n")
			name := "late"
			if missing {
				name = "unrelated"
			}
			writePreviewFile(t, dir, "sources/repo.yaml", fmt.Sprintf("apiVersion: source.toolkit.fluxcd.io/v1\nkind: GitRepository\nmetadata:\n  name: %s\n  namespace: flux-system\nspec:\n  url: %s\n", name, source))
			p, err := newTestPreview(t, WithLogger(logr.Discard()), WithPaths([]string{"root"}, false), WithFluxKS(), WithGitRepo())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			var out bytes.Buffer
			err = p.Test(context.Background(), dir, &out)
			if missing {
				if err == nil || !strings.Contains(err.Error(), "unresolved GitRepository flux-system/late") {
					t.Fatalf("Test(missing source) = %v, want unresolved source failure", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Test(late source) = %v, output %s", err, &out)
			}
			out.Reset()
			if err := p.Render(context.Background(), dir, &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "name: late-app") {
				t.Fatalf("Render(late source) omitted app: %s", &out)
			}
		})
	}
}

func TestRenderGraphOmittedPathUsesSourceRoot(t *testing.T) {
	dir := t.TempDir()
	writePreviewFile(t, dir, "ks.yaml", "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: self\n  namespace: flux-system\nspec:\n  prune: true\n")
	p, err := newTestPreview(t, WithLogger(logr.Discard()), WithPaths([]string{"."}, false), WithFluxKS())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Test(context.Background(), dir, &bytes.Buffer{}); err != nil {
		t.Fatalf("Test(omitted path) = %v", err)
	}
}

func TestRenderGraphSelfReferenceTerminates(t *testing.T) {
	dir := t.TempDir()
	writePreviewFile(t, dir, "apps/ks.yaml", "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: self\n  namespace: flux-system\nspec:\n  path: apps\n")
	p, err := newTestPreview(t, WithPaths([]string{"apps"}, false), WithFluxKS())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if err := p.Test(t.Context(), dir, &bytes.Buffer{}); err != nil {
		t.Fatalf("self-reference failed to terminate: %v", err)
	}
}

func TestRenderGraphMissingDiscoveredPathFails(t *testing.T) {
	dir := t.TempDir()
	writePreviewFile(t, dir, "root/ks.yaml", "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: apps\n  namespace: flux\nspec:\n  path: missing\n")
	p, err := newTestPreview(t, WithPaths([]string{"root"}, false), WithFluxKS())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if err := p.Test(t.Context(), dir, &bytes.Buffer{}); err == nil {
		t.Fatal("Test(missing discovered path) succeeded, want error")
	}
}
