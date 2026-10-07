package helm

import (
	"context"
	"testing"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/expander"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	helmcli "helm.sh/helm/v4/pkg/cli"
)

func TestParseManifestsRejectsInvalidAndDuplicateResources(t *testing.T) {
	valid := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: example\n"
	for name, data := range map[string]string{
		"missing name": valid + "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {}\n",
		"duplicate":    valid + "---\n" + valid,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseManifests([]byte(data), logr.Discard()); err == nil {
				t.Fatal("parseManifests succeeded, want invalid/duplicate error")
			}
		})
	}
}

func TestRenderChartsPreservesOrigin(t *testing.T) {
	dir := t.TempDir()
	writeChartFile(t, dir, "Chart.yaml", "apiVersion: v2\nname: test\nversion: 0.1.0\n")
	writeChartFile(t, dir, "templates/resource.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: example\n  labels:\n    helm.toolkit.fluxcd.io/name: origin\n    helm.toolkit.fluxcd.io/namespace: flux-system\n")
	runner := NewRunner(helmcli.New(), logr.Discard())
	resources, errs, err := runner.RenderCharts(context.Background(), []RenderTask{{localChartPath: dir, releaseName: "destination", namespace: "apps", postRenderer: newPostRendererOriginLabels("origin", "flux-system")}})
	if err != nil || len(errs) != 0 {
		t.Fatalf("RenderCharts errors = %v, %v", err, errs)
	}
	res := resources.Resources()[0]
	if got := res.GetLabels()["helm.toolkit.fluxcd.io/name"]; got != "origin" {
		t.Errorf("origin name = %q, want origin", got)
	}
	if got := res.GetLabels()["helm.toolkit.fluxcd.io/namespace"]; got != "flux-system" {
		t.Errorf("origin namespace = %q, want flux-system", got)
	}
	if got := res.GetAnnotations()[render.ProducerAnnotation]; got != "HelmRelease flux-system/origin" {
		t.Errorf("producer = %q, want HelmRelease flux-system/origin", got)
	}
}

func TestExpandReportsInvalidAndDuplicateChartOutput(t *testing.T) {
	valid := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: example\n"
	for name, manifest := range map[string]string{
		"invalid":   "apiVersion: v1\nkind: ConfigMap\nmetadata: {}\n",
		"duplicate": valid + "---\n" + valid,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeChartFile(t, dir, "Chart.yaml", "apiVersion: v2\nname: test\nversion: 0.1.0\n")
			writeChartFile(t, dir, "templates/resources.yaml", manifest)
			r := renderFromYAML(t, "apiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata:\n  name: origin\n  namespace: flux-system\nspec:\n  releaseName: destination\n  targetNamespace: apps\n  chart:\n    spec:\n      chart: .\n      sourceRef:\n        kind: GitRepository\n        name: source\n")
			e := NewExpander(NewRunner(helmcli.New(), logr.Discard()), stubChartSourceResolver{"flux-system/source": dir}, logr.Discard())
			registry := expander.NewRegistry(logr.Discard())
			registry.Register(e)
			result, err := registry.Expand(context.Background(), r)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Errors) == 0 {
				t.Fatal("Expand invalid/duplicate chart returned no Errors")
			}
			if result.Resources.Size() != 0 {
				t.Errorf("invalid chart produced %d resources, want 0", result.Resources.Size())
			}
		})
	}
}

func TestRenderChartsReportsCrossChartDuplicates(t *testing.T) {
	dir := t.TempDir()
	writeChartFile(t, dir, "Chart.yaml", "apiVersion: v2\nname: test\nversion: 0.1.0\n")
	writeChartFile(t, dir, "templates/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: shared\n")
	runner := NewRunner(helmcli.New(), logr.Discard())
	_, errs, err := runner.RenderCharts(context.Background(), []RenderTask{
		{localChartPath: dir, releaseName: "first", namespace: "apps"},
		{localChartPath: dir, releaseName: "second", namespace: "apps"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 1 {
		t.Errorf("cross-chart duplicate errors = %v, want one error", errs)
	}
}
