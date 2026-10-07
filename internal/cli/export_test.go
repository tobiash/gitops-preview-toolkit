package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/githubaction"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	"github.com/tobiash/gitops-preview-toolkit/pkg/preview"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"sigs.k8s.io/kustomize/api/resource"
)

func exportTarget(t *testing.T, value string) *preview.Snapshot {
	t.Helper()
	r := render.NewDefaultRender(logr.Discard())
	for _, manifest := range []string{
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: named\ndata:\n  value: " + value + "\n",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: unchanged\ndata:\n  value: stable\n",
	} {
		res, err := resource.NewFactory(nil).FromBytes([]byte(manifest))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Append(res); err != nil {
			t.Fatal(err)
		}
	}
	return &preview.Snapshot{
		Complete: true, Clusters: map[string]*render.Render{"": r},
		Logical: map[string][]plugin.Resource{"": {{
			ID: "logical:parent/database", Logical: true,
			YAML: "apiVersion: example.org/v1\nkind: Database\nmetadata:\n  generateName: database-\nspec:\n  value: " + value + "\n",
		}}},
	}
}

func exportedFiles(t *testing.T, directory string) map[string]string {
	t.Helper()
	files := map[string]string{}
	if err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		files[relative] = string(data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}

func TestExportSnapshotRetainsNamedAndLogicalObjects(t *testing.T) {
	snapshot := exportTarget(t, "target")
	first, second := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")
	for _, directory := range []string{first, second} {
		if err := exportSnapshot(t.Context(), snapshot, nil, directory, false); err != nil {
			t.Fatal(err)
		}
	}
	files := exportedFiles(t, first)
	if len(files) != 3 || !reflect.DeepEqual(files, exportedFiles(t, second)) {
		t.Fatalf("exports are incomplete or nondeterministic: %v", files)
	}
	for name, data := range files {
		object, err := plugin.Object(plugin.Resource{YAML: data})
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(name, "logical-") {
			metadata := object["metadata"].(map[string]any)
			if metadata["name"] != nil || metadata["generateName"] != "database-" || data != snapshot.Logical[""][0].YAML {
				t.Fatalf("logical export changed desired object: %s", data)
			}
		}
	}
}

func TestExportChangedOnlyUsesTargetObjects(t *testing.T) {
	before, after := exportTarget(t, "before"), exportTarget(t, "after")
	changes, err := preview.CompareSnapshots(t.Context(), before, after)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "changed")
	if err := exportSnapshot(t.Context(), after, changes, directory, true); err != nil {
		t.Fatal(err)
	}
	files := exportedFiles(t, directory)
	if len(files) != 2 {
		t.Fatalf("changed-only files = %v, want named and logical modification", files)
	}
	for _, data := range files {
		if !strings.Contains(data, "value: after") || strings.Contains(data, "unchanged") {
			t.Fatalf("export did not select the exact target: %s", data)
		}
	}
}

func TestExportClusterNamesCannotEscapeDestination(t *testing.T) {
	base := exportTarget(t, "target")
	snapshot := &preview.Snapshot{
		Complete: true,
		Clusters: map[string]*render.Render{"../../outside": base.Clusters[""], "/absolute": base.Clusters[""]},
	}
	directory := filepath.Join(t.TempDir(), "export")
	if err := exportSnapshot(t.Context(), snapshot, nil, directory, false); err != nil {
		t.Fatal(err)
	}
	files := exportedFiles(t, directory)
	if len(files) != 4 {
		t.Fatalf("cluster exports collided: %v", files)
	}
	for path := range files {
		if !filepath.IsLocal(path) || !strings.HasPrefix(filepath.Dir(path), "cluster-") {
			t.Fatalf("unsafe cluster export path %q", path)
		}
	}
}

func TestExportRejectsIncompleteCancelledAndNonemptyDestinations(t *testing.T) {
	snapshot := exportTarget(t, "target")
	incomplete := *snapshot
	incomplete.Complete = false
	directory := filepath.Join(t.TempDir(), "export")
	if err := exportSnapshot(t.Context(), &incomplete, nil, directory, false); err == nil {
		t.Fatal("incomplete target exported successfully")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := exportSnapshot(ctx, snapshot, nil, directory, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled export = %v", err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("failed preflight created destination: %v", err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, "existing.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := exportSnapshot(t.Context(), snapshot, nil, directory, false); err == nil {
		t.Fatal("nonempty destination accepted")
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "keep" {
		t.Fatalf("export followed existing symlink: %s, %v", data, err)
	}
}

func TestExecuteActionExportsAndReportsFailureHonestly(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	writeFile(t, left, "manifests/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\ndata:\n  value: before\n")
	writeFile(t, right, "manifests/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app\ndata:\n  value: after\n")
	directory := filepath.Join(t.TempDir(), "export")
	req := &githubaction.Request{RepoA: left, RepoB: right, Paths: []string{"manifests"}, ExportDir: directory}
	report, changes, err := executeAction(t.Context(), logr.Discard(), req)
	if err != nil || report == nil || report.ExportDir != directory || changes.TotalChanged() != 1 {
		t.Fatalf("executeAction(export) = %#v, %#v, %v", report, changes, err)
	}
	files := exportedFiles(t, directory)
	if len(files) != 1 {
		t.Fatalf("Action export = %v", files)
	}
	for _, data := range files {
		if !strings.Contains(data, "value: after") {
			t.Fatalf("Action exported wrong source: %s", data)
		}
	}
	report, _, err = executeAction(t.Context(), logr.Discard(), req)
	if err == nil || report == nil || report.Status != githubaction.StatusError || report.ExportDir != "" || len(report.Errors) == 0 {
		t.Fatalf("executeAction(nonempty export) reported success: %#v, %v", report, err)
	}
}

func TestChangedOnlyExportRequiresComparison(t *testing.T) {
	if err := exportSnapshot(t.Context(), exportTarget(t, "target"), nil, filepath.Join(t.TempDir(), "export"), true); err == nil {
		t.Fatal("changed-only export without comparison succeeded")
	}
}

func TestExportFileLimitFailsBeforeCreatingDestination(t *testing.T) {
	snapshot := exportTarget(t, "target")
	logical := snapshot.Logical[""][0]
	resources := make([]plugin.Resource, maxExportFiles+1)
	for i := range resources {
		resources[i] = logical
		resources[i].ID = fmt.Sprintf("logical:parent/slot-%d", i)
	}
	snapshot.Logical[""] = resources
	directory := filepath.Join(t.TempDir(), "export")
	if err := exportSnapshot(t.Context(), snapshot, nil, directory, false); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("excessive export = %v", err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("limit failure created partial destination: %v", err)
	}
}
