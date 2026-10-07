package helm

import (
	"bytes"
	"context"
	"testing"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/resource"
)

func TestBuildPostRenderersFromSpec(t *testing.T) {
	spec := map[string]any{
		"commonMetadata": map[string]any{
			"labels": map[string]any{"env": "prod"},
		},
		"postRenderers": []any{
			map[string]any{
				"kustomize": map[string]any{
					"patches": []any{
						map[string]any{
							"patch": `apiVersion: v1
kind: ConfigMap
metadata:
  name: test
  labels:
    patched: "true"
`,
						},
					},
				},
			},
		},
	}

	renderer, err := buildPostRenderersFromSpec("release", "default", spec)
	if err != nil {
		t.Fatalf("buildPostRenderersFromSpec() error = %v", err)
	}

	out, err := renderer.Run(bytes.NewBufferString(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test
`))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	result := out.String()
	if !bytes.Contains([]byte(result), []byte("patched: \"true\"")) {
		t.Fatalf("expected patched label in output, got:\n%s", result)
	}
	if !bytes.Contains([]byte(result), []byte("env: prod")) {
		t.Fatalf("expected common metadata label in output, got:\n%s", result)
	}
	if !bytes.Contains([]byte(result), []byte("helm.toolkit.fluxcd.io/name: release")) {
		t.Fatalf("expected origin label in output, got:\n%s", result)
	}
}

func TestRenderAllCharts_PassesPostRendererToRunner(t *testing.T) {
	runner := &stubChartRunner{}
	s := &expandState{
		runner: runner,
		render: render.NewDefaultRender(logr.Discard()),
		logger: logr.Discard(),
		releases: []*helmv2.HelmRelease{{
			ObjectMeta: metav1.ObjectMeta{Name: "podinfo", Namespace: "default"},
			Spec: helmv2.HelmReleaseSpec{
				Chart: &helmv2.HelmChartTemplate{Spec: helmv2.HelmChartTemplateSpec{
					Chart: "podinfo",
					SourceRef: helmv2.CrossNamespaceObjectReference{
						Kind: "HelmRepository",
						Name: "podinfo",
					},
				}},
				CommonMetadata: &helmv2.CommonMetadata{Labels: map[string]string{"env": "prod"}},
			},
		}},
		repositories: []*sourcev1.HelmRepository{{
			ObjectMeta: metav1.ObjectMeta{Name: "podinfo", Namespace: "default"},
			Spec:       sourcev1.HelmRepositorySpec{URL: "https://example.com/charts"},
		}},
	}

	_, errs, err := s.renderAllCharts(context.Background())
	if err != nil {
		t.Fatalf("renderAllCharts() error = %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("renderAllCharts() returned unexpected warnings: %v", errs)
	}
	if len(runner.tasks) != 1 {
		t.Fatalf("expected one render task, got %d", len(runner.tasks))
	}
	if runner.tasks[0].postRenderer == nil {
		t.Fatal("expected render task to include a post renderer")
	}
	if got := runner.tasks[0].origin; got != render.HelmReleaseProvenance("default", "podinfo") {
		t.Errorf("task origin = %+v, want HelmRelease default/podinfo", got)
	}

	out, err := runPostRenderer(runner.tasks[0].postRenderer, bytes.NewBufferString(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test
`))
	if err != nil {
		t.Fatalf("runPostRenderer() error = %v", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("env: prod")) {
		t.Fatalf("expected task post renderer to add common metadata, got:\n%s", out.String())
	}
}

func TestRenderAllCharts_ResolvesGitRepositoryChartSource(t *testing.T) {
	runner := &stubChartRunner{}
	s := &expandState{
		runner:   runner,
		resolver: stubChartSourceResolver{"flux-system/podinfo": "/tmp/source"},
		render:   render.NewDefaultRender(logr.Discard()),
		logger:   logr.Discard(),
		releases: []*helmv2.HelmRelease{{
			ObjectMeta: metav1.ObjectMeta{Name: "podinfo", Namespace: "default"},
			Spec: helmv2.HelmReleaseSpec{
				Chart: &helmv2.HelmChartTemplate{Spec: helmv2.HelmChartTemplateSpec{
					Chart: "./charts/podinfo",
					SourceRef: helmv2.CrossNamespaceObjectReference{
						Kind:      "GitRepository",
						Name:      "podinfo",
						Namespace: "flux-system",
					},
				}},
			},
		}},
	}

	_, errs, err := s.renderAllCharts(context.Background())
	if err != nil {
		t.Fatalf("renderAllCharts() error = %v", err)
	}
	if len(errs) != 0 {
		t.Fatalf("renderAllCharts() returned unexpected warnings: %v", errs)
	}
	if len(runner.tasks) != 1 {
		t.Fatalf("expected one render task, got %d", len(runner.tasks))
	}
	if got, want := runner.tasks[0].localChartPath, "/tmp/source/charts/podinfo"; got != want {
		t.Fatalf("localChartPath = %q, want %q", got, want)
	}
}

func TestComposeValues_MergesValuesFromAndInlineValues(t *testing.T) {
	r := renderFromYAML(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: base-values
  namespace: default
data:
  values.yaml: |
    image:
      repository: podinfo
      tag: 1.0.0
    service:
      port: 80
`)
	s := newTestExpandState(r)
	valuesClient, err := s.newValuesClient()
	if err != nil {
		t.Fatalf("newValuesClient() error = %v", err)
	}
	hr := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{Name: "podinfo", Namespace: "default"},
		Spec: helmv2.HelmReleaseSpec{
			ValuesFrom: []helmv2.ValuesReference{{Kind: "ConfigMap", Name: "base-values"}},
			Values:     &apiextensionsv1.JSON{Raw: []byte(`{"image":{"tag":"2.0.0"},"replicas":2}`)},
		},
	}

	values, err := s.composeValues(context.Background(), valuesClient, hr)
	if err != nil {
		t.Fatalf("composeValues() error = %v", err)
	}
	image, ok := values["image"].(map[string]any)
	if !ok {
		t.Fatalf("image = %#v, want map", values["image"])
	}
	if got, want := image["repository"], "podinfo"; got != want {
		t.Fatalf("image.repository = %v, want %v", got, want)
	}
	if got, want := image["tag"], "2.0.0"; got != want {
		t.Fatalf("image.tag = %v, want %v", got, want)
	}
	service, ok := values["service"].(map[string]any)
	if !ok {
		t.Fatalf("service = %#v, want map", values["service"])
	}
	assertNumericValue(t, service["port"], 80, "service.port")
	assertNumericValue(t, values["replicas"], 2, "replicas")
}

func TestComposeValues_IgnoresOptionalMissingReference(t *testing.T) {
	s := newTestExpandState(render.NewDefaultRender(logr.Discard()))
	valuesClient, err := s.newValuesClient()
	if err != nil {
		t.Fatalf("newValuesClient() error = %v", err)
	}
	hr := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{Name: "podinfo", Namespace: "default"},
		Spec: helmv2.HelmReleaseSpec{
			ValuesFrom: []helmv2.ValuesReference{{Kind: "ConfigMap", Name: "missing", Optional: true}},
			Values:     &apiextensionsv1.JSON{Raw: []byte(`{"replicas":2}`)},
		},
	}

	values, err := s.composeValues(context.Background(), valuesClient, hr)
	if err != nil {
		t.Fatalf("composeValues() error = %v", err)
	}
	assertNumericValue(t, values["replicas"], 2, "replicas")
}

func TestComposeValues_UsesFluxTargetPathMerging(t *testing.T) {
	r := renderFromYAML(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: target-values
  namespace: default
data:
  url: 'https://example.com'
`)
	s := newTestExpandState(r)
	valuesClient, err := s.newValuesClient()
	if err != nil {
		t.Fatalf("newValuesClient() error = %v", err)
	}
	hr := &helmv2.HelmRelease{
		ObjectMeta: metav1.ObjectMeta{Name: "podinfo", Namespace: "default"},
		Spec: helmv2.HelmReleaseSpec{
			ValuesFrom: []helmv2.ValuesReference{{
				Kind:       "ConfigMap",
				Name:       "target-values",
				ValuesKey:  "url",
				TargetPath: "env.URL",
			}},
		},
	}

	values, err := s.composeValues(context.Background(), valuesClient, hr)
	if err != nil {
		t.Fatalf("composeValues() error = %v", err)
	}
	env, ok := values["env"].(map[string]any)
	if !ok {
		t.Fatalf("env = %#v, want map", values["env"])
	}
	if got, want := env["URL"], "https://example.com"; got != want {
		t.Fatalf("env.URL = %v, want %v", got, want)
	}
}

func newTestExpandState(r *render.Render) *expandState {
	sch := runtime.NewScheme()
	_ = scheme.AddToScheme(sch)
	return &expandState{render: r, logger: logr.Discard(), scheme: sch}
}

func renderFromYAML(t *testing.T, manifests string) *render.Render {
	t.Helper()
	factory := resmap.NewFactory(resource.NewFactory(nil))
	rm, err := factory.NewResMapFromBytes([]byte(manifests))
	if err != nil {
		t.Fatalf("NewResMapFromBytes() error = %v", err)
	}
	r := render.NewDefaultRender(logr.Discard())
	if err := r.AbsorbAll(rm); err != nil {
		t.Fatalf("AbsorbAll() error = %v", err)
	}
	return r
}

func assertNumericValue(t *testing.T, got any, want int, field string) {
	t.Helper()
	switch v := got.(type) {
	case int:
		if v != want {
			t.Fatalf("%s = %v, want %d", field, got, want)
		}
	case int64:
		if v != int64(want) {
			t.Fatalf("%s = %v, want %d", field, got, want)
		}
	case float64:
		if v != float64(want) {
			t.Fatalf("%s = %v, want %d", field, got, want)
		}
	default:
		t.Fatalf("%s = %v (%T), want numeric value %d", field, got, got, want)
	}
}

type stubChartRunner struct {
	tasks []RenderTask
}

type stubChartSourceResolver map[string]string

func (s stubChartSourceResolver) ResolvePath(namespace, name string) (string, bool) {
	path, ok := s[namespace+"/"+name]
	return path, ok
}

func (s *stubChartRunner) RenderCharts(_ context.Context, tasks []RenderTask) (resmap.ResMap, []error, error) {
	s.tasks = append([]RenderTask(nil), tasks...)
	return resmap.New(), nil, nil
}
