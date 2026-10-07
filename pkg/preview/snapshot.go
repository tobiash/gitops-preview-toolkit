package preview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/diff"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
)

// Snapshot is a detached render inventory, keyed by cluster ("" for unclustered input).
// Incomplete snapshots must not be used to infer additions or deletions.
type Snapshot struct {
	Clusters map[string]*render.Render
	// Logical preserves unnamed composed resources without adding invented names
	// to the named Kubernetes inventory.
	Logical  map[string][]plugin.Resource
	Evidence map[string][]json.RawMessage
	Complete bool
	Warnings []string
}

// RenderSnapshot loads path and returns the exact inventory used for comparison.
// On failure the returned snapshot is incomplete and the error describes why.
func (p *Preview) RenderSnapshot(ctx context.Context, path string) (*Snapshot, error) {
	snapshot := &Snapshot{Clusters: make(map[string]*render.Render), Logical: make(map[string][]plugin.Resource), Evidence: make(map[string][]json.RawMessage)}
	results, err := p.loadRepo(ctx, path)
	if err != nil {
		snapshot.Warnings = []string{err.Error()}
		return snapshot, err
	}
	diagnostics := &ExpansionError{}
	for _, cluster := range sortedClusterNames(results) {
		r := results[cluster]
		diagnostics.Errors = append(diagnostics.Errors, r.errors...)
		diagnostics.Warnings = append(diagnostics.Warnings, r.warnings...)
		if p.helmReleaseName != "" {
			r.render.FilterByLabel("helm.toolkit.fluxcd.io/name", p.helmReleaseName)
		}
		p.applyOutputOptions(r.render)
		snapshot.Clusters[cluster] = r.render
		snapshot.Logical[cluster] = r.logical
		snapshot.Evidence[cluster] = r.evidence
		if _, err := diff.LogicalChangeSet(r.logical, r.logical); err != nil {
			diagnostics.Errors = append(diagnostics.Errors, fmt.Errorf("cluster %q: %w", cluster, err))
		}
		for i, res := range r.render.Resources() {
			if _, err := res.Map(); err != nil {
				diagnostics.Errors = append(diagnostics.Errors, fmt.Errorf("cluster %q resource %d: invalid resource map: %w", cluster, i+1, err))
			}
		}
	}
	snapshot.Warnings = expansionWarnings(diagnostics)
	if err := ctx.Err(); err != nil {
		snapshot.Warnings = append(snapshot.Warnings, err.Error())
		return snapshot, err
	}
	if len(diagnostics.Errors) > 0 {
		return snapshot, diagnostics
	}
	snapshot.Complete = true
	return snapshot, nil
}

// CompareSnapshots compares complete inventories without rendering or mutating them.
// Cluster identity is retained even when resource IDs are identical across clusters.
func CompareSnapshots(ctx context.Context, before, after *Snapshot) (*diff.DiffResult, error) {
	return compareSnapshots(ctx, before, after, io.Discard)
}

func compareSnapshots(ctx context.Context, before, after *Snapshot, out io.Writer) (*diff.DiffResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, snapshot := range []*Snapshot{before, after} {
		if snapshot == nil || !snapshot.Complete || snapshot.Clusters == nil {
			return &diff.DiffResult{}, fmt.Errorf("cannot compare incomplete snapshot")
		}
		for cluster, r := range snapshot.Clusters {
			if err := ctx.Err(); err != nil {
				return &diff.DiffResult{}, err
			}
			if r == nil || r.ResMap == nil {
				return &diff.DiffResult{}, fmt.Errorf("cluster %q has no render inventory", cluster)
			}
			for _, res := range r.Resources() {
				if _, err := res.Map(); err != nil {
					return &diff.DiffResult{}, fmt.Errorf("cluster %q: invalid resource: %w", cluster, err)
				}
			}
		}
	}
	clusterSet := make(map[string]bool)
	for _, snapshot := range []*Snapshot{before, after} {
		for cluster := range snapshot.Clusters {
			clusterSet[cluster] = true
		}
		for cluster := range snapshot.Logical {
			clusterSet[cluster] = true
		}
	}
	clusters := make([]string, 0, len(clusterSet))
	for cluster := range clusterSet {
		clusters = append(clusters, cluster)
	}
	sort.Strings(clusters)
	result := &diff.DiffResult{}
	if len(clusters) > 1 || (len(clusters) == 1 && clusters[0] != "") {
		result.Clustered, result.Clusters = true, clusters
	}
	for _, cluster := range clusters {
		if err := ctx.Err(); err != nil {
			return &diff.DiffResult{}, err
		}
		left, right := before.Clusters[cluster], after.Clusters[cluster]
		if left == nil {
			left = render.NewDefaultRender(logr.Discard())
		}
		if right == nil {
			right = render.NewDefaultRender(logr.Discard())
		}
		named, err := diff.ChangeSet(left, right)
		if err != nil {
			return &diff.DiffResult{}, err
		}
		logical, err := diff.LogicalChangeSet(before.Logical[cluster], after.Logical[cluster])
		if err != nil {
			return &diff.DiffResult{}, fmt.Errorf("cluster %q: %w", cluster, err)
		}
		for _, changes := range []*diff.DiffResult{named, logical} {
			for i := range changes.Added {
				changes.Added[i].Cluster = cluster
			}
			for i := range changes.Deleted {
				changes.Deleted[i].Cluster = cluster
			}
			for i := range changes.Modified {
				changes.Modified[i].Cluster = cluster
			}
			result.Added = append(result.Added, changes.Added...)
			result.Deleted = append(result.Deleted, changes.Deleted...)
			result.Modified = append(result.Modified, changes.Modified...)
		}
	}
	if err := ctx.Err(); err != nil {
		return &diff.DiffResult{}, err
	}
	result.Sort()
	result.WriteUnified(out)
	return result, nil
}

func (p *Preview) diffSnapshots(ctx context.Context, a, b string, out io.Writer) (*diff.DiffResult, *Snapshot, *Snapshot, error) {
	defer p.beginRun()()
	// Configured filters may carry mutable state, so load sequentially.
	before, leftErr := p.RenderSnapshot(ctx, a)
	if leftErr != nil {
		var expansionErr *ExpansionError
		if !errors.As(leftErr, &expansionErr) {
			return nil, before, nil, leftErr
		}
	}
	after, rightErr := p.RenderSnapshot(ctx, b)
	if rightErr != nil || leftErr != nil {
		diagnostics := &ExpansionError{}
		for _, err := range []error{leftErr, rightErr} {
			if err == nil {
				continue
			}
			var expansionErr *ExpansionError
			if errors.As(err, &expansionErr) {
				diagnostics.Errors = append(diagnostics.Errors, expansionErr.Errors...)
				diagnostics.Warnings = append(diagnostics.Warnings, expansionErr.Warnings...)
			} else {
				return nil, before, after, errors.Join(leftErr, rightErr)
			}
		}
		return &diff.DiffResult{}, before, after, diagnostics
	}
	result, err := compareSnapshots(ctx, before, after, out)
	if err == nil && len(before.Warnings)+len(after.Warnings) > 0 {
		diagnostics := &ExpansionError{}
		for _, warning := range append(append([]string(nil), before.Warnings...), after.Warnings...) {
			diagnostics.Warnings = append(diagnostics.Warnings, errors.New(warning))
		}
		err = diagnostics
	}
	return result, before, after, err
}
