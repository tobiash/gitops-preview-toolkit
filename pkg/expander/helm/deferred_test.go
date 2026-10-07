package helm

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	helmcli "helm.sh/helm/v4/pkg/cli"
	"sigs.k8s.io/kustomize/api/resmap"
)

type countingChartRunner struct {
	calls int
	fail  bool
}

func TestStrictInputsPreservesRemoteChartTasks(t *testing.T) {
	for _, source := range []struct {
		kind, spec, url string
		oci             bool
	}{
		{"HelmRepository", "url: https://example.invalid/charts", "https://example.invalid/charts", false},
		{"OCIRepository", "url: oci://example.invalid/charts", "oci://example.invalid/charts", true},
	} {
		t.Run(source.kind, func(t *testing.T) {
			r := renderFromYAML(t, fmt.Sprintf(`apiVersion: source.toolkit.fluxcd.io/v1
kind: %s
metadata: {name: remote, namespace: default}
spec: {%s}
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: app, namespace: default}
spec:
  chart:
    spec:
      chart: app
      sourceRef: {kind: %s, name: remote}
`, source.kind, source.spec, source.kind))
			runner := &stubChartRunner{}
			e := NewExpander(NewRunner(helmcli.New(), logr.Discard()), nil, logr.Discard())
			e.SetStrictInputs()
			e.runner = runner // Verify acquisition tasks without performing network I/O.
			result, err := e.Expand(t.Context(), r)
			if err != nil || len(result.Errors)+len(result.DeferredErrors) != 0 || len(runner.tasks) != 1 {
				t.Fatalf("Expand(strict %s) = %#v, %v, tasks=%d, want remote chart task", source.kind, result, err, len(runner.tasks))
			}
			task := runner.tasks[0]
			if task.repo.URL != source.url || task.isOCI != source.oci || e.localRoot != "" {
				t.Fatalf("strict remote task = %#v, localRoot=%q, want unchanged acquisition", task, e.localRoot)
			}
		})
	}
}

func (r *countingChartRunner) RenderCharts(context.Context, []RenderTask) (resmap.ResMap, []error, error) {
	r.calls++
	if r.fail {
		return resmap.New(), []error{errors.New("permanent chart error")}, nil
	}
	return resmap.New(), nil, nil
}

func TestDeferredHelmSourceRetriesWithoutRepeatingTasks(t *testing.T) {
	for _, fail := range []bool{false, true} {
		r := renderFromYAML(t, "apiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata: {name: app, namespace: default}\nspec:\n  chart:\n    spec:\n      chart: chart\n      sourceRef: {kind: GitRepository, name: late}\n")
		resolver := stubChartSourceResolver{}
		runner := &countingChartRunner{fail: fail}
		e := NewExpander(NewRunner(helmcli.New(), logr.Discard()), resolver, logr.Discard())
		e.runner = runner
		result, err := e.Expand(t.Context(), r)
		if err != nil || len(result.Errors) != 0 || len(result.DeferredErrors) != 1 || runner.calls != 0 {
			t.Fatalf("Expand(missing source) = %#v, %v, calls=%d, want deferred without task", result, err, runner.calls)
		}
		resolver["default/late"] = t.TempDir()
		result, err = e.Expand(t.Context(), r)
		if err != nil || len(result.DeferredErrors) != 0 || runner.calls != 1 || (len(result.Errors) > 0) != fail {
			t.Fatalf("Expand(resolved source) = %#v, %v, calls=%d, want one task (fail=%v)", result, err, runner.calls, fail)
		}
		result, err = e.Expand(t.Context(), r)
		if err != nil || len(result.Errors)+len(result.DeferredErrors) != 0 || runner.calls != 1 {
			t.Fatalf("Expand(already attempted) = %#v, %v, calls=%d, want no repeated output/errors", result, err, runner.calls)
		}
	}
}
