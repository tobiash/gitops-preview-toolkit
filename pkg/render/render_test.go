package render

import (
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"sigs.k8s.io/kustomize/api/hasher"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/resource"
)

func TestSort(t *testing.T) {
	r := newRenderFromYAML(t,
		`apiVersion: v1
kind: Service
metadata:
  name: svc-b
  namespace: alpha
`,
		`apiVersion: v1
kind: ConfigMap
metadata:
  name: cm-a
  namespace: beta
`,
		`apiVersion: v1
kind: ConfigMap
metadata:
  name: cm-b
  namespace: alpha
`,
	)

	r.Sort()

	resources := r.Resources()
	if len(resources) != 3 {
		t.Fatalf("expected 3 resources, got %d", len(resources))
	}

	// Expected order: ConfigMap/alpha/cm-b, ConfigMap/beta/cm-a, Service/alpha/svc-b
	checkOrder(t, resources, 0, "ConfigMap", "alpha", "cm-b")
	checkOrder(t, resources, 1, "ConfigMap", "beta", "cm-a")
	checkOrder(t, resources, 2, "Service", "alpha", "svc-b")
}

func TestFilterCRDs(t *testing.T) {
	r := newRenderFromYAML(t,
		`apiVersion: v1
kind: ConfigMap
metadata:
  name: my-cm
  namespace: default
`,
		`apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.com
`,
		`apiVersion: v1
kind: Service
metadata:
  name: my-svc
  namespace: default
`,
	)

	r.FilterCRDs()

	resources := r.Resources()
	if len(resources) != 2 {
		t.Fatalf("expected 2 resources after CRD filtering, got %d", len(resources))
	}
	for _, res := range resources {
		if res.GetKind() == "CustomResourceDefinition" {
			t.Error("CRD should have been filtered out")
		}
	}
}

func TestFilterCRDs_EmptyRender(t *testing.T) {
	r := NewDefaultRender(logr.Discard())
	r.FilterCRDs()
	if r.Size() != 0 {
		t.Errorf("expected empty render to stay empty, got %d resources", r.Size())
	}
}

func TestAbsorbResMap_ReplacesDuplicateResource(t *testing.T) {
	r := NewDefaultRender(logr.Discard())
	factory := resmap.NewFactory(resource.NewFactory(&hasher.Hasher{}))
	first, err := factory.NewResMapFromBytes([]byte(`apiVersion: v1
kind: Namespace
metadata:
  name: csi-addons-system
  labels:
    source: local
`))
	if err != nil {
		t.Fatalf("NewResMapFromBytes(first) error = %v", err)
	}
	second, err := factory.NewResMapFromBytes([]byte(`apiVersion: v1
kind: Namespace
metadata:
  name: csi-addons-system
  labels:
    source: remote
`))
	if err != nil {
		t.Fatalf("NewResMapFromBytes(second) error = %v", err)
	}
	if err := r.absorbResMap("first", "Kustomization flux-system/first", first); err != nil {
		t.Fatalf("absorbResMap(first) error = %v", err)
	}
	if err := r.absorbResMap("second", "Kustomization flux-system/second", second); err != nil {
		t.Fatalf("absorbResMap(second) error = %v", err)
	}
	resources := r.Resources()
	if len(resources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(resources))
	}
	if resources[0].GetLabels()["source"] != "remote" {
		t.Fatalf("expected duplicate resource replacement, got labels %v", resources[0].GetLabels())
	}
	warnings := r.Warnings()
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d", len(warnings))
	}
	if got := warnings[0].Error(); got == "" || !strings.Contains(got, "Kustomization flux-system/second") || !strings.Contains(got, "Kustomization flux-system/first") {
		t.Fatalf("expected duplicate resource warning, got %q", got)
	}
}

func TestApplySubstitutionsToNew(t *testing.T) {
	r := newRenderFromYAML(t, `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${APP}
  namespace: default
spec:
  accessModes:
  - ${VOLSYNC_ACCESSMODES:-ReadWriteOnce}
`)

	if err := r.ApplySubstitutionsToNew(0, "Kustomization flux-system/obico", map[string]string{"APP": "obico"}); err != nil {
		t.Fatalf("ApplySubstitutionsToNew() error = %v", err)
	}

	resources := r.Resources()
	if len(resources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(resources))
	}
	if got, want := resources[0].GetName(), "obico"; got != want {
		t.Fatalf("name = %q, want %q", got, want)
	}
	yaml := resources[0].MustYaml()
	if strings.Contains(yaml, "${APP}") {
		t.Fatalf("expected APP substitution, got:\n%s", yaml)
	}
	if !strings.Contains(yaml, "ReadWriteOnce") {
		t.Fatalf("expected default substitution, got:\n%s", yaml)
	}
}

func checkOrder(t *testing.T, resources []*resource.Resource, idx int, kind, namespace, name string) {
	t.Helper()
	res := resources[idx]
	if res.GetKind() != kind {
		t.Errorf("resource[%d]: expected kind %q, got %q", idx, kind, res.GetKind())
	}
	if res.GetNamespace() != namespace {
		t.Errorf("resource[%d]: expected namespace %q, got %q", idx, namespace, res.GetNamespace())
	}
	if res.GetName() != name {
		t.Errorf("resource[%d]: expected name %q, got %q", idx, name, res.GetName())
	}
}

func newRenderFromYAML(t *testing.T, yamls ...string) *Render {
	t.Helper()
	r := NewDefaultRender(logr.Discard())
	factory := resmap.NewFactory(resource.NewFactory(&hasher.Hasher{}))
	for _, y := range yamls {
		rm, err := factory.NewResMapFromBytes([]byte(y))
		if err != nil {
			t.Fatalf("failed to parse YAML: %v", err)
		}
		if err := r.AbsorbAll(rm); err != nil {
			t.Fatalf("failed to absorb: %v", err)
		}
	}
	return r
}

func TestFilterByLabel(t *testing.T) {
	r := newRenderFromYAML(t,
		`apiVersion: v1
kind: ConfigMap
metadata:
  name: cm-a
  namespace: default
  labels:
    helm.toolkit.fluxcd.io/name: my-release
    helm.toolkit.fluxcd.io/namespace: flux-system
`,
		`apiVersion: v1
kind: ConfigMap
metadata:
  name: cm-b
  namespace: default
  labels:
    helm.toolkit.fluxcd.io/name: other-release
    helm.toolkit.fluxcd.io/namespace: flux-system
`,
		`apiVersion: v1
kind: Service
metadata:
  name: svc-a
  namespace: default
`,
	)

	r.FilterByLabel("helm.toolkit.fluxcd.io/name", "my-release")

	resources := r.Resources()
	if len(resources) != 1 {
		t.Fatalf("expected 1 resource after label filtering, got %d", len(resources))
	}
	if resources[0].GetName() != "cm-a" {
		t.Errorf("expected cm-a, got %s", resources[0].GetName())
	}
}

func TestFilterByLabel_NoMatch(t *testing.T) {
	r := newRenderFromYAML(t,
		`apiVersion: v1
kind: ConfigMap
metadata:
  name: cm-a
  namespace: default
`,
	)

	r.FilterByLabel("helm.toolkit.fluxcd.io/name", "nonexistent")

	if r.Size() != 0 {
		t.Errorf("expected 0 resources, got %d", r.Size())
	}
}

func TestSetProvenancePreservesStructuredOriginWithoutChangingYAML(t *testing.T) {
	r := newRenderFromYAML(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\n")
	res := r.Resources()[0]
	before := res.MustYaml()
	want := Provenance{Kind: "Kustomization", Name: "apps", Namespace: "flux", Path: "clusters/apps"}
	r.SetProvenance(res.CurId(), want)
	view, ok := r.ResourceViewForID(res.CurId())
	if !ok || view.Provenance != want || view.YAML != before {
		t.Fatalf("resource view = %#v, %t; want exact provenance and unchanged YAML", view, ok)
	}
	if err := r.ApplyNamespaceToNew(0, "apps"); err != nil {
		t.Fatal(err)
	}
	if got := r.ProvenanceForID(res.CurId()); got != want {
		t.Fatalf("namespace transform lost structured origin: %#v, want %#v", got, want)
	}
	merged := NewDefaultRender(logr.Discard())
	if err := merged.AbsorbAll(r); err != nil {
		t.Fatal(err)
	}
	if got := merged.ProvenanceForID(res.CurId()); got != want {
		t.Fatalf("merged provenance = %#v, want %#v", got, want)
	}
}
