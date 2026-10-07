package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/diff"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	"github.com/tobiash/gitops-preview-toolkit/pkg/preview"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/resource"
	"sigs.k8s.io/kustomize/kyaml/resid"
)

// Only serialized bytes and fixed-size lifecycle metadata survive an operation.
// Decoded maps, YAML trees, and render machinery are never held by the cache.
type cachedEntry struct {
	created time.Time
	data    []byte
}

type artifact struct {
	Snapshot bool
	Clusters []string
	Records  []record
	Policies *config.PolicyConfig
	Evidence map[string][]json.RawMessage `json:",omitempty"`
}

func (c *cachedEntry) decode(id string, restoreSnapshot bool) (*entry, error) {
	var a artifact
	decoder := json.NewDecoder(bytes.NewReader(c.data))
	decoder.UseNumber()
	if err := decoder.Decode(&a); err != nil {
		return nil, err
	}
	e := &entry{id: id, records: a.Records, policies: a.Policies}
	if !a.Snapshot {
		e.changes = &diff.DiffResult{}
		for _, r := range a.Records {
			summary := r.Summary
			change := diff.ResourceChange{
				ID: recordID(summary), LogicalID: summary.LogicalID, Cluster: summary.Cluster,
				Kind: summary.Kind, Name: summary.Name, Namespace: summary.Namespace,
				Action: summary.Action, Producer: summary.Producer,
				BeforeOrigin: provenance(summary.BeforeOrigin), AfterOrigin: provenance(summary.AfterOrigin),
				Old: r.Old, New: r.New,
			}
			p := change.AfterOrigin
			if p == nil {
				p = change.BeforeOrigin
			}
			if p != nil {
				change.Provenance = *p
			}
			switch summary.Action {
			case "added":
				e.changes.Added = append(e.changes.Added, change)
			case "modified":
				e.changes.Modified = append(e.changes.Modified, change)
			case "deleted":
				e.changes.Deleted = append(e.changes.Deleted, change)
			default:
				return nil, errInput
			}
		}
		return e, nil
	}
	e.snapshot = &preview.Snapshot{Complete: true, Evidence: a.Evidence}
	if !restoreSnapshot {
		return e, nil
	}
	e.snapshot.Clusters = make(map[string]*render.Render, len(a.Clusters))
	e.snapshot.Logical = make(map[string][]plugin.Resource)
	for _, cluster := range a.Clusters {
		e.snapshot.Clusters[cluster] = render.NewDefaultRender(logr.Discard())
	}
	factory := resmap.NewFactory(resource.NewFactory(nil))
	for _, record := range a.Records {
		if record.Summary.LogicalID != "" {
			p := plugin.Provenance{}
			if o := record.Summary.AfterOrigin; o != nil {
				p = plugin.Provenance{Kind: o.Kind, Name: o.Name, Namespace: o.Namespace, Path: o.Path, Text: o.Text}
			}
			cluster := record.Summary.Cluster
			e.snapshot.Logical[cluster] = append(e.snapshot.Logical[cluster], plugin.Resource{
				ID: record.Summary.LogicalID, YAML: record.YAML, Logical: true, Provenance: p,
			})
			continue
		}
		// Reparse the original rendered YAML, not JSON maps: scalar tags,
		// quoting, ordering, and comments can affect the existing comparator.
		resources, err := factory.NewResMapFromBytes([]byte(record.YAML))
		if err != nil {
			return nil, err
		}
		r := e.snapshot.Clusters[record.Summary.Cluster]
		if r == nil || resources.Size() != 1 {
			return nil, errInput
		}
		if err := r.Append(resources.Resources()[0]); err != nil {
			return nil, err
		}
		if p := provenance(record.Summary.AfterOrigin); p != nil {
			r.SetProvenance(resources.Resources()[0].CurId(), *p)
		}
	}
	return e, nil
}

func recordID(s ResourceSummary) resid.ResId {
	group, version, ok := strings.Cut(s.APIVersion, "/")
	if !ok {
		version, group = group, ""
	}
	return resid.ResId{Gvk: resid.NewGvk(group, version, s.Kind), Name: s.Name, Namespace: s.Namespace}
}

func provenance(o *Origin) *render.Provenance {
	if o == nil {
		return nil
	}
	return &render.Provenance{Kind: o.Kind, Name: o.Name, Namespace: o.Namespace, Path: o.Path, Text: o.Text}
}

// Restore exact saved origins, keeping logical slots distinct even when their
// unnamed Kubernetes identities are identical.
func restoreOrigins(result *diff.DiffResult, before, after []record) {
	type key struct {
		cluster string
		id      resid.ResId
		logical string
	}
	left, right := make(map[key]*Origin), make(map[key]*Origin)
	identity := func(cluster string, id resid.ResId, logical string) key {
		if logical != "" {
			id = resid.ResId{}
		}
		return key{cluster: cluster, id: id, logical: logical}
	}
	for _, r := range before {
		left[identity(r.Summary.Cluster, recordID(r.Summary), r.Summary.LogicalID)] = r.Summary.AfterOrigin
	}
	for _, r := range after {
		right[identity(r.Summary.Cluster, recordID(r.Summary), r.Summary.LogicalID)] = r.Summary.AfterOrigin
	}
	for _, changes := range [][]diff.ResourceChange{result.Added, result.Modified, result.Deleted} {
		for i := range changes {
			c := &changes[i]
			k := identity(c.Cluster, c.ID, c.LogicalID)
			c.BeforeOrigin, c.AfterOrigin = provenance(left[k]), provenance(right[k])
			p := c.AfterOrigin
			if p == nil {
				p = c.BeforeOrigin
			}
			if p != nil {
				c.Provenance, c.Producer = *p, p.String()
			}
		}
	}
}
