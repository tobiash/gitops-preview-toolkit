package preview

import (
	"strings"
	"testing"

	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
)

func TestSnapshotExpandsHelmGeneratedFlux(t *testing.T) {
	for _, remote := range []bool{false, true} {
		name := "local children"
		url := "."
		if remote {
			name, url = "remote child denied", "https://example.invalid/child.git"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeLocalHelmSource(t, root, ".", "chart", "")
			writePreviewFile(t, root, "chart/templates/cm.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata: {name: child-source, namespace: default}
spec: {url: '`+url+`'}
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: {name: child-ks, namespace: default}
spec:
  path: child
  sourceRef: {kind: GitRepository, name: child-source}
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: child-hr, namespace: default}
spec:
  chart:
    spec:
      chart: child-chart
      sourceRef: {kind: GitRepository, name: child-source}
`)
			writePreviewFile(t, root, "child/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: from-child-ks}\n")
			writePreviewFile(t, root, "child-chart/Chart.yaml", "apiVersion: v2\nname: child\nversion: 0.1.0\n")
			writePreviewFile(t, root, "child-chart/templates/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: from-child-hr}\n")
			p, err := newTestPreview(t, WithLocalOnly(), WithPaths([]string{"root"}, false), WithFluxKS(), WithHelm(&config.HelmSettings{}))
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := p.RenderSnapshot(t.Context(), root)
			if remote {
				if err == nil || snapshot.Complete || !strings.Contains(err.Error(), "denies remote clone") {
					t.Fatalf("RenderSnapshot(generated remote) = %#v, %v, want incomplete denial", snapshot, err)
				}
				return
			}
			if err != nil || !snapshot.Complete {
				t.Fatalf("RenderSnapshot(generated local children) = %#v, %v", snapshot, err)
			}
			found := map[string]bool{}
			for _, res := range snapshot.Clusters[""].Resources() {
				if res.GetKind() == "ConfigMap" {
					found[res.GetName()] = true
				}
			}
			if !found["from-child-ks"] || !found["from-child-hr"] {
				t.Fatalf("generated child ConfigMaps = %v, want both KS and Helm output", found)
			}
		})
	}
}

func TestSnapshotRetriesLateHelmSource(t *testing.T) {
	root := t.TempDir()
	writeLocalHelmSource(t, root, ".", "chart", "")
	writePreviewFile(t, root, "root/late.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: {name: sources, namespace: default}
spec:
  path: sources
  sourceRef: {kind: GitRepository, name: local}
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: late, namespace: default}
spec:
  chart:
    spec:
      chart: chart
      sourceRef: {kind: GitRepository, name: late-source}
`)
	writePreviewFile(t, root, "sources/repo.yaml", "apiVersion: source.toolkit.fluxcd.io/v1\nkind: GitRepository\nmetadata: {name: late-source, namespace: default}\nspec: {url: '.'}\n")
	p, err := newTestPreview(t, WithLocalOnly(), WithPaths([]string{"root"}, false), WithFluxKS(), WithHelm(&config.HelmSettings{}))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := p.RenderSnapshot(t.Context(), root)
	if err != nil || !snapshot.Complete {
		t.Fatalf("RenderSnapshot(late Helm source) = %#v, %v, want complete", snapshot, err)
	}
	for _, res := range snapshot.Clusters[""].Resources() {
		if res.GetKind() == "ConfigMap" && res.GetName() == "late" {
			return
		}
	}
	t.Fatal("RenderSnapshot(late Helm source) omitted the late chart")
}

func TestLocalSnapshotUnsupportedFluxInputs(t *testing.T) {
	// This is an explicit matrix of known unsupported rendering inputs, not a
	// claim that offline previews reproduce every Flux controller behavior.
	for _, tc := range []struct {
		name, kind, spec string
	}{
		{"substituteFrom required", "Kustomization", "  postBuild:\n    substituteFrom: [{kind: Secret, name: missing, optional: false}]\n"},
		{"substituteFrom optional", "Kustomization", "  postBuild:\n    substituteFrom: [{kind: ConfigMap, name: existing, optional: true}]\n"},
		{"patches", "Kustomization", "  patches: [{patch: 'metadata: {name: changed}'}]\n"},
		{"images", "Kustomization", "  images: [{name: app, newTag: changed}]\n"},
		{"components", "Kustomization", "  components: [component]\n"},
		{"namePrefix", "Kustomization", "  namePrefix: changed-\n"},
		{"nameSuffix", "Kustomization", "  nameSuffix: -changed\n"},
		{"commonMetadata", "Kustomization", "  commonMetadata: {labels: {changed: 'true'}}\n"},
		{"decryption", "Kustomization", "  decryption: {provider: sops}\n"},
		{"valuesFiles", "HelmRelease", "      valuesFiles: [chart/override.yaml]\n"},
		{"ignoreMissingValuesFiles", "HelmRelease", "      ignoreMissingValuesFiles: true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeLocalHelmSource(t, root, ".", "chart", tc.spec)
			writePreviewFile(t, root, "chart/override.yaml", "value: overridden\n")
			if tc.kind == "Kustomization" {
				writeLocalHelmSource(t, root, ".", "chart", "")
				writePreviewFile(t, root, "root/ks.yaml", "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata: {name: child, namespace: default}\nspec:\n  path: child\n  sourceRef: {kind: GitRepository, name: local}\n"+tc.spec)
				writePreviewFile(t, root, "child/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: child}\ndata: {value: '${REQUIRED}'}\n")
			}
			p, err := newTestPreview(t, WithLocalOnly(), WithPaths([]string{"root"}, false), WithFluxKS(), WithHelm(&config.HelmSettings{}))
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := p.RenderSnapshot(t.Context(), root)
			if err == nil || snapshot.Complete || !strings.Contains(err.Error(), "unsupported") {
				t.Fatalf("RenderSnapshot(%s) = %#v, %v, want explicit unsupported error", tc.name, snapshot, err)
			}
			trusted, err := newTestPreview(t, WithGitRepo(), WithPaths([]string{"root"}, false), WithFluxKS(), WithHelm(&config.HelmSettings{}))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := trusted.Close(); err != nil {
					t.Error(err)
				}
			})
			if snapshot, err := trusted.RenderSnapshot(t.Context(), root); err != nil || !snapshot.Complete {
				t.Fatalf("trusted RenderSnapshot(%s) = %#v, %v, want legacy behavior unchanged", tc.name, snapshot, err)
			}
			if err := WithStrictInputs()(trusted); err != nil {
				t.Fatal(err)
			}
			if trusted.localOnly {
				t.Fatal("WithStrictInputs enabled local-only restrictions")
			}
			if snapshot, err := trusted.RenderSnapshot(t.Context(), root); err == nil || snapshot.Complete || !strings.Contains(err.Error(), "unsupported") {
				t.Fatalf("strict RenderSnapshot(%s) = %#v, %v, want unsupported input error", tc.name, snapshot, err)
			}
		})
	}
}
