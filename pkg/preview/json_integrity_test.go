package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/filter"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

func TestClusteredRenderJSONPreservesExpansionWarnings(t *testing.T) {
	dir := t.TempDir()
	manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: duplicate\n"
	writePreviewFile(t, dir, "a.yaml", manifest)
	writePreviewFile(t, dir, "b.yaml", manifest)
	p, err := newTestPreview(t, WithLogger(logr.Discard()), WithClusterPaths(map[string][]string{"test": {"."}}))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = p.RenderJSON(context.Background(), dir, &out)
	var expansionErr *ExpansionError
	if !errors.As(err, &expansionErr) || len(expansionErr.Errors) == 0 || len(expansionErr.Warnings) == 0 {
		t.Fatalf("RenderJSON(duplicate) error=%#v, want expansion errors and warnings", err)
	}
	p.filters = &filter.FilterConfig{Filters: []filter.KFilter{{Filter: invalidJSONMapFilter{}}}}
	out.Reset()
	err = p.RenderJSON(context.Background(), dir, &out)
	if !errors.As(err, &expansionErr) || len(expansionErr.Errors) == 0 || len(expansionErr.Warnings) == 0 {
		t.Fatalf("RenderJSON(duplicate and invalid map) error=%#v, output=%s; want errors and warnings", err, &out)
	}
	// The host suppresses an incomplete plugin inventory before output filters.
	// The separate invalid-map tests below exercise filter failures on complete input.
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(out.Bytes(), &list); err != nil || len(list.Items) != 0 {
		t.Fatalf("incomplete plugin output = %s, decode error=%v; want an empty inventory", &out, err)
	}
}

type invalidJSONMapFilter struct{}

func (invalidJSONMapFilter) Filter(nodes []*yaml.RNode) ([]*yaml.RNode, error) {
	node := nodes[0].YNode()
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "broken"},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: "not-an-int"})
	return nodes, nil
}

func TestRenderJSONRejectsInvalidResourceMap(t *testing.T) {
	for _, clustered := range []bool{false, true} {
		name := "single"
		paths := WithPaths([]string{"."}, false)
		if clustered {
			name = "clustered"
			paths = WithClusterPaths(map[string][]string{"test": {"."}})
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writePreviewFile(t, dir, "cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: broken\n")
			p, err := newTestPreview(t, WithLogger(logr.Discard()), paths,
				WithFilterConfig(&filter.FilterConfig{Filters: []filter.KFilter{{Filter: invalidJSONMapFilter{}}}}))
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if err := p.RenderJSON(context.Background(), dir, &out); err == nil || out.Len() != 0 {
				t.Fatalf("RenderJSON(invalid map) = %s, error=%v; want error without output", &out, err)
			}
		})
	}
}
