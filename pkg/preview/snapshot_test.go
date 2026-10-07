package preview

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/filter"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"sigs.k8s.io/kustomize/kyaml/kio"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

func TestSnapshotsDetachedAndClusterIdentity(t *testing.T) {
	root := t.TempDir()
	writePreviewFile(t, root, "cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: same\ndata:\n  value: before\n")
	p, err := newTestPreview(t, WithLocalOnly(), WithLogger(logr.Discard()), WithClusterPaths(map[string][]string{"a": {"."}, "b": {"."}}))
	if err != nil {
		t.Fatal(err)
	}
	before, err := p.RenderSnapshot(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	writePreviewFile(t, root, "cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: same\ndata:\n  value: after\n")
	after, err := p.RenderSnapshot(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	result, err := CompareSnapshots(t.Context(), before, after)
	if err != nil || result.TotalChanged() != 2 {
		t.Fatalf("CompareSnapshots() = %#v, %v, want two cluster changes", result, err)
	}
	for _, cluster := range []string{"a", "b"} {
		value, err := before.Clusters[cluster].Resources()[0].GetFieldValue("data.value")
		if err != nil || value != "before" {
			t.Errorf("before[%s].data.value = %v, %v, want before", cluster, value, err)
		}
	}
	after.Complete = false
	if result, err := CompareSnapshots(t.Context(), before, after); err == nil || result.TotalChanged() != 0 {
		t.Fatalf("CompareSnapshots(incomplete) = %#v, %v, want no changes and error", result, err)
	}
}

func TestRunDiffRetainsExactInventories(t *testing.T) {
	root := t.TempDir()
	writePreviewFile(t, root, "cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: same\n")
	calls := 0
	// A stateful filter makes any accidental rerender observably different.
	fc := &filter.FilterConfig{Filters: []filter.KFilter{{Filter: kio.FilterFunc(func(nodes []*yaml.RNode) ([]*yaml.RNode, error) {
		calls++
		for _, node := range nodes {
			if err := node.PipeE(yaml.SetAnnotation("render-pass", fmt.Sprint(calls))); err != nil {
				return nil, err
			}
		}
		return nodes, nil
	})}}}
	p, err := newTestPreview(t, WithLocalOnly(), WithPaths([]string{"."}, false), WithFilterConfig(fc))
	if err != nil {
		t.Fatal(err)
	}
	run, err := p.RunDiff(t.Context(), DiffRunOptions{LeftPath: root, RightPath: root, RetainSnapshots: true, Policies: &config.PolicyConfig{}})
	if err != nil {
		t.Fatal(err)
	}
	if run.Before == nil || run.After == nil || !run.Complete {
		t.Fatalf("RunDiff(retain) = %#v, want complete snapshots", run)
	}
	if calls != 2 || run.Result.TotalChanged() != 1 {
		t.Fatalf("RunDiff(retain) calls=%d changes=%d, want two loads and one change", calls, run.Result.TotalChanged())
	}
	compared, err := CompareSnapshots(t.Context(), run.Before, run.After)
	if err != nil || !reflect.DeepEqual(compared.ToJSON(), run.Result.ToJSON()) {
		t.Fatalf("CompareSnapshots(retained) = %#v, %v, want original result", compared, err)
	}
	run, err = p.RunDiff(t.Context(), DiffRunOptions{LeftPath: root, RightPath: root})
	if err != nil || run.Before != nil || run.After != nil {
		t.Fatalf("RunDiff(no retain) = %#v, %v, want no snapshots", run, err)
	}
}

func TestSnapshotCancellationAndMissingSource(t *testing.T) {
	p, err := newTestPreview(t, WithLocalOnly(), WithFluxKS(), WithPaths([]string{"."}, false))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writePreviewFile(t, root, "ks.yaml", "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: missing\nspec:\n  path: .\n  sourceRef:\n    kind: GitRepository\n    name: missing\n")
	snapshot, err := p.RenderSnapshot(t.Context(), root)
	if err == nil || snapshot.Complete {
		t.Fatalf("RenderSnapshot(missing source) = %#v, %v, want incomplete", snapshot, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	snapshot, err = p.RenderSnapshot(ctx, root)
	if !errors.Is(err, context.Canceled) || snapshot.Complete {
		t.Fatalf("RenderSnapshot(canceled) = %#v, %v", snapshot, err)
	}
}

func TestSnapshotRejectsInvalidResourceMap(t *testing.T) {
	root := t.TempDir()
	writePreviewFile(t, root, "cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: broken\n")
	p, err := newTestPreview(t, WithPaths([]string{"."}, false), WithFilterConfig(&filter.FilterConfig{
		Filters: []filter.KFilter{{Filter: invalidJSONMapFilter{}}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := p.RenderSnapshot(t.Context(), root)
	if err == nil || snapshot.Complete {
		t.Fatalf("RenderSnapshot(invalid map) = %#v, %v, want incomplete", snapshot, err)
	}
	// Complete is caller-mutable; comparison also validates the inventory.
	snapshot.Complete = true
	result, err := CompareSnapshots(t.Context(), snapshot, snapshot)
	if err == nil || result.TotalChanged() != 0 {
		t.Fatalf("CompareSnapshots(invalid map) = %#v, %v, want no authoritative changes", result, err)
	}
}

func TestComparisonDoesNotSilentlyDiscardLogicalResources(t *testing.T) {
	snapshot := &Snapshot{
		Complete: true,
		Clusters: map[string]*render.Render{"": render.NewDefaultRender(logr.Discard())},
		Logical: map[string][]plugin.Resource{"": {{
			ID: "composed/database", Logical: true,
			YAML: "apiVersion: example.org/v1\nkind: Database\nmetadata: {}\n",
		}}},
	}
	result, err := CompareSnapshots(t.Context(), snapshot, snapshot)
	if err != nil || result.TotalChanged() != 0 {
		t.Fatalf("CompareSnapshots(identical logical resources) = %#v, %v; want clean comparison", result, err)
	}
}
