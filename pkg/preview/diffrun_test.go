package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/ai"
	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/diff"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
)

func TestRunDiffLocalHelmChartStructuredOrigins(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	for i, dir := range []string{left, right} {
		writePreviewFile(t, dir, "root/resources.yaml", fmt.Sprintf(`apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: charts
  namespace: flux-system
spec:
  url: %s
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: origin
  namespace: flux-system
spec:
  releaseName: destination
  targetNamespace: apps
  chart:
    spec:
      chart: chart
      sourceRef:
        kind: GitRepository
        name: charts
`, dir))
		writePreviewFile(t, dir, "chart/Chart.yaml", "apiVersion: v2\nname: local\nversion: 0.1.0\n")
		writePreviewFile(t, dir, "chart/templates/cm.yaml", fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\n  namespace: {{ .Release.Namespace }}\ndata:\n  value: %q\n", fmt.Sprint(i)))
	}
	p, err := newTestPreview(t, WithLogger(logr.Discard()), WithPaths([]string{"root"}, false), WithFluxKS(), WithGitRepo(), WithHelm(&config.HelmSettings{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	run, err := p.RunDiff(context.Background(), DiffRunOptions{LeftPath: left, RightPath: right})
	if err != nil {
		t.Fatal(err)
	}
	if !run.Complete || len(run.Warnings) != 0 {
		t.Fatalf("RunDiff(local chart) = %#v, want complete without warnings", run)
	}
	data, err := json.Marshal(run.Result.ToJSON())
	if err != nil {
		t.Fatal(err)
	}
	var output diff.DiffResultJSON
	if err := json.Unmarshal(data, &output); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, change := range output.Changes {
		if change.ObjectRef.Kind != "ConfigMap" {
			continue
		}
		found = true
		if change.Action != "modified" || change.ObjectRef.Name != "destination" || change.ObjectRef.Namespace != "apps" {
			t.Errorf("local chart change = %#v, want modified ConfigMap apps/destination", change)
		}
		want := render.HelmReleaseProvenance("flux-system", "origin")
		for name, origin := range map[string]*render.Provenance{"provenance": &change.Provenance, "beforeOrigin": change.BeforeOrigin, "afterOrigin": change.AfterOrigin} {
			if origin == nil || origin.Kind != want.Kind || origin.Name != want.Name || origin.Namespace != want.Namespace {
				t.Errorf("local chart %s = %#v, want %#v", name, origin, want)
			}
		}
	}
	if !found {
		t.Fatalf("RunDiff(local chart) omitted ConfigMap change: %s", data)
	}
}

func TestRunDiffKeepsPartialResultWhenHelmExpansionWarns(t *testing.T) {
	left := t.TempDir()
	right := t.TempDir()

	writePreviewFile(t, left, "helmrelease.yaml", brokenHelmReleaseYAML())
	writePreviewFile(t, right, "helmrelease.yaml", brokenHelmReleaseYAML())
	writePreviewFile(t, right, "configmap.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: new-config
  namespace: default
data:
  key: value
`)

	p, err := newTestPreview(t,
		WithLogger(logr.Discard()),
		WithPaths([]string{"."}, false),
		WithHelm(&config.HelmSettings{}),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	var output bytes.Buffer
	assessor := &warningAssessor{}
	run, err := p.RunDiff(context.Background(), DiffRunOptions{LeftPath: left, RightPath: right,
		DiffWriter: &output, Policies: &config.PolicyConfig{Inline: []string{"invalid rego"}},
		AI: &config.AIConfig{Enabled: boolPtr(true)}, AIAssessor: assessor})
	if err == nil {
		t.Fatal("RunDiff() error = nil, want incomplete render error")
	}
	if run == nil || run.Result.TotalChanged() != 0 || run.PolicyResult != nil || run.AIAssessment != nil {
		t.Fatalf("RunDiff() = %#v, want suppressed changes and assessments", run)
	}
	if run.Complete || output.Len() != 0 {
		t.Fatalf("RunDiff() complete=%v output=%q, want incomplete without authoritative diff", run.Complete, &output)
	}
	if assessor.called {
		t.Fatal("AI assessor called for incomplete render")
	}
	if len(run.Warnings) != 1 {
		t.Fatalf("warnings = %#v, want one warning", run.Warnings)
	}
	if !strings.Contains(run.Warnings[0], "HelmRelease default/broken") {
		t.Fatalf("warning %q does not mention broken HelmRelease", run.Warnings[0])
	}
}

type warningAssessor struct{ called bool }

func (a *warningAssessor) Assess(context.Context, ai.Request) (*ai.Assessment, error) {
	a.called = true
	return &ai.Assessment{Warnings: []string{"optional assessment warning"}}, nil
}

func TestRunDiffWarningsDoNotImplyIncomplete(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	writePreviewFile(t, right, "cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: added\n")
	p, err := newTestPreview(t, WithLogger(logr.Discard()), WithPaths([]string{"."}, false))
	if err != nil {
		t.Fatal(err)
	}
	run, err := p.RunDiff(context.Background(), DiffRunOptions{LeftPath: left, RightPath: right,
		AI: &config.AIConfig{Enabled: boolPtr(true)}, AIAssessor: &warningAssessor{}})
	if err != nil {
		t.Fatal(err)
	}
	if !run.Complete || len(run.Warnings) != 1 || run.Result.TotalChanged() != 1 {
		t.Fatalf("RunDiff() = %#v, want complete with warning and addition", run)
	}
}

func TestDuplicateResourcesMakePreviewIncomplete(t *testing.T) {
	dir := t.TempDir()
	manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: duplicate\n"
	writePreviewFile(t, dir, "a.yaml", manifest)
	writePreviewFile(t, dir, "b.yaml", manifest)
	p, err := newTestPreview(t, WithLogger(logr.Discard()), WithPaths([]string{"."}, false))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Test(context.Background(), dir, &bytes.Buffer{}); err == nil {
		t.Fatal("Test(duplicate resources) succeeded, want failure")
	}
}

func brokenHelmReleaseYAML() string {
	return `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: broken
  namespace: default
spec:
  interval: 30m
  chart:
    spec:
      chart: missing
      sourceRef:
        kind: HelmRepository
        name: missing
        namespace: default
`
}
