package diff

import (
	"fmt"
	"io"
	"testing"

	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
)

func TestClusteredJSONSnapshotsAndOrigins(t *testing.T) {
	manifest := func(name, value string) string {
		return fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\ndata:\n  value: %s\n", name, value)
	}
	left := makeRender(t, manifest("deleted", "old")+"---\n"+manifest("modified", "old"))
	right := makeRender(t, manifest("added", "new")+"---\n"+manifest("modified", "new"))
	left.MarkProvenanceToNew(0, "path before")
	right.MarkProvenanceToNew(0, "path after")
	r, err := DiffWithResultClustered(map[string]*render.Render{"a": left, "b": left}, map[string]*render.Render{"a": right, "b": right}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	changes := r.ToJSON().Changes
	if len(changes) != 6 {
		t.Fatalf("Changes = %v, want 6", changes)
	}
	seen := map[string]bool{}
	for _, c := range changes {
		key := c.Cluster + "/" + c.Action
		if seen[key] {
			t.Errorf("duplicate cluster/action %s", key)
		}
		seen[key] = true
		if c.Action != "added" && (c.Old == nil || c.BeforeOrigin == nil || c.BeforeOrigin.String() != "path before") {
			t.Errorf("change %s missing before snapshot/origin: %#v", key, c)
		}
		if c.Action != "deleted" && (c.New == nil || c.AfterOrigin == nil || c.AfterOrigin.String() != "path after") {
			t.Errorf("change %s missing after snapshot/origin: %#v", key, c)
		}
	}
}

func TestChangeSetRejectsDuplicateIDsAfterMutation(t *testing.T) {
	r := makeRender(t, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: b\n")
	if err := r.Resources()[1].SetName("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := ChangeSet(r, r); err == nil {
		t.Fatal("ChangeSet(duplicate IDs) succeeded, want error")
	}
}

func TestJSONAllActionsRetainOrigins(t *testing.T) {
	r := &DiffResult{}
	for _, cluster := range []string{"a", "b"} {
		for _, action := range []string{"added", "modified", "deleted"} {
			c := ResourceChange{Cluster: cluster, Name: "same", Kind: "ConfigMap", Action: action,
				Provenance: render.PathProvenance("after"), Producer: "path after"}
			switch action {
			case "added":
				r.Added = append(r.Added, c)
			case "modified":
				r.Modified = append(r.Modified, c)
			case "deleted":
				r.Deleted = append(r.Deleted, c)
			}
		}
	}
	out := r.ToJSON()
	if len(out.Changes) != 6 {
		t.Fatalf("ToJSON().Changes = %v, want six clustered changes", out.Changes)
	}
	for _, c := range out.Changes {
		if c.Cluster == "" || c.Action == "" || c.Provenance.Path != "after" {
			t.Errorf("ToJSON() change = %#v, want identity and provenance", c)
		}
	}
	empty := (&DiffResult{}).ToJSON()
	if empty.Added == nil || empty.Deleted == nil || empty.Modified == nil || empty.Changes == nil {
		t.Fatalf("ToJSON(empty) = %#v, want non-nil arrays", empty)
	}
}
