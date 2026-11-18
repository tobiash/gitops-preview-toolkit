package preview

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"
)

func TestSnapshotHelmValuesFiles(t *testing.T) {
	for _, localOnly := range []bool{true, false} {
		for _, tc := range []struct {
			name, files     string
			ignore, wantErr bool
			want            string
		}{
			{name: "ordered source files", files: "[environments/first.yaml, environments/last.yaml]", want: "last:from-reference:inline:absent"},
			{name: "explicit chart defaults", files: "[chart/values.yaml, environments/last.yaml]", want: "last:from-reference:inline:default"},
			{name: "default special case", files: "[values.yaml]", want: "default:from-reference:inline:default"},
			{name: "missing required", files: "[environments/missing.yaml]", wantErr: true},
			{name: "missing ignored", files: "[environments/first.yaml, environments/missing.yaml, environments/last.yaml]", ignore: true, want: "last:from-reference:inline:absent"},
			{name: "all missing", files: "[environments/missing.yaml]", ignore: true, want: "absent:from-reference:inline:absent"},
			{name: "bad YAML never ignored", files: "[environments/invalid.yaml]", ignore: true, wantErr: true},
			{name: "directory never ignored", files: "[environments]", ignore: true, wantErr: true},
			{name: "traversal never ignored", files: "[../outside.yaml]", ignore: true, wantErr: true},
			{name: "symlink never ignored", files: "[environments/escape.yaml]", ignore: true, wantErr: true},
		} {
			t.Run(fmt.Sprintf("%s/local=%t", tc.name, localOnly), func(t *testing.T) {
				root := t.TempDir()
				writeLocalHelmSource(t, root, ".", "chart", fmt.Sprintf("      valuesFiles: %s\n      ignoreMissingValuesFiles: %t\n  valuesFrom: [{kind: ConfigMap, name: release-values}]\n  values: {inline: inline}\n", tc.files, tc.ignore))
				writePreviewFile(t, root, "root/values.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: release-values, namespace: default}\ndata:\n  values.yaml: |\n    reference: from-reference\n    inline: reference\n")
				writePreviewFile(t, root, "chart/values.yaml", "ordered: default\nreference: default\ninline: default\ndefaultOnly: default\n")
				writePreviewFile(t, root, "environments/first.yaml", "ordered: first\nreference: first\ninline: first\n")
				writePreviewFile(t, root, "environments/last.yaml", "ordered: last\nreference: last\ninline: last\n")
				writePreviewFile(t, root, "environments/invalid.yaml", "ordered: [\n")
				outside := t.TempDir()
				writePreviewFile(t, outside, "outside.yaml", "ordered: private\n")
				if err := os.Symlink(filepath.Join(outside, "outside.yaml"), filepath.Join(root, "environments/escape.yaml")); err != nil {
					t.Fatal(err)
				}
				writePreviewFile(t, root, "chart/templates/cm.yaml", `apiVersion: v1
kind: ConfigMap
metadata: {name: rendered-values}
data:
  value: {{ printf "%s:%s:%s:%s" (.Values.ordered | default "absent") (.Values.reference | default "absent") (.Values.inline | default "absent") (.Values.defaultOnly | default "absent") | quote }}
`)
				opts := []Opt{WithPaths([]string{"root"}, false), WithFluxKS(), WithHelm(&config.HelmSettings{}), WithStrictInputs()}
				if localOnly {
					opts = append(opts, WithLocalOnly())
				} else {
					opts = append(opts, WithGitRepo())
				}
				p, err := newTestPreview(t, opts...)
				if err != nil {
					t.Fatal(err)
				}
				snapshot, err := p.RenderSnapshot(t.Context(), root)
				if tc.wantErr {
					if err == nil || snapshot.Complete {
						t.Fatalf("RenderSnapshot(%s) = complete %t, %v, want incomplete error", tc.name, snapshot.Complete, err)
					}
					for _, res := range snapshot.Clusters[""].Resources() {
						if res.GetKind() == "ConfigMap" && res.GetName() == "rendered-values" {
							t.Errorf("RenderSnapshot(%s) published output from a chart with invalid values files", tc.name)
						}
					}
					return
				}
				if err != nil || !snapshot.Complete {
					t.Fatalf("RenderSnapshot(%s) = %#v, %v, want complete", tc.name, snapshot, err)
				}
				for _, res := range snapshot.Clusters[""].Resources() {
					if res.GetKind() == "ConfigMap" && res.GetName() == "rendered-values" {
						got, err := res.GetFieldValue("data.value")
						if err != nil || got != tc.want {
							t.Errorf("RenderSnapshot(%s) value = %v, %v, want %q", tc.name, got, err, tc.want)
						}
						return
					}
				}
				t.Fatal("RenderSnapshot omitted rendered-values")
			})
		}
	}
}

func TestSnapshotRepositoryChartValuesFiles(t *testing.T) {
	chartRoot, archiveRoot := t.TempDir(), t.TempDir()
	writePreviewFile(t, chartRoot, "Chart.yaml", "apiVersion: v2\nname: app\nversion: 0.1.0\n")
	writePreviewFile(t, chartRoot, "values.yaml", "value: default\ndefaultOnly: default\n")
	writePreviewFile(t, chartRoot, "first.yaml", "value: first\n")
	writePreviewFile(t, chartRoot, "last.yaml", "value: last\n")
	writePreviewFile(t, chartRoot, "templates/cm.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ .Release.Name }}
data:
  value: {{ .Values.value | default "absent" | quote }}
  defaultOnly: {{ .Values.defaultOnly | default "absent" | quote }}
`)
	ch, err := loader.Load(chartRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := chartutil.Save(ch, archiveRoot); err != nil {
		t.Fatal(err)
	}
	writePreviewFile(t, archiveRoot, "index.yaml", "apiVersion: v1\nentries:\n  app:\n  - apiVersion: v2\n    name: app\n    version: 0.1.0\n    urls: [app-0.1.0.tgz]\n")
	server := httptest.NewServer(http.FileServer(http.Dir(archiveRoot)))
	t.Cleanup(server.Close)
	root := t.TempDir()
	writePreviewFile(t, root, "root/repository.yaml", fmt.Sprintf("apiVersion: source.toolkit.fluxcd.io/v1\nkind: HelmRepository\nmetadata: {name: charts, namespace: default}\nspec: {url: %q}\n", server.URL))
	for name, files := range map[string]string{"custom": "[first.yaml, last.yaml]", "defaults": "[values.yaml]", "missing": "[missing.yaml]"} {
		writePreviewFile(t, root, "root/"+name+".yaml", fmt.Sprintf("apiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata: {name: %s, namespace: default}\nspec:\n  chart:\n    spec:\n      chart: app\n      version: 0.1.0\n      sourceRef: {kind: HelmRepository, name: charts}\n      valuesFiles: %s\n      ignoreMissingValuesFiles: true\n", name, files))
	}
	p, err := newTestPreview(t, WithPaths([]string{"root"}, false), WithHelm(&config.HelmSettings{RepositoryCache: t.TempDir(), RepositoryConfig: filepath.Join(t.TempDir(), "repositories.yaml")}), WithStrictInputs())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := p.RenderSnapshot(t.Context(), root)
	if err != nil || !snapshot.Complete {
		t.Fatalf("RenderSnapshot(repository values files) = %#v, %v, want complete", snapshot, err)
	}
	want := map[string][2]string{"custom": {"last", "absent"}, "defaults": {"default", "default"}, "missing": {"absent", "absent"}}
	for _, res := range snapshot.Clusters[""].Resources() {
		if res.GetKind() != "ConfigMap" {
			continue
		}
		expected, ok := want[res.GetName()]
		if !ok {
			continue
		}
		for i, field := range []string{"data.value", "data.defaultOnly"} {
			got, err := res.GetFieldValue(field)
			if err != nil || got != expected[i] {
				t.Errorf("RenderSnapshot(repository %s) %s = %v, %v, want %q", res.GetName(), field, got, err, expected[i])
			}
		}
		delete(want, res.GetName())
	}
	if len(want) != 0 {
		t.Errorf("RenderSnapshot(repository values files) omitted releases %v", want)
	}
}
