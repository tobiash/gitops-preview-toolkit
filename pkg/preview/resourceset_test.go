package preview

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
)

func TestResourceSetTwoRegionExample(t *testing.T) {
	root, err := filepath.Abs("../../docs/examples/resourcesets")
	if err != nil {
		t.Fatal(err)
	}
	p, err := newTestPreview(t, WithLocalOnly(), WithPaths([]string{"root"}, false), WithFluxKS())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := p.RenderSnapshot(t.Context(), root)
	if err != nil || !snapshot.Complete {
		t.Fatalf("RenderSnapshot(two-region example) = %#v, %v, want complete", snapshot, err)
	}
	got := map[string]string{}
	for _, res := range snapshot.Clusters[""].Resources() {
		key := res.GetKind() + "/" + res.GetNamespace() + "/" + res.GetName()
		got[key] = ""
		if res.GetKind() == "ConfigMap" {
			value, err := res.GetFieldValue("data.region")
			if err != nil {
				t.Fatal(err)
			}
			got[key] = value.(string)
		}
	}
	want := map[string]string{
		"GitRepository/flux-system/local":               "",
		"ResourceSet/flux-system/regional-workloads":    "",
		"Kustomization/flux-system/workloads-eu-west-1": "",
		"Kustomization/flux-system/workloads-us-east-1": "",
		"Namespace//apps-eu-west-1":                     "",
		"Namespace//apps-us-east-1":                     "",
		"ConfigMap/apps-eu-west-1/regional-settings":    "eu-west-1",
		"ConfigMap/apps-us-east-1/regional-settings":    "us-east-1",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("RenderSnapshot(two-region example) inventory mismatch (-want +got):\n%s", diff)
	}
}

func TestResourceSetSnapshotExpandsKustomizationAndHelmDescendants(t *testing.T) {
	root := t.TempDir()
	writeResourceSetTree(t, root, "    - name: east\n      path: child-east\n      message: east\n")
	p, err := newTestPreview(t, WithLocalOnly(), WithPaths([]string{"root"}, false), WithFluxKS(), WithHelm(&config.HelmSettings{}))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := p.RenderSnapshot(t.Context(), root)
	if err != nil || !snapshot.Complete {
		t.Fatalf("RenderSnapshot(ResourceSet -> KS -> HR) = %#v, %v, want complete snapshot", snapshot, err)
	}
	got := map[string]string{}
	for _, res := range snapshot.Clusters[""].Resources() {
		got[res.GetKind()+"/"+res.GetNamespace()+"/"+res.GetName()] = ""
		if res.GetKind() == "ConfigMap" {
			value, err := res.GetFieldValue("data.message")
			if err != nil {
				t.Fatal(err)
			}
			got[res.GetKind()+"/"+res.GetNamespace()+"/"+res.GetName()] = value.(string)
		}
	}
	want := map[string]string{
		"ResourceSet/apps/regions":      "",
		"GitRepository/apps/local":      "",
		"Kustomization/apps/east":       "",
		"HelmRelease/apps/east-release": "",
		"ConfigMap/apps/east-config":    "east",
		"ConfigMap/apps/east-chart":     "east",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("RenderSnapshot(ResourceSet -> KS -> HR) inventory mismatch (-want +got):\n%s", diff)
	}
}

func TestResourceSetRunDiffChangedInputsRetractsDescendants(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	writeResourceSetTree(t, left, "    - name: east\n      path: child-east\n      message: before\n    - name: west\n      path: child-west\n      message: west\n")
	writeResourceSetTree(t, right, "    - name: east\n      path: child-east\n      message: after\n")
	p, err := newTestPreview(t, WithLocalOnly(), WithPaths([]string{"root"}, false), WithFluxKS(), WithHelm(&config.HelmSettings{}))
	if err != nil {
		t.Fatal(err)
	}
	run, err := p.RunDiff(t.Context(), DiffRunOptions{LeftPath: left, RightPath: right, RetainSnapshots: true})
	if err != nil || !run.Complete {
		t.Fatalf("RunDiff(changed ResourceSet inputs) = %#v, %v, want complete diff", run, err)
	}
	got := map[string]string{}
	for _, change := range run.Result.ToJSON().Changes {
		got[change.ObjectRef.Kind+"/"+change.ObjectRef.Namespace+"/"+change.ObjectRef.Name] = change.Action
	}
	want := map[string]string{
		"ResourceSet/apps/regions":      "modified",
		"Kustomization/apps/east":       "modified",
		"HelmRelease/apps/east-release": "modified",
		"ConfigMap/apps/east-config":    "modified",
		"ConfigMap/apps/east-chart":     "modified",
		"Kustomization/apps/west":       "deleted",
		"HelmRelease/apps/west-release": "deleted",
		"ConfigMap/apps/west-config":    "deleted",
		"ConfigMap/apps/west-chart":     "deleted",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("RunDiff(changed ResourceSet inputs) changes mismatch (-want +got):\n%s", diff)
	}
	for side, snapshot := range map[string]*Snapshot{"before": run.Before, "after": run.After} {
		wantMessage := "before"
		if side == "after" {
			wantMessage = "after"
		}
		for _, res := range snapshot.Clusters[""].Resources() {
			if res.GetKind() != "ConfigMap" || (res.GetName() != "east-config" && res.GetName() != "east-chart") {
				continue
			}
			got, err := res.GetFieldValue("data.message")
			if err != nil || got != wantMessage {
				t.Errorf("RunDiff(changed inputs) %s %s data.message = %v, %v, want %q", side, res.GetName(), got, err, wantMessage)
			}
		}
	}
}

func TestResourceSetProviderFailureSuppressesDiff(t *testing.T) {
	for _, tc := range []struct {
		name, provider string
	}{
		{name: "missing named provider"},
		{name: "dynamic provider without snapshot", provider: resourceSetPreviewProvider},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left, right := t.TempDir(), t.TempDir()
			writeResourceSetProviderFixture(t, left, resourceSetPreviewProvider+"status:\n  exportedInputs:\n    - id: east\n      message: before\n")
			writeResourceSetProviderFixture(t, right, tc.provider)
			p, err := newTestPreview(t, WithLocalOnly(), WithPaths([]string{"root"}, false))
			if err != nil {
				t.Fatal(err)
			}
			baseline, err := p.RenderSnapshot(t.Context(), left)
			if err != nil || !baseline.Complete || baseline.Clusters[""].Size() != 3 {
				t.Fatalf("RenderSnapshot(valid provider snapshot) = %#v, %v, want complete three-resource inventory", baseline, err)
			}
			failed, err := p.RenderSnapshot(t.Context(), right)
			if err == nil || failed.Complete {
				t.Errorf("RenderSnapshot(%s) = %#v, %v, want incomplete with error", tc.name, failed, err)
			}
			for _, paths := range [][2]string{{left, right}, {right, left}} {
				var output bytes.Buffer
				run, err := p.RunDiff(t.Context(), DiffRunOptions{
					LeftPath: paths[0], RightPath: paths[1], DiffWriter: &output,
					Policies: &config.PolicyConfig{Inline: []string{"invalid rego"}},
				})
				if err == nil || run == nil {
					t.Fatalf("RunDiff(%s, %v) = %#v, %v, want incomplete run with error", tc.name, paths, run, err)
				}
				if run.Complete || run.Result.TotalChanged() != 0 || len(run.Result.ToJSON().Changes) != 0 || output.Len() != 0 || run.PolicyResult != nil {
					t.Errorf("RunDiff(%s, %v) = %#v, output %q, want incomplete without changes, diff output or policy result", tc.name, paths, run, output.String())
				}
				if len(run.Warnings) == 0 || !strings.Contains(strings.Join(run.Warnings, "\n"), "settings") {
					t.Errorf("RunDiff(%s) warnings = %v, want provider settings diagnostic", tc.name, run.Warnings)
				}
			}
		})
	}
}

func TestResourceSetProviderSnapshotChangesAndEmptyRetraction(t *testing.T) {
	left, changed, empty := t.TempDir(), t.TempDir(), t.TempDir()
	writeResourceSetProviderFixture(t, left, resourceSetPreviewProvider+"status:\n  exportedInputs:\n    - id: east\n      message: before\n")
	writeResourceSetProviderFixture(t, changed, resourceSetPreviewProvider+"status:\n  exportedInputs:\n    - id: east\n      message: after\n")
	writeResourceSetProviderFixture(t, empty, resourceSetPreviewProvider+"status:\n  exportedInputs: []\n")
	p, err := newTestPreview(t, WithLocalOnly(), WithPaths([]string{"root"}, false))
	if err != nil {
		t.Fatal(err)
	}
	run, err := p.RunDiff(t.Context(), DiffRunOptions{LeftPath: left, RightPath: changed})
	if err != nil || !run.Complete {
		t.Fatalf("RunDiff(changed provider snapshot) = %#v, %v, want complete", run, err)
	}
	got := map[string]string{}
	for _, change := range run.Result.ToJSON().Changes {
		got[change.ObjectRef.Kind+"/"+change.ObjectRef.Name] = change.Action
	}
	want := map[string]string{"ResourceSetInputProvider/settings": "modified", "ConfigMap/region-east": "modified"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("RunDiff(changed provider snapshot) changes mismatch (-want +got):\n%s", diff)
	}
	run, err = p.RunDiff(t.Context(), DiffRunOptions{LeftPath: changed, RightPath: empty, RetainSnapshots: true})
	if err != nil || !run.Complete {
		t.Fatalf("RunDiff(empty provider snapshot) = %#v, %v, want complete", run, err)
	}
	got = map[string]string{}
	for _, change := range run.Result.ToJSON().Changes {
		got[change.ObjectRef.Kind+"/"+change.ObjectRef.Name] = change.Action
	}
	want = map[string]string{"ResourceSetInputProvider/settings": "modified", "ConfigMap/region-east": "deleted"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("RunDiff(empty provider snapshot) changes mismatch (-want +got):\n%s", diff)
	}
	if got := run.After.Clusters[""].Size(); got != 2 {
		t.Errorf("RunDiff(empty provider snapshot) target inventory size = %d, want two controls and no stale output", got)
	}
}

const resourceSetPreviewProvider = `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSetInputProvider
metadata: {name: settings, namespace: apps}
spec: {type: GitHubBranch}
`

func writeResourceSetProviderFixture(t *testing.T, root, provider string) {
	t.Helper()
	writePreviewFile(t, root, "root/resourceset.yaml", `apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata: {name: regions, namespace: apps}
spec:
  inputsFrom:
    - kind: ResourceSetInputProvider
      name: settings
  resourcesTemplate: |
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: region-<< inputs.id >>
      namespace: << inputs.provider.namespace >>
    data:
      message: << inputs.message | quote >>
`)
	if provider != "" {
		writePreviewFile(t, root, "root/provider.yaml", provider)
	}
}

func writeResourceSetTree(t *testing.T, root, inputs string) {
	t.Helper()
	writePreviewFile(t, root, "root/resources.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata: {name: local, namespace: apps}
spec: {url: '.'}
---
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSet
metadata: {name: regions, namespace: apps}
spec:
  inputs:
`+inputs+`  resourcesTemplate: |
    apiVersion: kustomize.toolkit.fluxcd.io/v1
    kind: Kustomization
    metadata:
      name: << inputs.name >>
      namespace: apps
    spec:
      path: << inputs.path >>
      sourceRef: {kind: GitRepository, name: local}
      postBuild:
        substitute:
          MESSAGE: << inputs.message | quote >>
`)
	for _, region := range []string{"east", "west"} {
		writePreviewFile(t, root, "child-"+region+"/resources.yaml", `apiVersion: v1
kind: ConfigMap
metadata: {name: `+region+`-config, namespace: apps}
data: {message: '${MESSAGE}'}
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: `+region+`-release, namespace: apps}
spec:
  releaseName: `+region+`-chart
  chart:
    spec:
      chart: chart
      sourceRef: {kind: GitRepository, name: local}
  values: {message: '${MESSAGE}'}
`)
	}
	writePreviewFile(t, root, "chart/Chart.yaml", "apiVersion: v2\nname: regional\nversion: 0.1.0\n")
	writePreviewFile(t, root, "chart/templates/cm.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}
  namespace: {{ .Release.Namespace }}
data:
  message: {{ .Values.message | quote }}
`)
}
