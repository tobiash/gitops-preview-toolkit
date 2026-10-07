package preview

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
)

func TestLocalSnapshotHelmSources(t *testing.T) {
	for _, url := range []string{".", "file:.", "https://example.invalid/self.git"} {
		t.Run(url, func(t *testing.T) {
			root := t.TempDir()
			writePreviewFile(t, root, ".fmp-source-repo-urls", "https://example.invalid/self.git\n")
			writeLocalHelmSource(t, root, url, "chart", "  postRenderers:\n  - kustomize:\n      patches:\n      - patch: |\n          apiVersion: v1\n          kind: ConfigMap\n          metadata:\n            name: app\n          data:\n            value: patched\n")
			p, err := newTestPreview(t, WithLocalOnly(), WithPaths([]string{"root"}, false), WithHelm(&config.HelmSettings{}), WithFluxKS())
			if err != nil {
				t.Fatal(err)
			}
			run, err := p.RunDiff(t.Context(), DiffRunOptions{LeftPath: root, RightPath: root, RetainSnapshots: true})
			if err != nil || !run.Complete || run.Before.Clusters[""].Size() != 3 || run.Result.TotalChanged() != 0 {
				t.Fatalf("RunDiff(local source %q) = %#v, %v, want three resources and no changes", url, run, err)
			}
		})
	}
}

func TestLocalSnapshotRejectsAcquisitionAndEscape(t *testing.T) {
	for _, tc := range []struct {
		name, url, chart, extra string
	}{
		{name: "remote git", url: "https://example.invalid/remote.git", chart: "chart"},
		{name: "relative git escape", url: "..", chart: "chart"},
		{name: "chart escape", url: ".", chart: "../chart"},
		{name: "postrender exec", url: ".", chart: "chart", extra: "  postRenderers:\n  - exec: {command: touch, args: [/tmp/never]}\n"},
		{name: "postrender remote path", url: ".", chart: "chart", extra: "  postRenderers:\n  - kustomize:\n      patches:\n      - path: https://example.invalid/patch\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeLocalHelmSource(t, root, tc.url, tc.chart, tc.extra)
			p, err := newTestPreview(t, WithLocalOnly(), WithPaths([]string{"root"}, false), WithHelm(&config.HelmSettings{}))
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := p.RenderSnapshot(t.Context(), root)
			wantError := "local-only"
			if tc.extra != "" {
				wantError = "unsupported Helm postrenderer"
			}
			if err == nil || snapshot.Complete || !strings.Contains(err.Error(), wantError) {
				t.Fatalf("RenderSnapshot(%s) = %#v, %v, want local-only incomplete", tc.name, snapshot, err)
			}
		})
	}
}

func writeLocalHelmSource(t *testing.T, root, url, chart, extra string) {
	t.Helper()
	writePreviewFile(t, root, "root/resources.yaml", fmt.Sprintf(`apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: local
  namespace: default
spec:
  url: %q
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: app
  namespace: default
spec:
  chart:
    spec:
      chart: %q
      sourceRef:
        kind: GitRepository
        name: local
%s`, url, chart, extra))
	writePreviewFile(t, root, "chart/Chart.yaml", "apiVersion: v2\nname: local\nversion: 0.1.0\n")
	writePreviewFile(t, root, "chart/templates/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\ndata:\n  value: local\n")
}

func TestLocalRootAndConfigSymlink(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writePreviewFile(t, outside, "cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: outside\n")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	p, err := newTestPreview(t, WithLocalOnly(), WithPaths([]string{"escape"}, false))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot, err := p.RenderSnapshot(t.Context(), root); err == nil || snapshot.Complete {
		t.Fatalf("RenderSnapshot(root symlink) = %#v, %v, want incomplete", snapshot, err)
	}
}

func TestPreviewCloseAndNewRollback(t *testing.T) {
	var captured *Preview
	want := errors.New("option failed")
	p, err := New(WithGitRepo(), func(p *Preview) error { captured = p; return want })
	if p != nil || !errors.Is(err, want) {
		t.Fatalf("New(failing option) = %v, %v, want nil and option error", p, err)
	}
	if err := captured.Close(); err != nil {
		t.Fatalf("Close(after rollback) = %v", err)
	}
	if err := captured.Close(); err != nil {
		t.Fatalf("Close(repeated) = %v", err)
	}
	if p, err := New(WithLocalOnly(), WithSOPSDecrypt(), WithGitRepo()); p != nil || err == nil {
		t.Fatalf("New(local SOPS) = %v, %v, want unsupported error", p, err)
	}
}
