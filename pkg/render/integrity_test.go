package render

import (
	"fmt"
	"testing"

	"github.com/go-logr/logr"
)

func TestAbsorbProducerAnnotationsAndHelmOriginLabels(t *testing.T) {
	for _, tt := range []struct {
		name     string
		producer string
		labels   string
		want     Provenance
	}{
		{name: "generated origin", producer: "HelmRelease flux-system/origin", labels: "    helm.toolkit.fluxcd.io/name: origin\n    helm.toolkit.fluxcd.io/namespace: flux-system\n", want: HelmReleaseProvenance("flux-system", "origin")},
		{name: "explicit producer", producer: "explicit source", labels: "    helm.toolkit.fluxcd.io/name: origin\n    helm.toolkit.fluxcd.io/namespace: flux-system\n", want: TextProvenance("explicit source")},
		{name: "different Helm producer", producer: "HelmRelease other/source", labels: "    helm.toolkit.fluxcd.io/name: origin\n    helm.toolkit.fluxcd.io/namespace: flux-system\n", want: TextProvenance("HelmRelease other/source")},
		{name: "text without labels", producer: "HelmRelease flux-system/origin", want: TextProvenance("HelmRelease flux-system/origin")},
		{name: "incomplete source labels", producer: "HelmRelease apps/origin", labels: "    helm.toolkit.fluxcd.io/name: origin\n", want: TextProvenance("HelmRelease apps/origin")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newRenderFromYAML(t, fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: destination\n  namespace: apps\n  annotations:\n    %s: %q\n  labels:\n%s", ProducerAnnotation, tt.producer, tt.labels))
			merged := NewDefaultRender(logr.Discard())
			if err := merged.AbsorbAll(r); err != nil {
				t.Fatal(err)
			}
			if got := merged.ProvenanceForID(merged.Resources()[0].CurId()); got != tt.want {
				t.Errorf("AbsorbAll(%s) provenance = %#v, want %#v", tt.name, got, tt.want)
			}
		})
	}
}

func TestNamespaceTransformReportsCollisionsAndPreservesClusterScope(t *testing.T) {
	r := newRenderFromYAML(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: shared
  namespace: first
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: shared
  namespace: second
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.com
spec:
  group: example.com
  scope: Cluster
  names:
    kind: Widget
    plural: widgets
---
apiVersion: example.com/v1
kind: Widget
metadata:
  name: cluster-widget
`)
	r.MarkProvenanceToNew(0, "Kustomization flux/apps")
	if err := r.ApplyNamespaceToNew(0, "target"); err != nil {
		t.Fatal(err)
	}
	if len(r.Warnings()) != 1 {
		t.Errorf("namespace collision warnings = %v, want one", r.Warnings())
	}
	for _, res := range r.Resources() {
		want := ""
		if res.GetKind() == "ConfigMap" {
			want = "target"
		}
		if got := res.GetNamespace(); got != want {
			t.Errorf("%s namespace = %q, want %q", res.GetKind(), got, want)
		}
		if got := r.ProvenanceForID(res.CurId()).String(); got != "Kustomization flux/apps" {
			t.Errorf("transformed provenance = %q", got)
		}
	}
	merged := NewDefaultRender(logr.Discard())
	if err := merged.AbsorbAll(r); err != nil {
		t.Fatal(err)
	}
	if len(merged.Warnings()) != 1 {
		t.Errorf("merged warnings = %v, want namespace collision", merged.Warnings())
	}
}
