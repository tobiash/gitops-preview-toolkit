package build

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

func TestLocalKustomizationReferences(t *testing.T) {
	for _, config := range []string{
		"resources: [https://example.invalid/base]",
		"bases: [git@github.com:org/repo]",
		"components: [github.com/org/repo//base]",
		"resources: [../outside.yaml]",
		"patches: [{path: https://example.invalid/patch}]",
		"patchesJson6902: [{path: ../outside.yaml}]",
		"patchesStrategicMerge: [../outside.yaml]",
		"replacements: [{path: ../outside.yaml}]",
		"openapi: {path: ../outside.yaml}",
		"configurations: [../outside.yaml]",
		"crds: [../outside.yaml]",
		"generators: [plugin.yaml]",
		"transformers: [plugin.yaml]",
		"validators: [plugin.yaml]",
		"helmCharts: [{name: remote, repo: https://example.invalid}]",
		"helmChartInflationGenerator: [{chartName: remote}]",
		"configMapGenerator: [{name: test, files: [key=../outside.yaml]}]",
		"secretGenerator: [{name: test, envs: [../outside.yaml]}]",
		"configMapGenerator: [{name: test, env: ../outside.yaml}]",
	} {
		t.Run(config, func(t *testing.T) {
			fs := filesys.MakeFsInMemory()
			for path, data := range map[string]string{
				"/root/kustomization.yaml": config,
				"/outside.yaml":            "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: outside\n",
				"/root/plugin.yaml":        "apiVersion: builtin\nkind: HelmChartInflationGenerator\nmetadata:\n  name: bad\n",
			} {
				if err := fs.WriteFile(path, []byte(data)); err != nil {
					t.Fatal(err)
				}
			}
			r := New(logr.Discard())
			r.SetLocalOnly("/root")
			if err := r.AddPath(fs, "/root"); err == nil {
				t.Fatalf("AddPath(%q) error = nil, want local-only denial", config)
			}
		})
	}
}

func TestLocalKustomizationNestedAndConstructedFilesystem(t *testing.T) {
	fs := filesys.MakeFsInMemory()
	for path, content := range map[string]string{
		"/root/kustomization.yaml":      "resources: [base]\n",
		"/root/base/kustomization.yaml": "resources: [https://example.invalid/resource.yaml]\n",
	} {
		if err := fs.WriteFile(path, []byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := render.ValidateLocalKustomization(fs, "/root", "/root"); err == nil {
		t.Fatal("ValidateLocalKustomization(nested remote) = nil, want denial")
	}
	if err := fs.WriteFile("/root/base/kustomization.yaml", []byte("configMapGenerator:\n- name: local\n  literals: [key=value]\n")); err != nil {
		t.Fatal(err)
	}
	r := New(logr.Discard())
	r.SetLocalOnly("/root")
	if err := r.AddPath(fs, "/root"); err != nil {
		t.Fatalf("AddPath(local nested generator) = %v", err)
	}
}

func TestLocalReadRejectsSymlinkEscape(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "cm.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cm.yaml", "kustomization.yaml"} {
		t.Run(name, func(t *testing.T) {
			link := filepath.Join(root, name)
			if err := os.Symlink(filepath.Join(outside, "cm.yaml"), link); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Remove(link) })
			r := New(logr.Discard())
			r.SetLocalOnly(root)
			if err := r.AddPath(filesys.MakeFsOnDisk(), root); err == nil {
				t.Fatal("AddPath(symlink escape) = nil, want denial")
			}
		})
	}
}
