package build

import (
	"testing"

	"github.com/go-logr/logr"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

func TestMalformedRawYAMLReturnsError(t *testing.T) {
	fs := filesys.MakeFsInMemory()
	if err := fs.MkdirAll("/raw"); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile("/raw/bad.yaml", []byte("apiVersion: v1\nkind: ConfigMap\nmetadata: [")); err != nil {
		t.Fatal(err)
	}
	if err := New(logr.Discard()).AddPath(fs, "/raw"); err == nil {
		t.Fatal("AddPath(malformed YAML) succeeded, want error")
	}
}

func TestRawYAMLExcludesOnlyKnownToolConfig(t *testing.T) {
	for _, name := range []string{".fmp.yaml", ".fmp.yml", ".gitops-preview.yaml", ".gitops-preview.yml", ".github/fmp.yaml"} {
		t.Run(name, func(t *testing.T) {
			fs := filesys.MakeFsInMemory()
			if err := fs.MkdirAll("/raw/.github"); err != nil {
				t.Fatal(err)
			}
			if err := fs.WriteFile("/raw/"+name, []byte("sort: true\n")); err != nil {
				t.Fatal(err)
			}
			if err := fs.WriteFile("/raw/cm.yaml", []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n")); err != nil {
				t.Fatal(err)
			}
			r := New(logr.Discard())
			if err := r.AddPaths(fs, "/raw"); err != nil {
				t.Fatalf("AddPaths(tool config %s) = %v", name, err)
			}
			if r.Size() != 1 {
				t.Errorf("AddPaths rendered %d resources, want 1", r.Size())
			}
			if err := fs.WriteFile("/raw/fmp.yaml", []byte("sort: true\n")); err != nil {
				t.Fatal(err)
			}
			if err := New(logr.Discard()).AddPaths(fs, "/raw"); err == nil {
				t.Fatal("AddPaths(non-config invalid YAML) succeeded, want error")
			}
		})
	}
}

func TestKustomizeBoundaryAndBuildProvenance(t *testing.T) {
	fs := filesys.MakeFsInMemory()
	for path, data := range map[string]string{
		"/root/kustomization.yaml": "resources: [raw/cm.yaml]\nnamePrefix: built-\n",
		"/root/raw/cm.yaml":        "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n",
	} {
		if err := fs.WriteFile(path, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	b := New(logr.Discard())
	if err := b.AddPathsWithProducer(fs, "/root", "Kustomization flux/apps"); err != nil {
		t.Fatal(err)
	}
	if b.Size() != 1 || b.Resources()[0].GetName() != "built-app" {
		t.Fatalf("recursive build crossed Kustomize boundary: %s", b.Resources())
	}
	if got := b.ProvenanceForID(b.Resources()[0].CurId()).String(); got != "Kustomization flux/apps" {
		t.Fatalf("build provenance = %q", got)
	}
}
