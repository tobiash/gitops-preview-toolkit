package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/diff"
	"github.com/tobiash/gitops-preview-toolkit/pkg/policy"
	"github.com/tobiash/gitops-preview-toolkit/pkg/preview"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"github.com/tobiash/k8q/pkg/engine"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

type record struct {
	Summary ResourceSummary
	Old     map[string]any
	New     map[string]any
	YAML    string `json:",omitempty"`
}

func origin(p *render.Provenance) *Origin {
	if p == nil {
		return nil
	}
	return &Origin{p.Kind, p.Name, p.Namespace, p.Path, p.Text}
}

func snapshotEntry(snapshot *preview.Snapshot, policies *config.PolicyConfig) (*entry, error) {
	if snapshot == nil || !snapshot.Complete || snapshot.Clusters == nil {
		return nil, errInput
	}
	e := &entry{snapshot: snapshot, policies: policies}
	var clusters []string
	for cluster := range snapshot.Clusters {
		clusters = append(clusters, cluster)
	}
	slices.Sort(clusters)
	for _, cluster := range clusters {
		r := snapshot.Clusters[cluster]
		if r == nil {
			return nil, errInput
		}
		for _, res := range r.Resources() {
			obj, err := res.Map()
			if err != nil {
				return nil, err
			}
			p := r.ProvenanceForID(res.CurId())
			data, err := res.AsYAML()
			if err != nil {
				return nil, err
			}
			e.records = append(e.records, record{Summary: ResourceSummary{ResourceID: opaqueID(), Cluster: cluster, APIVersion: res.GetApiVersion(), Kind: res.GetKind(), Name: res.GetName(), Namespace: res.GetNamespace(), Producer: p.String(), AfterOrigin: origin(&p)}, New: obj, YAML: string(data)})
		}
	}
	for cluster, resources := range snapshot.Logical {
		changes, err := diff.LogicalChangeSet(nil, resources)
		if err != nil {
			return nil, err
		}
		yamlByID := make(map[string]string, len(resources))
		for _, res := range resources {
			yamlByID[res.ID] = res.YAML
		}
		for _, change := range changes.Added {
			change.Cluster = cluster
			r := changeRecord(change)
			r.Summary.Action = ""
			r.YAML = yamlByID[change.LogicalID]
			e.records = append(e.records, r)
		}
	}
	sortRecords(e.records)
	return e, nil
}

func diffEntry(changes *diff.DiffResult, policies *config.PolicyConfig) *entry {
	e := &entry{changes: changes, policies: policies}
	for _, c := range changes.Changes() {
		e.records = append(e.records, changeRecord(c))
	}
	sortRecords(e.records)
	return e
}

func changeRecord(c diff.ResourceChange) record {
	version := c.ID.Version
	if c.ID.Group != "" {
		version = c.ID.Group + "/" + version
	}
	return record{
		Summary: ResourceSummary{
			ResourceID: opaqueID(), LogicalID: c.LogicalID, Cluster: c.Cluster,
			APIVersion: version, Kind: c.Kind, Name: c.Name, Namespace: c.Namespace,
			Action: c.Action, Producer: c.Producer,
			BeforeOrigin: origin(c.BeforeOrigin), AfterOrigin: origin(c.AfterOrigin),
		},
		Old: c.Old, New: c.New,
	}
}

func sortRecords(records []record) {
	slices.SortFunc(records, func(a, b record) int {
		for _, pair := range [][2]string{
			{a.Summary.Cluster, b.Summary.Cluster},
			{a.Summary.APIVersion, b.Summary.APIVersion},
			{a.Summary.Kind, b.Summary.Kind},
			{a.Summary.Namespace, b.Summary.Namespace},
			{a.Summary.Name, b.Summary.Name},
			{a.Summary.LogicalID, b.Summary.LogicalID},
		} {
			if c := strings.Compare(pair[0], pair[1]); c != 0 {
				return c
			}
		}
		return 0
	})
}

func matchOptions(q Query) (engine.MatchOptions, error) {
	if q.Action != "" && q.Action != "added" && q.Action != "modified" && q.Action != "deleted" {
		return engine.MatchOptions{}, errInput
	}
	selector, err := labels.Parse(q.Labels)
	if err != nil {
		return engine.MatchOptions{}, errInput
	}
	return engine.MatchOptions{Kind: q.Kind, Name: q.Name, Namespace: q.Namespace, Group: q.Group, Selector: selector}, nil
}

func matches(r record, q Query, opts engine.MatchOptions) bool {
	if q.Cluster != "" && q.Cluster != r.Summary.Cluster || q.Action != "" && q.Action != r.Summary.Action || q.Producer != "" && q.Producer != r.Summary.Producer {
		return false
	}
	obj := r.New
	if obj == nil {
		obj = r.Old
	}
	meta := yaml.ResourceMeta{}
	meta.APIVersion, meta.Kind, meta.Name, meta.Namespace = r.Summary.APIVersion, r.Summary.Kind, r.Summary.Name, r.Summary.Namespace
	meta.Labels = map[string]string{}
	if metadata, ok := obj["metadata"].(map[string]any); ok {
		if ls, ok := metadata["labels"].(map[string]any); ok {
			for k, v := range ls {
				if str, ok := v.(string); ok {
					meta.Labels[k] = str
				}
			}
		}
	}
	return engine.Match(meta, opts)
}

func (s *Service) query(ctx context.Context, req Request, e *entry) Response {
	if len(req.Fields) != 0 {
		return publicError(req.Operation, "query", errInput)
	}
	opts, _ := matchOptions(req.Query)
	data := QueryData{ID: e.id, Items: []ResourceSummary{}, Offset: req.Offset}
	limit := req.Limit
	if limit == 0 {
		limit = 20
	}
	for _, record := range e.records {
		if err := ctx.Err(); err != nil {
			return publicError(req.Operation, "query", err)
		}
		if !matches(record, req.Query, opts) {
			continue
		}
		index := data.Total
		data.Total++
		if index >= req.Offset && len(data.Items) < limit {
			data.Items = append(data.Items, cloneSummary(record.Summary))
		}
	}
	for {
		data.Truncated = req.Offset+len(data.Items) < data.Total
		if data.Truncated {
			next := req.Offset + len(data.Items)
			data.NextOffset = &next
		} else {
			data.NextOffset = nil
		}
		r := boundResponse(success(req.Operation, data))
		if r.Error == nil || len(data.Items) <= 1 {
			return r
		}
		data.Items = data.Items[:len(data.Items)-1]
	}
}

func cloneSummary(s ResourceSummary) ResourceSummary {
	if s.BeforeOrigin != nil {
		p := *s.BeforeOrigin
		s.BeforeOrigin = &p
	}
	if s.AfterOrigin != nil {
		p := *s.AfterOrigin
		s.AfterOrigin = &p
	}
	return s
}

func (s *Service) inspect(req Request, e *entry) Response {
	for _, r := range e.records {
		if r.Summary.ResourceID != req.ResourceID {
			continue
		}
		old, err := project(redacted(r.Old), req.Fields)
		if err != nil {
			return publicError(req.Operation, "inspect", err)
		}
		new, err := project(redacted(r.New), req.Fields)
		if err != nil {
			return publicError(req.Operation, "inspect", err)
		}
		return success(req.Operation, InspectData{e.id, cloneSummary(r.Summary), old, new})
	}
	return failure(req.Operation, "NotFound", "inspect", "Resource identity is not present in this handle.")
}

func credentialKey(key string) bool {
	key = strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "").Replace(key))
	return strings.Contains(key, "password") || strings.Contains(key, "passwd") || strings.Contains(key, "token") || strings.Contains(key, "secret") || strings.Contains(key, "privatekey") || strings.Contains(key, "apikey") || strings.Contains(key, "credential") || key == "authorization" || key == "dockerconfigjson" || key == "tlskey"
}

func redactValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		name, _ := x["name"].(string)
		for k, value := range x {
			if credentialKey(k) || strings.Contains(k, "last-applied-configuration") || k == "value" && credentialKey(name) {
				out[k] = "[REDACTED]"
			} else {
				out[k] = redactValue(value)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = redactValue(item)
		}
		return out
	default:
		return v
	}
}

func redacted(obj map[string]any) map[string]any {
	if obj == nil {
		return nil
	}
	copy := redactValue(obj).(map[string]any)
	if obj["kind"] == "Secret" && obj["apiVersion"] == "v1" {
		for _, key := range []string{"data", "stringData"} {
			if _, ok := copy[key]; ok {
				copy[key] = "[REDACTED]"
			}
		}
	}
	return copy
}

func validateFields(fields []string) error {
	if len(fields) > 100 {
		return errInput
	}
	for _, field := range fields {
		if field == "" {
			continue
		}
		if !strings.HasPrefix(field, "/") {
			return errInput
		}
		for i := 0; i < len(field); i++ {
			if field[i] == '~' {
				i++
				if i >= len(field) || field[i] != '0' && field[i] != '1' {
					return errInput
				}
			}
		}
	}
	return nil
}

func project(obj map[string]any, fields []string) (map[string]any, error) {
	if obj == nil || len(fields) == 0 {
		return obj, nil
	}
	out := make(map[string]any, len(fields))
	for _, field := range fields {
		var value any = obj
		if field != "" {
			for _, token := range strings.Split(field[1:], "/") {
				token = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
				switch v := value.(type) {
				case map[string]any:
					value = v[token]
				case []any:
					i, err := strconv.Atoi(token)
					if err != nil || i < 0 || strconv.Itoa(i) != token {
						return nil, errInput
					}
					if i >= len(v) {
						value = nil
					} else {
						value = v[i]
					}
				default:
					// Redacted parents stay redacted even when a child is requested.
					if value != "[REDACTED]" {
						value = nil
					}
				}
			}
		}
		out[field] = value
	}
	return out, nil
}

func (s *Service) check(ctx context.Context, req Request, e *entry) Response {
	if len(req.Fields) != 0 {
		return publicError(req.Operation, "check", errInput)
	}
	if e.snapshot != nil && !e.snapshot.Complete {
		return failure(req.Operation, "Incomplete", "check", "Analysis requires a complete snapshot.")
	}
	opts, _ := matchOptions(req.Query)
	data := CheckData{ID: e.id, Verdict: "passed"}
	if e.changes != nil {
		if req.Query != (Query{}) || req.MaxCPURequests != "" || req.MaxMemRequests != "" || req.MaxCPULimits != "" || req.MaxMemLimits != "" || req.RequireRequests || req.RequireLimits {
			return publicError(req.Operation, "check", errInput)
		}
		result, err := policy.Evaluate(ctx, e.changes, e.policies, "")
		if err != nil {
			return publicError(req.Operation, "policy", err)
		}
		data.ClassificationCount, data.ViolationCount = len(result.Classifications), len(result.Violations)
		if result.PolicyFailed {
			data.Verdict = "failed"
		}
		return success(req.Operation, data)
	}
	var nodes []*yaml.RNode
	var replicaWork int64
	for _, r := range e.records {
		if err := ctx.Err(); err != nil {
			return publicError(req.Operation, "check", err)
		}
		if !matches(r, req.Query, opts) {
			continue
		}
		// k8q currently adds quantities once per replica. Refuse excessive total
		// work rather than silently clipping replicas or changing accounting.
		n := int64(1)
		if spec, ok := r.New["spec"].(map[string]any); ok {
			if replicas, ok := spec["replicas"]; ok {
				b, err := json.Marshal(replicas)
				if err != nil {
					return publicError(req.Operation, "check", errInput)
				}
				n, err = strconv.ParseInt(string(b), 10, 64)
				if err != nil || n < 0 {
					return publicError(req.Operation, "check", errInput)
				}
			}
		}
		if n > 100000-replicaWork {
			return failure(req.Operation, "AnalysisLimit", "check", "Replica accounting exceeds the bounded analysis limit of 100000.")
		}
		replicaWork += n
		b, err := json.Marshal(r.New)
		if err != nil {
			return publicError(req.Operation, "check", err)
		}
		node, err := yaml.Parse(string(b))
		if err != nil {
			return publicError(req.Operation, "check", err)
		}
		nodes = append(nodes, node)
	}
	count, err := engine.CountJSON(nodes, engine.CountOptions{GroupByKind: true})
	if err != nil {
		return publicError(req.Operation, "check", err)
	}
	resources, err := engine.SumJSON(nodes, engine.SumOptions{MaxCPURequests: req.MaxCPURequests, MaxMemRequests: req.MaxMemRequests, MaxCPULimits: req.MaxCPULimits, MaxMemLimits: req.MaxMemLimits, RequireRequests: req.RequireRequests, RequireLimits: req.RequireLimits})
	if err != nil {
		if !errors.Is(err, engine.ErrAssertion) {
			return publicError(req.Operation, "check", errInput)
		}
		data.Verdict = "failed"
	}
	// Missing-resource errors can include arbitrary malformed chart values.
	if resources != nil && resources.Assertions != nil {
		for i := range resources.Assertions.MissingResources {
			resources.Assertions.MissingResources[i] = "A workload is missing required resource declarations."
		}
	}
	data.Count, data.Resources = count, resources
	return success(req.Operation, data)
}
