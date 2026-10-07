package agent

import (
	"encoding/json"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	"github.com/tobiash/gitops-preview-toolkit/pkg/preview"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
)

func logicalSnapshot(value string, slots ...string) *preview.Snapshot {
	snapshot := &preview.Snapshot{
		Complete: true, Clusters: map[string]*render.Render{},
		Logical:  map[string][]plugin.Resource{},
		Evidence: map[string][]json.RawMessage{"east": {json.RawMessage(`{"ready":false,"status":{"phase":"evaluated"}}`)}},
	}
	for _, cluster := range []string{"east", "west"} {
		snapshot.Clusters[cluster] = render.NewDefaultRender(logr.Discard())
		for _, slot := range slots {
			snapshot.Logical[cluster] = append(snapshot.Logical[cluster], plugin.Resource{
				ID: "logical:parent/" + slot, Logical: true,
				YAML:       "# retained output\napiVersion: example.io/v1\nkind: Bucket\nmetadata:\n  generateName: bucket-\nspec:\n  value: " + value + "\n  precise: 9007199254740993\n  quoted: '007'\n",
				Provenance: plugin.Provenance{Kind: "CrossplaneRender", Name: slot, Path: cluster + "/" + value},
			})
		}
	}
	return snapshot
}

func TestLogicalHandlesRoundTripCompareAndInspect(t *testing.T) {
	t.Parallel()
	s := newTestService(t, t.TempDir(), Options{})
	before := logicalSnapshot("old", "b", "a", "removed")
	after := logicalSnapshot("new", "added", "a", "b")
	entries := []*entry{}
	for _, snapshot := range []*preview.Snapshot{before, after} {
		e, err := snapshotEntry(snapshot, nil)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}
	if err := s.store(entries...); err != nil {
		t.Fatal(err)
	}
	for i, e := range entries {
		decoded, err := s.entries[e.id].decode(e.id, true)
		if err != nil {
			t.Fatal(err)
		}
		original := []*preview.Snapshot{before, after}[i]
		if !reflect.DeepEqual(decoded.snapshot.Evidence, original.Evidence) {
			t.Fatal("evaluation evidence lost on cache round trip")
		}
		for cluster, resources := range original.Logical {
			if decoded.snapshot.Clusters[cluster].Size() != 0 {
				t.Fatal("logical resources entered named inventory")
			}
			for _, want := range resources {
				found := false
				for _, got := range decoded.snapshot.Logical[cluster] {
					if got.ID == want.ID {
						found = reflect.DeepEqual(got, want)
					}
				}
				if !found {
					t.Fatalf("logical YAML or origin lost: %+v", want)
				}
			}
		}
	}
	page := execute(t, s, Request{Operation: "query", ID: entries[0].id}).Data.(QueryData)
	if page.Total != 6 || page.Items[0].LogicalID != "logical:parent/a" || page.Items[1].LogicalID != "logical:parent/b" {
		t.Fatalf("logical inventory ordering: %+v", page)
	}
	ids := map[string]bool{}
	for _, item := range page.Items {
		if item.Name != "" || item.ResourceID == "" || ids[item.ResourceID] {
			t.Fatalf("ambiguous or synthesized identity: %+v", item)
		}
		ids[item.ResourceID] = true
		inspection := execute(t, s, Request{Operation: "inspect", ID: entries[0].id, ResourceID: item.ResourceID}).Data.(InspectData)
		if inspection.LogicalID != item.LogicalID || inspection.New["metadata"].(map[string]any)["name"] != nil {
			t.Fatalf("inspection invented a name: %+v", inspection)
		}
		if inspection.New["spec"].(map[string]any)["precise"] != json.Number("9007199254740993") {
			t.Fatal("logical number lost precision")
		}
	}
	comparison := execute(t, s, Request{Operation: "compare", BeforeID: entries[0].id, AfterID: entries[1].id}).Data.(PreviewData)
	if comparison.Summary != (Summary{Added: 2, Modified: 4, Deleted: 2, Total: 8}) {
		t.Fatalf("logical comparison: %+v", comparison)
	}
	changes, err := s.entries[comparison.ID].decode(comparison.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	want, err := preview.CompareSnapshots(t.Context(), before, after)
	if err != nil {
		t.Fatal(err)
	}
	left, err := s.entries[entries[0].id].decode(entries[0].id, true)
	if err != nil {
		t.Fatal(err)
	}
	right, err := s.entries[entries[1].id].decode(entries[1].id, true)
	if err != nil {
		t.Fatal(err)
	}
	got, err := preview.CompareSnapshots(t.Context(), left.snapshot, right.snapshot)
	if err != nil {
		t.Fatal(err)
	}
	restoreOrigins(got, left.records, right.records)
	if !reflect.DeepEqual(got.ToJSON(), want.ToJSON()) {
		t.Fatal("snapshot comparison YAML, identities or origins changed after cache round trip")
	}
	for _, c := range changes.changes.ToJSON().Changes {
		if c.LogicalID == "" || c.ObjectRef.Name != "" || !strings.Contains(c.UnifiedDiff, c.LogicalID) {
			t.Fatalf("cached diff lost logical identity: %+v", c)
		}
	}
	for _, c := range changes.changes.Modified {
		if c.BeforeOrigin.Name != c.AfterOrigin.Name || c.BeforeOrigin.Path != c.Cluster+"/old" || c.AfterOrigin.Path != c.Cluster+"/new" {
			t.Fatalf("same-kind slots collided: %+v", c)
		}
	}
}

func TestLogicalEvidenceBudgetAndIncompleteSnapshots(t *testing.T) {
	t.Parallel()
	s := newTestService(t, t.TempDir(), Options{MaxBytes: 4096})
	snapshot := logicalSnapshot("old", "a")
	snapshot.Complete = false
	if _, err := snapshotEntry(snapshot, nil); err == nil {
		t.Fatal("incomplete snapshot accepted")
	}
	if err := s.store(&entry{snapshot: snapshot}); err != errInput {
		t.Fatalf("incomplete store: %v", err)
	}
	snapshot.Complete = true
	snapshot.Evidence["east"] = []json.RawMessage{json.RawMessage(`{"detail":"` + strings.Repeat("x", 4096) + `"}`)}
	e, err := snapshotEntry(snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store(e); err != errCapacity || s.bytes != 0 || len(s.entries) != 0 {
		t.Fatalf("evidence escaped byte bound: err=%v bytes=%d", err, s.bytes)
	}
}

func TestStartupPluginOptionsAndRepositoryConfig(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, ".gitops-preview.yaml", "paths: [manifests]\ncrossplane:\n  enabled: true\n  command: /repository/never-run\n  engine: /repository/never-run\nplugin-commands:\n  - command: /repository/never-run\n")
	writeFixture(t, root, ".fmp.yaml", "paths: [missing]\n")
	writeFixture(t, root, "manifests/config.yaml", configMap)
	flux, err := exec.LookPath("gitops-preview-flux")
	if err != nil {
		t.Fatal(err)
	}
	for _, trusted := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "trusted"}[trusted], func(t *testing.T) {
			commands := []plugin.Command{{Name: "flux", Command: flux, Config: json.RawMessage(`{}`)}}
			s := newTestService(t, root, Options{Trusted: trusted, PluginCommands: commands})
			commands[0].Command = "/mutated/never-run"
			commands[0].Config[0] = '!'
			data := execute(t, s, Request{Operation: "discover"}).Data.(DiscoverData)
			if !data.ConfigFound || !reflect.DeepEqual(data.Paths, []string{"manifests"}) {
				t.Fatalf("new config precedence: %+v", data)
			}
			r := execute(t, s, Request{Operation: "render"}).Data.(RenderData)
			if r.ResourceCount != 1 {
				t.Fatalf("startup plugin render: %+v", r)
			}
		})
	}
}

func TestStartupPluginValidationAndBorrowedRollback(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{name: "missing executable", opts: Options{PluginCommands: []plugin.Command{{Name: "flux"}}}},
		{name: "invalid config", opts: Options{PluginCommands: []plugin.Command{{Name: "flux", Command: "flux", Config: json.RawMessage(`[]`)}}}},
		{name: "duplicate engine", opts: Options{PluginCommands: []plugin.Command{{Name: "flux", Command: "flux"}, {Name: "flux", Command: "other"}}}},
		{name: "invalid crossplane config", opts: Options{Crossplane: true, CrossplaneConfig: json.RawMessage(`{`)}},
		{name: "duplicate crossplane", opts: Options{Crossplane: true, PluginCommands: []plugin.Command{{Name: "crossplane", Command: "crossplane"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := New(root, tc.opts)
			if err != errInput || s != nil {
				t.Fatalf("invalid startup options accepted: service=%v err=%v", s, err)
			}
		})
	}
	writeFixture(t, root, "manifests/config.yaml", configMap)
	s := newTestService(t, root, Options{})
	if _, err := preview.New(preview.WithPluginHost(nil)); err == nil {
		t.Fatal("nil borrowed host accepted")
	}
	commands := []plugin.Command{{Name: "flux", Command: "gitops-preview-flux"}}
	if _, err := preview.New(preview.WithPluginHost(s.host), preview.WithPlugins(commands)); err == nil {
		t.Fatal("invalid borrowed preview accepted")
	}
	if _, err := preview.New(preview.WithPluginHost(s.host), func(*preview.Preview) error { return errInput }); !errors.Is(err, errInput) {
		t.Fatalf("borrowed preview rollback: %v", err)
	}
	// Constructor rollback and borrower Close must leave the owner's host usable.
	renderID(t, s)
	renderID(t, s)
}
