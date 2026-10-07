package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/filter"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	"github.com/tobiash/gitops-preview-toolkit/pkg/pluginhost"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

// The real host runs fixed-point discovery against this deterministic service.
// Each session fixes its desired output so normalization varies across renders,
// never within a host evaluation.
type logicalService struct {
	opens       []plugin.OpenRequest
	sessions    map[string][]plugin.Resource
	resources   func(plugin.OpenRequest, int) []plugin.Resource
	diagnostics []plugin.Diagnostic
}

func (s *logicalService) Describe(context.Context, *plugin.DescribeRequest) (*plugin.DescribeResponse, error) {
	return &plugin.DescribeResponse{Name: "flux", ProtocolVersion: plugin.ProtocolVersion}, nil
}

func (s *logicalService) OpenRender(_ context.Context, request *plugin.OpenRequest) (*plugin.OpenResponse, error) {
	s.opens = append(s.opens, *request)
	id := fmt.Sprint(len(s.opens))
	s.sessions[id] = s.resources(*request, len(s.opens))
	return &plugin.OpenResponse{Session: id}, nil
}

func (s *logicalService) Expand(_ context.Context, request *plugin.ExpandRequest) (*plugin.ExpandResponse, error) {
	return &plugin.ExpandResponse{
		Expansions:  []plugin.Expansion{{ID: "source", Trigger: "root", Resources: s.sessions[request.Session]}},
		Diagnostics: s.diagnostics,
		Evidence:    []json.RawMessage{json.RawMessage(`{"ready":false}`)},
	}, nil
}

func (s *logicalService) CloseRender(_ context.Context, request *plugin.CloseRequest) (*plugin.CloseResponse, error) {
	delete(s.sessions, request.Session)
	return &plugin.CloseResponse{}, nil
}

func logicalPreview(t *testing.T, s *logicalService, opts ...Opt) *Preview {
	t.Helper()
	s.sessions = make(map[string][]plugin.Resource)
	host, err := pluginhost.NewWithStarter([]plugin.Command{{Name: "flux"}},
		func(context.Context, plugin.Command) (plugin.Service, func() error, error) { return s, nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	p := &Preview{host: host, paths: []string{"."}, log: logr.Discard()}
	for _, opt := range opts {
		if err := opt(p); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	return p
}

func unnamed(slot, value string) plugin.Resource {
	return plugin.Resource{
		ID: "logical:parent/" + slot, Logical: true,
		YAML:       "apiVersion: example.org/v1\nkind: Database\nmetadata:\n  namespace: apps\nspec:\n  value: " + value + "\n",
		Provenance: plugin.Provenance{Kind: "Composite", Name: "parent", Path: "composition.yaml"},
	}
}

func namedFixture(t *testing.T, value string) plugin.Resource {
	t.Helper()
	r, err := plugin.ParseResource([]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: settings\ndata:\n  value: "+value+"\n"),
		plugin.Provenance{Kind: "Path", Path: "roots", Text: "exact source"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLogicalRenderedSnapshotsCombinedComparison(t *testing.T) {
	resources := []plugin.Resource{namedFixture(t, "before"), unnamed("keep", "same"), unnamed("change", "before"), unnamed("remove", "old")}
	s := &logicalService{resources: func(plugin.OpenRequest, int) []plugin.Resource { return resources }}
	p := logicalPreview(t, s, WithClusterPaths(map[string][]string{"east": {"."}, "west": {"."}}))
	root := t.TempDir()
	before, err := p.RenderSnapshot(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	resources = []plugin.Resource{unnamed("add", "new"), unnamed("change", "after"), namedFixture(t, "after"), unnamed("keep", "same")}
	after, err := p.RenderSnapshot(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	result, err := CompareSnapshots(t.Context(), before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Added) != 2 || len(result.Deleted) != 2 || len(result.Modified) != 4 {
		t.Fatalf("combined result = %+v", result.Summary())
	}
	if result.Summary().ByKind["Database"] != 6 || result.Summary().ClusterBreakdown["east"].Total != 4 {
		t.Fatalf("logical changes missing from summary: %+v", result.Summary())
	}
	for _, change := range result.ToJSON().Changes {
		if change.ObjectRef.Kind == "Database" {
			if change.LogicalID == "" || change.ObjectRef.Name != "" || !strings.Contains(change.UnifiedDiff, change.LogicalID) {
				t.Fatalf("logical JSON lost identity: %+v", change)
			}
			if change.Provenance.Path != "composition.yaml" {
				t.Fatalf("lost origin: %+v", change)
			}
		}
	}
	for _, change := range result.Changes() {
		if change.LogicalID != "" && (change.Name != "" || change.ID.Name != "") {
			t.Fatalf("invented name: %+v", change)
		}
	}
	var raw bytes.Buffer
	written, err := compareSnapshots(t.Context(), before, after, &raw)
	if err != nil || !reflect.DeepEqual(written.ToJSON(), result.ToJSON()) {
		t.Fatalf("raw comparison: %v", err)
	}
	for _, id := range []string{"logical:parent/add", "logical:parent/remove", "logical:parent/change", "ConfigMap"} {
		if !strings.Contains(raw.String(), id) {
			t.Fatalf("raw output omitted %s: %s", id, raw.String())
		}
	}
	if len(before.Evidence["east"]) != 1 || before.Clusters["east"].ProvenanceForID(before.Clusters["east"].Resources()[0].CurId()).Path != "roots" {
		t.Fatal("snapshot lost evidence or structured named provenance")
	}
	after.Complete = false
	raw.Reset()
	blocked, err := compareSnapshots(t.Context(), before, after, &raw)
	if err == nil || blocked.TotalChanged() != 0 || raw.Len() != 0 {
		t.Fatal("incomplete inventory exposed changes")
	}
}

func TestLogicalRenderFormatsAndFilters(t *testing.T) {
	for _, clustered := range []bool{false, true} {
		t.Run(fmt.Sprintf("clustered=%t", clustered), func(t *testing.T) {
			crd := unnamed("crd", "skip")
			crd.YAML = "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n"
			s := &logicalService{resources: func(plugin.OpenRequest, int) []plugin.Resource {
				return []plugin.Resource{unnamed("z", "!!str 001"), crd, namedFixture(t, "named"), unnamed("a", "random")}
			}}
			p := logicalPreview(t, s, WithSort(), WithExcludeCRDs(), WithSOPSDecrypt(), WithFilterConfig(&filter.FilterConfig{
				Filters: []filter.KFilter{{Filter: &filter.FieldNormalizer{
					Match:      filter.MatchCriteria{Kind: "Database"},
					FieldPaths: []filter.FieldPath{{Path: []string{"spec", "value"}, Placeholder: "normalized"}},
				}}},
			}))
			if clustered {
				p.clusterPaths = map[string][]string{"east": {"."}}
			}
			root := t.TempDir()
			var output bytes.Buffer
			if err := p.Render(t.Context(), root, &output); err != nil {
				t.Fatal(err)
			}
			decoder := yaml.NewDecoder(strings.NewReader(output.String()))
			count, unnamedCount := 0, 0
			for {
				object := map[string]any{}
				if err := decoder.Decode(&object); err == io.EOF {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				if len(object) == 0 {
					continue
				}
				count++
				if object["kind"] == "Database" {
					unnamedCount++
					if object["metadata"].(map[string]any)["name"] != nil {
						t.Fatal("YAML acquired metadata.name")
					}
					if object["spec"].(map[string]any)["value"] != "normalized" {
						t.Fatal("logical field filter skipped")
					}
				}
			}
			if count != 3 || unnamedCount != 2 {
				t.Fatalf("YAML inventory: %s", output.String())
			}
			output.Reset()
			if err := p.RenderJSON(t.Context(), root, &output); err != nil {
				t.Fatal(err)
			}
			var list struct {
				Items []map[string]any `json:"items"`
			}
			if err := json.Unmarshal(output.Bytes(), &list); err != nil {
				t.Fatal(err)
			}
			if len(list.Items) != 3 {
				t.Fatalf("JSON omitted outputs: %s", output.String())
			}
			for _, object := range list.Items {
				if object["kind"] == "Database" && object["metadata"].(map[string]any)["name"] != nil {
					t.Fatal("JSON acquired metadata.name")
				}
				if clustered && object["_fmp_cluster"] != "east" {
					t.Fatal("logical JSON lost cluster")
				}
			}
			snapshot, err := p.RenderSnapshot(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			cluster := ""
			if clustered {
				cluster = "east"
			}
			if snapshot.Logical[cluster][0].ID != "logical:parent/a" || len(snapshot.Logical[cluster]) != 2 {
				t.Fatal("logical sort or CRD exclusion skipped")
			}
		})
	}
}

func TestLogicalNormalizationFreshSessions(t *testing.T) {
	s := &logicalService{resources: func(_ plugin.OpenRequest, pass int) []plugin.Resource {
		return []plugin.Resource{unnamed("changing", fmt.Sprintf("!!str %03d", pass)), unnamed("stable", "same")}
	}}
	p := logicalPreview(t, s)
	root := t.TempDir()
	if err := p.Render(t.Context(), root, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := p.RenderSnapshot(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	diagnosis, err := p.DiagnoseNormalization(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnosis.Findings) != 1 || diagnosis.Findings[0].Diff.LogicalID != "logical:parent/changing" || diagnosis.Findings[0].Diff.Name != "" {
		t.Fatalf("logical normalization findings: %+v", diagnosis)
	}
	var config bytes.Buffer
	if err := p.DetectPermadiffs(t.Context(), root, &config); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(config.String(), "Database") || !strings.Contains(config.String(), "value") {
		t.Fatalf("logical filter config: %s", config.String())
	}
	for i, request := range s.opens {
		if request.Fresh != (i >= 2) {
			t.Fatalf("open %d Fresh=%t", i, request.Fresh)
		}
	}
	if err := WithFilterYAML(config.String())(p); err != nil {
		t.Fatal(err)
	}
	before, err := p.RenderSnapshot(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	after, err := p.RenderSnapshot(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	result, err := CompareSnapshots(t.Context(), before, after)
	if err != nil || result.TotalChanged() != 0 {
		t.Fatalf("normalized logical diff: %+v, %v", result, err)
	}
}

func TestLogicalDiagnosticsRetainEvidenceSuppressChanges(t *testing.T) {
	s := &logicalService{resources: func(plugin.OpenRequest, int) []plugin.Resource { return []plugin.Resource{unnamed("one", "value")} }}
	p := logicalPreview(t, s)
	root := t.TempDir()
	before, err := p.RenderSnapshot(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	s.diagnostics = []plugin.Diagnostic{{Severity: "error", Code: "evaluation", Message: "composition failed"}}
	after, err := p.RenderSnapshot(t.Context(), root)
	if err == nil || after.Complete || len(after.Evidence[""]) != 1 {
		t.Fatalf("incomplete snapshot lost diagnostics/evidence: %+v, %v", after, err)
	}
	result, err := CompareSnapshots(t.Context(), before, after)
	if err == nil || result.TotalChanged() != 0 {
		t.Fatal("incomplete logical outputs inferred deletions")
	}
}

func TestLogicalOutputSelectionAndTypedValues(t *testing.T) {
	selected := unnamed("selected", "!!str 001")
	selected.YAML = strings.Replace(selected.YAML, "  namespace: apps\n",
		"  namespace: apps\n  labels:\n    helm.toolkit.fluxcd.io/name: selected\n", 1)
	s := &logicalService{resources: func(plugin.OpenRequest, int) []plugin.Resource {
		return []plugin.Resource{selected, unnamed("other", "skip")}
	}}
	p := logicalPreview(t, s, WithHelmReleaseFilter("selected"))
	root := t.TempDir()
	var output bytes.Buffer
	if err := p.Render(t.Context(), root, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "!!str 001") || strings.Contains(output.String(), "skip") {
		t.Fatalf("logical selector or tagged YAML preservation failed: %s", output.String())
	}
	output.Reset()
	if err := p.RenderJSON(t.Context(), root, &output); err != nil {
		t.Fatal(err)
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(output.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || list.Items[0]["spec"].(map[string]any)["value"] != "001" {
		t.Fatalf("logical JSON scalar type lost: %s", output.String())
	}
}

func TestLogicalSOPSDecryptsOnlyEncryptedUnnamedSecrets(t *testing.T) {
	for _, test := range []struct {
		name, kind        string
		encrypted         bool
		omitDecryptedName bool
	}{
		{name: "plain Secret", kind: "Secret"},
		{name: "non-Secret sops field", kind: "Database", encrypted: true},
		{name: "encrypted unnamed Secret", kind: "Secret", encrypted: true},
		{name: "encrypted Secret without name field", kind: "Secret", encrypted: true, omitDecryptedName: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			resource := unnamed("secret", "value")
			resource.YAML = strings.Replace(resource.YAML, "kind: Database", "kind: "+test.kind, 1)
			resource.YAML += "data:\n  key: ENC[encrypted]\n"
			if test.encrypted {
				resource.YAML += "sops: {mac: encrypted}\n"
			}
			decrypted := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: \"\"\n  namespace: apps\n  generateName: child-\ndata:\n  key: cGxhaW4=\n"
			if test.omitDecryptedName {
				decrypted = strings.Replace(decrypted, "  name: \"\"\n", "", 1)
			}
			captured := fakeLogicalSOPS(t, decrypted, 0)
			s := &logicalService{resources: func(plugin.OpenRequest, int) []plugin.Resource { return []plugin.Resource{resource} }}
			p := logicalPreview(t, s, WithSOPSDecrypt())
			root := t.TempDir()
			snapshot, err := p.RenderSnapshot(t.Context(), root)
			if err != nil {
				t.Fatal(err)
			}
			if !snapshot.Complete || snapshot.Clusters[""].Size() != 0 || len(snapshot.Logical[""]) != 1 {
				t.Fatalf("unnamed resource left logical inventory: %+v", snapshot)
			}
			got := snapshot.Logical[""][0]
			if got.ID != resource.ID || !got.Logical || got.Provenance != resource.Provenance {
				t.Fatalf("decryption changed logical identity or provenance: %+v", got)
			}
			object, err := plugin.Object(got)
			if err != nil {
				t.Fatal(err)
			}
			metadata := object["metadata"].(map[string]any)
			if metadata["name"] != nil && metadata["name"] != "" {
				t.Fatalf("decryption fabricated metadata.name: %+v", metadata)
			}
			if test.encrypted && test.kind == "Secret" {
				if object["data"].(map[string]any)["key"] != "cGxhaW4=" || object["sops"] != nil || metadata["generateName"] != "child-" {
					t.Fatalf("unnamed Secret was not decrypted faithfully: %+v", object)
				}
				input, err := os.ReadFile(captured)
				if err != nil {
					t.Fatal(err)
				}
				inputObject, err := plugin.Object(plugin.Resource{YAML: string(input)})
				if err != nil || !reflect.DeepEqual(inputObject["metadata"], map[string]any{"namespace": "apps"}) {
					t.Fatalf("SOPS input was given a fabricated name: %s, %v", input, err)
				}
			} else {
				if got.YAML != resource.YAML {
					t.Fatalf("non-encrypted or non-Secret document changed: %s", got.YAML)
				}
				if _, err := os.Stat(captured); !os.IsNotExist(err) {
					t.Fatalf("SOPS executed for an ineligible resource: %v", err)
				}
			}
			var output bytes.Buffer
			if err := p.Render(t.Context(), root, &output); err != nil {
				t.Fatal(err)
			}
			rendered, err := plugin.Object(plugin.Resource{YAML: output.String()})
			if err != nil || !reflect.DeepEqual(rendered, object) {
				t.Fatalf("YAML rendering changed decrypted logical document: %s, %v", output.Bytes(), err)
			}
			output.Reset()
			if err := p.RenderJSON(t.Context(), root, &output); err != nil {
				t.Fatal(err)
			}
			var list struct {
				Items []map[string]any `json:"items"`
			}
			if err := json.Unmarshal(output.Bytes(), &list); err != nil || len(list.Items) != 1 {
				t.Fatalf("logical JSON inventory: %s, %v", output.Bytes(), err)
			}
			name := list.Items[0]["metadata"].(map[string]any)["name"]
			if name != nil && name != "" {
				t.Fatalf("JSON fabricated a name: %s", output.Bytes())
			}
			if !reflect.DeepEqual(list.Items[0], object) {
				t.Fatalf("JSON rendering changed decrypted logical document: %s", output.Bytes())
			}
		})
	}
}

// Exercise the production executable boundary without SOPS keys or a globally
// installed binary. The fixture is read literally, never interpolated into sh.
func fakeLogicalSOPS(t *testing.T, output string, exitCode int) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("SOPS executable fixture requires a Unix shell")
	}
	dir := t.TempDir()
	inputPath := filepath.Join(dir, "input.yaml")
	outputPath := filepath.Join(dir, "output.yaml")
	if err := os.WriteFile(outputPath, []byte(output), 0o600); err != nil {
		t.Fatal(err)
	}
	const script = "#!/bin/sh\n[ \"$1\" = \"-d\" ] || exit 90\ncat \"$2\" > \"$SOPS_TEST_INPUT\" || exit 91\ncat \"$SOPS_TEST_OUTPUT\" || exit 92\nif [ \"$SOPS_TEST_EXIT\" != 0 ]; then printf 'fixture decryption failure\\n' >&2; fi\nexit \"$SOPS_TEST_EXIT\"\n"
	if err := os.WriteFile(filepath.Join(dir, "sops"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SOPS_TEST_INPUT", inputPath)
	t.Setenv("SOPS_TEST_OUTPUT", outputPath)
	t.Setenv("SOPS_TEST_EXIT", fmt.Sprint(exitCode))
	return inputPath
}

func TestLogicalSOPSFailureSuppressesPartialInventory(t *testing.T) {
	const decrypted = "apiVersion: v1\nkind: Secret\nmetadata:\n  name: \"\"\n  namespace: apps\ndata:\n  key: cGxhaW4=\n"
	for _, test := range []struct {
		name, output string
		exitCode     int
	}{
		{name: "decryptor failure", output: decrypted, exitCode: 1},
		{name: "malformed YAML", output: "metadata: ["},
		{name: "multiple documents", output: decrypted + "---\n" + decrypted},
		{name: "malformed trailing document", output: decrypted + "---\n["},
		{name: "fabricated name", output: strings.Replace(decrypted, `name: ""`, "name: invented", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			fakeLogicalSOPS(t, test.output, test.exitCode)
			secret := unnamed("secret", "value")
			secret.YAML = "apiVersion: v1\nkind: Secret\nmetadata:\n  namespace: apps\ndata:\n  key: ENC[encrypted]\nsops: {mac: encrypted}\n"
			s := &logicalService{resources: func(plugin.OpenRequest, int) []plugin.Resource {
				return []plugin.Resource{namedFixture(t, "valid"), unnamed("before-secret", "valid"), secret}
			}}
			p := logicalPreview(t, s, WithSOPSDecrypt())
			root := t.TempDir()
			snapshot, err := p.RenderSnapshot(t.Context(), root)
			if err == nil || snapshot == nil || snapshot.Complete {
				t.Fatalf("invalid decrypted document produced a complete snapshot: %+v, %v", snapshot, err)
			}
			if len(snapshot.Logical[""]) != 0 || (snapshot.Clusters[""] != nil && snapshot.Clusters[""].Size() != 0) {
				t.Fatalf("decryption failure exposed partial authoritative inventory: %+v", snapshot)
			}
			for _, format := range []string{"YAML", "JSON"} {
				var output bytes.Buffer
				if format == "YAML" {
					err = p.Render(t.Context(), root, &output)
				} else {
					err = p.RenderJSON(t.Context(), root, &output)
				}
				if err == nil || output.Len() != 0 {
					t.Fatalf("%s published partial inventory after decryption failure: %s, %v", format, output.Bytes(), err)
				}
			}
		})
	}
}

func TestLogicalRunDiffUsesCombinedInventory(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	s := &logicalService{
		resources: func(request plugin.OpenRequest, _ int) []plugin.Resource {
			value := "after"
			if request.Root == left {
				value = "before"
			}
			return []plugin.Resource{namedFixture(t, value), unnamed("one", value), unnamed("two", value)}
		},
		diagnostics: []plugin.Diagnostic{{Severity: "warning", Code: "readiness", Message: "not ready"}},
	}
	p := logicalPreview(t, s)
	var output bytes.Buffer
	run, err := p.RunDiff(t.Context(), DiffRunOptions{
		LeftPath: left, RightPath: right, DiffWriter: &output, RetainSnapshots: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !run.Complete || run.Summary.Total != 3 || run.Summary.ByKind["Database"] != 2 {
		t.Fatalf("run summary omitted logical outputs: %+v", run)
	}
	if output.String() != run.DiffText || !strings.Contains(run.DiffText, "logical:parent/two") {
		t.Fatal("RunDiff writer omitted logical changes")
	}
	if len(run.Warnings) != 1 || run.Warnings[0] != "not ready" {
		t.Fatalf("lost diagnostics: %+v", run.Warnings)
	}
	compared, err := CompareSnapshots(t.Context(), run.Before, run.After)
	if err != nil || !reflect.DeepEqual(compared.ToJSON(), run.Result.ToJSON()) {
		t.Fatalf("retained logical inventories differ: %v", err)
	}
	if len(s.opens) != 2 {
		t.Fatalf("unexpected rerender: %d opens", len(s.opens))
	}
}

func TestLogicalOnlyClusterAdditionAndDeletion(t *testing.T) {
	before := &Snapshot{Complete: true, Clusters: map[string]*render.Render{},
		Logical: map[string][]plugin.Resource{"gone": {unnamed("same", "value")}}}
	after := &Snapshot{Complete: true, Clusters: map[string]*render.Render{},
		Logical: map[string][]plugin.Resource{"new": {unnamed("same", "value")}}}
	result, err := CompareSnapshots(t.Context(), before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Added) != 1 || len(result.Deleted) != 1 || len(result.Modified) != 0 || !result.Clustered {
		t.Fatalf("sidecar-only clusters lost: %+v", result)
	}
	if result.Added[0].Cluster != "new" || result.Deleted[0].Cluster != "gone" {
		t.Fatalf("logical IDs crossed cluster boundaries: %+v", result)
	}
}
