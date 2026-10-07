package fluxrender_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/fluxrender"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	"github.com/tobiash/gitops-preview-toolkit/pkg/pluginhost"
	"github.com/tobiash/gitops-preview-toolkit/pkg/preview"
	"gopkg.in/yaml.v3"
)

func randomHelmTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"chart/Chart.yaml": "apiVersion: v2\nname: random\nversion: 0.1.0\n",
		"chart/templates/result.yaml": `apiVersion: v1
kind: ConfigMap
metadata:
  name: generated
data:
  value: {{ randAlphaNum 64 | quote }}
`,
		"root/inputs.yaml": `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: source
  namespace: flux-system
spec:
  url: ./
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: app
  namespace: flux-system
spec:
  chart:
    spec:
      chart: ./chart
      sourceRef:
        kind: GitRepository
        name: source
`,
	}
	for name, data := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestHostRandomHelmConvergesWithIndependentEvaluations(t *testing.T) {
	t.Parallel()
	svc, err := fluxrender.New(logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := json.Marshal(fluxrender.Config{Helm: true, ResolveGit: true})
	if err != nil {
		t.Fatal(err)
	}
	host, err := pluginhost.NewWithStarter([]plugin.Command{{Name: "flux", Config: cfg}},
		func(context.Context, plugin.Command) (plugin.Service, func() error, error) {
			return svc, svc.Close, nil
		})
	if err != nil {
		_ = svc.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := host.Close(); err != nil {
			t.Error(err)
		}
	})
	request := plugin.OpenRequest{Root: randomHelmTree(t), Paths: []string{"root"}, LocalOnly: true, Fresh: true}
	generated := []string{}
	for range 2 {
		result, err := host.Render(t.Context(), request)
		if err != nil || !result.Complete {
			t.Fatalf("real Flux host evaluation did not converge: result=%+v, error=%v", result, err)
		}
		found := false
		for _, resource := range result.Resources {
			if resource.ID == "v1/ConfigMap/flux-system/generated" {
				generated = append(generated, resource.YAML)
				found = true
			}
		}
		if !found {
			t.Fatalf("missing rendered random resource: %+v", result)
		}
	}
	if generated[0] == generated[1] {
		t.Fatal("independent render sessions reused random desired output")
	}
}

func TestPreviewDetectPermadiffsUsesRealFluxRandomHelm(t *testing.T) {
	t.Parallel()
	binary := filepath.Join(t.TempDir(), "gitops-preview-flux")
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-o", binary, "../../cmd/gitops-preview-flux")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Flux executable: %v\n%s", err, output)
	}
	p, err := preview.New(
		preview.WithPlugins([]plugin.Command{{Name: "flux", Command: binary}}),
		preview.WithPaths([]string{"root"}, false),
		preview.WithGitRepo(),
		preview.WithLocalOnly(),
		preview.WithHelm(&config.HelmSettings{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	var output bytes.Buffer
	if err := p.DetectPermadiffs(ctx, randomHelmTree(t), &output); err != nil {
		t.Fatalf("actual Preview permadiff detection failed: %v\n%s", err, output.String())
	}
	var normalization struct {
		Filters []struct {
			Kind       string `yaml:"kind"`
			FieldPaths []struct {
				Path []string `yaml:"path"`
			} `yaml:"fieldPaths"`
		} `yaml:"filters"`
	}
	if err := yaml.Unmarshal(output.Bytes(), &normalization); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, filter := range normalization.Filters {
		for _, field := range filter.FieldPaths {
			if filter.Kind == "FieldNormalizer" && slices.Equal(field.Path, []string{"data", "value"}) {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("random Helm value was hidden from permadiff detection: %s", output.String())
	}
}
