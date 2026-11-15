package helm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	chartcommon "helm.sh/helm/v4/pkg/chart/common"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"
)

func TestChartValuesFiles(t *testing.T) {
	for _, source := range []string{"git directory", "git archive", "repository archive"} {
		t.Run(source, func(t *testing.T) {
			root := t.TempDir()
			writeChartFile(t, root, "chart/Chart.yaml", "apiVersion: v2\nname: local\nversion: 0.1.0\n")
			writeChartFile(t, root, "chart/values.yaml", "defaultOnly: original\nnested: {first: default, second: default}\n")
			writeChartFile(t, root, "chart/first.yaml", "nested: {first: first, second: first}\nlist: [first]\nnullable: first\n")
			writeChartFile(t, root, "chart/last.yaml", "nested: {second: last}\nlist: [last]\nnullable: null\n")
			writeChartFile(t, root, "chart/invalid.yaml", "value: [\n")
			ch, err := loader.Load(filepath.Join(root, "chart"))
			if err != nil {
				t.Fatal(err)
			}
			if source != "git directory" {
				archive, err := chartutil.Save(ch, t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				ch, err = loader.Load(archive)
				if err != nil {
					t.Fatal(err)
				}
			}
			prefix, sourceRoot := "chart/", root
			if source == "repository archive" {
				prefix, sourceRoot = "", ""
			}
			for _, tc := range []struct {
				name    string
				paths   []string
				ignore  bool
				want    map[string]any
				wantErr bool
			}{
				{name: "no files", want: ch.Values},
				{name: "default special case", paths: []string{"./values.yaml"}, want: ch.Values},
				{name: "replacement and order", paths: []string{prefix + "first.yaml", prefix + "last.yaml"}, want: map[string]any{"nested": map[string]any{"first": "first", "second": "last"}, "list": []any{"last"}, "nullable": nil}},
				{name: "explicit defaults", paths: []string{prefix + "values.yaml", prefix + "last.yaml"}, want: map[string]any{"defaultOnly": "original", "nested": map[string]any{"first": "default", "second": "last"}, "list": []any{"last"}, "nullable": nil}},
				{name: "missing required", paths: []string{prefix + "missing.yaml"}, wantErr: true},
				{name: "invalid is not missing", paths: []string{prefix + "invalid.yaml"}, ignore: true, wantErr: true},
				{name: "all missing removes defaults", paths: []string{prefix + "missing.yaml"}, ignore: true, want: map[string]any{}},
				{name: "missing between files", paths: []string{prefix + "first.yaml", prefix + "missing.yaml", prefix + "last.yaml"}, ignore: true, want: map[string]any{"nested": map[string]any{"first": "first", "second": "last"}, "list": []any{"last"}, "nullable": nil}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					copyChart := *ch
					err := applyChartValuesFiles(&copyChart, &RenderTask{valuesFiles: tc.paths, ignoreMissingValuesFiles: tc.ignore, localSourceRoot: sourceRoot})
					if (err != nil) != tc.wantErr {
						t.Fatalf("applyChartValuesFiles(%v) error = %v, want error %t", tc.paths, err, tc.wantErr)
					}
					if err == nil {
						if diff := cmp.Diff(tc.want, copyChart.Values); diff != "" {
							t.Errorf("applyChartValuesFiles(%v) mismatch (-want +got):\n%s", tc.paths, diff)
						}
					}
				})
			}
		})
	}
}

func TestChartValuesFilesRequiresOriginalDefaults(t *testing.T) {
	ch := &chart.Chart{Files: []*chartcommon.File{{Name: "override.yaml", Data: []byte("value: custom\n")}}}
	if err := applyChartValuesFiles(ch, &RenderTask{valuesFiles: []string{"override.yaml"}}); err == nil {
		t.Error("applyChartValuesFiles(chart without values.yaml) = nil, want error matching Flux's default replacement")
	}
}

func TestSourceValuesFilesBoundaries(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	writeChartFile(t, outside, "values.yaml", "secret: private\n")
	writeChartFile(t, root, "invalid.yaml", "value: [\n")
	writeChartFile(t, root, "list.yaml", "- value\n")
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../values.yaml", filepath.Join(outside, "values.yaml"), "escape/values.yaml", "escape/missing.yaml", ".", "invalid.yaml", "list.yaml"} {
		for _, ignore := range []bool{false, true} {
			ch := &chart.Chart{Values: map[string]any{"unchanged": true}, Raw: []*chartcommon.File{{Name: "values.yaml"}}}
			err := applyChartValuesFiles(ch, &RenderTask{localSourceRoot: root, valuesFiles: []string{path}, ignoreMissingValuesFiles: ignore})
			if err == nil || ch.Values["unchanged"] != true {
				t.Errorf("applyChartValuesFiles(%q, ignore=%t) = %v, values=%v, want error without mutation", path, ignore, err, ch.Values)
			}
		}
	}
}
