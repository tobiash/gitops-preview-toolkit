package helm

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	helmcli "helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/repo/v1"
)

func TestLocalRunnerNeverAcquiresCharts(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	root := t.TempDir()
	writeChartFile(t, root, "Chart.yaml", "apiVersion: v2\nname: local\nversion: 0.1.0\ndependencies:\n- name: missing\n  version: 1.0.0\n  repository: "+server.URL+"\n")
	runner := NewRunner(helmcli.New(), logr.Discard())
	runner.SetLocalOnly(root)
	for _, task := range []RenderTask{
		{chart: "remote", repo: repo.Entry{URL: server.URL}},
		{chart: "remote", repo: repo.Entry{URL: server.URL}, isOCI: true},
		{localChartPath: root, releaseName: "local", namespace: "default"},
	} {
		if resources, err := runner.renderChart(t.Context(), &task); err == nil || resources != nil {
			t.Errorf("renderChart(%#v) = %v, %v, want denied acquisition", task, resources, err)
		}
	}
	if requests.Load() != 0 {
		t.Errorf("local runner made %d network requests, want zero", requests.Load())
	}
}

func TestLocalChartSchemaReferences(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	root := t.TempDir()
	writeChartFile(t, root, "Chart.yaml", "apiVersion: v2\nname: local\nversion: 0.1.0\n")
	runner := NewRunner(helmcli.New(), logr.Discard())
	runner.SetLocalOnly(root)
	for _, schema := range []string{
		fmt.Sprintf(`{"$ref":%q}`, server.URL),
		fmt.Sprintf(`{"$schema":%q}`, server.URL),
		`{"$ref":"file:///etc/passwd"}`,
		`{"$schema":"json-schema.org/draft-07/schema"}`,
		`{"$schema":"https://http://json-schema.org/draft-07/schema"}`,
		`{"properties":{"nested":{"$dynamicRef":"https://example.invalid/schema"}}}`,
		`{"$id":"file:///etc/","$ref":"#test"}`,
	} {
		writeChartFile(t, root, "values.schema.json", schema)
		if _, err := runner.renderChart(t.Context(), &RenderTask{localChartPath: root, releaseName: "local"}); err == nil {
			t.Errorf("renderChart(schema %s) = nil, want local-only denial", schema)
		}
	}
	writeChartFile(t, root, "values.schema.json", `{"$schema":"http://json-schema.org/draft-07/schema#","definitions":{"local":{"type":"object"}},"$ref":"#/definitions/local"}`)
	if _, err := runner.renderChart(t.Context(), &RenderTask{localChartPath: root, releaseName: "local"}); err != nil {
		t.Errorf("renderChart(in-document schema) = %v, want success", err)
	}
	parent := &chart.Chart{Metadata: &chart.Metadata{Name: "parent"}}
	parent.AddDependency(&chart.Chart{Metadata: &chart.Metadata{Name: "child"}, Schema: []byte(`{"$ref":"https://example.invalid/schema"}`)})
	if err := validateLocalChart(parent); err == nil {
		t.Error("validateLocalChart(subchart remote schema) = nil, want denial")
	}
	if requests.Load() != 0 {
		t.Errorf("schema validation made %d HTTP requests, want zero", requests.Load())
	}
}

func TestLocalRunnerRejectsChartSymlinksAndPostrendererPlugins(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeChartFile(t, root, "Chart.yaml", "apiVersion: v2\nname: local\nversion: 0.1.0\n")
	writeChartFile(t, outside, "secret", "private")
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "leak")); err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(helmcli.New(), logr.Discard())
	runner.SetLocalOnly(root)
	if _, err := runner.renderChart(t.Context(), &RenderTask{localChartPath: root}); err == nil {
		t.Fatal("renderChart(chart symlink) = nil, want denial")
	}
	plugin := &localForbiddenPostRenderer{}
	if _, err := localPostRenderer(plugin); err == nil || plugin.called {
		t.Fatalf("localPostRenderer(plugin) = %v, called=%v, want rejection without execution", err, plugin.called)
	}
}

type localForbiddenPostRenderer struct{ called bool }

func (p *localForbiddenPostRenderer) Run(b *bytes.Buffer) (*bytes.Buffer, error) {
	p.called = true
	return b, nil
}
