package fluxrender

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
)

func newService(t *testing.T) *Service {
	t.Helper()
	s, err := New(logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func writeFixture(t *testing.T, root, name, data string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func openSession(t *testing.T, s *Service, root string, paths []string, recursive bool) string {
	t.Helper()
	config, err := json.Marshal(Config{Helm: true, ResolveGit: true})
	if err != nil {
		t.Fatal(err)
	}
	response, err := s.OpenRender(context.Background(), &plugin.OpenRequest{
		Root: root, Paths: paths, Recursive: recursive, LocalOnly: true, Config: config,
	})
	if err != nil {
		t.Fatal(err)
	}
	return response.Session
}

func input(t *testing.T, data string) plugin.Resource {
	t.Helper()
	r, err := plugin.ParseResource([]byte(data), plugin.Provenance{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func expandSession(t *testing.T, s *Service, session string, inputs ...plugin.Resource) *plugin.ExpandResponse {
	t.Helper()
	r, err := s.Expand(context.Background(), &plugin.ExpandRequest{Session: session, Resources: inputs})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func outputs(response *plugin.ExpandResponse) []plugin.Resource {
	result := []plugin.Resource{}
	for _, expansion := range response.Expansions {
		result = append(result, expansion.Resources...)
	}
	return result
}

func requireClean(t *testing.T, r *plugin.ExpandResponse) {
	t.Helper()
	if len(r.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", r.Diagnostics)
	}
}

func chartFixture(t *testing.T, root, value string) {
	t.Helper()
	writeFixture(t, root, "chart/Chart.yaml", "apiVersion: v2\nname: example\nversion: 0.1.0\n")
	writeFixture(t, root, "chart/values.yaml", "message: "+value+"\n")
	writeFixture(t, root, "chart/templates/result.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: rendered
data:
  message: {{ .Values.message | quote }}
`)
}

const gitInput = `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: source
  namespace: flux-system
spec:
  url: ./
`

const helmInput = `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: app
  namespace: flux-system
spec:
  releaseName: custom-name
  targetNamespace: workload
  chart:
    spec:
      chart: ./chart
      sourceRef:
        kind: GitRepository
        name: source
`

func TestSessionsIsolateBaseAndHead(t *testing.T) {
	t.Parallel()
	s := newService(t)
	base, head := t.TempDir(), t.TempDir()
	chartFixture(t, base, "base")
	chartFixture(t, head, "head")
	baseID := openSession(t, s, base, nil, false)
	headID := openSession(t, s, head, nil, false)
	for _, test := range []struct{ name, session, want string }{
		{name: "base", session: baseID, want: "base"},
		{name: "head", session: headID, want: "head"},
		{name: "base-again", session: baseID, want: "base"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := expandSession(t, s, test.session, input(t, gitInput), input(t, helmInput))
			requireClean(t, r)
			out := outputs(r)
			if len(out) != 1 || !strings.Contains(out[0].YAML, "message: "+test.want) {
				t.Fatalf("want %s output, got %+v", test.want, out)
			}
			if out[0].Provenance.Name != "app" || out[0].Provenance.Namespace != "flux-system" {
				t.Fatalf("provenance must identify source HR, not Helm release: %+v", out[0].Provenance)
			}
			if !strings.Contains(out[0].YAML, "helm.toolkit.fluxcd.io/name: app") {
				t.Fatalf("source labels missing: %s", out[0].YAML)
			}
			if len(r.Expansions) != 1 || r.Expansions[0].Trigger != input(t, helmInput).ID {
				t.Fatalf("incorrect trigger: %+v", r.Expansions)
			}
		})
	}
	for range 2 {
		if _, err := s.CloseRender(context.Background(), &plugin.CloseRequest{Session: baseID}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Expand(context.Background(), &plugin.ExpandRequest{Session: baseID}); err == nil {
		t.Fatal("closed session accepted")
	}
	requireClean(t, expandSession(t, s, headID, input(t, gitInput), input(t, helmInput)))
}

func TestRawRootIgnoresToolkitConfiguration(t *testing.T) {
	t.Parallel()
	for _, name := range []string{".gitops-preview.yaml", ".gitops-preview.yml"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newService(t)
			root := t.TempDir()
			writeFixture(t, root, name, "plugins:\n- name: flux\n  command: gitops-preview-flux\nsort: true\n")
			writeFixture(t, root, "manifest.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: desired\n")
			id := openSession(t, s, root, []string{"."}, false)
			response := expandSession(t, s, id)
			requireClean(t, response)
			out := outputs(response)
			if len(out) != 1 || out[0].ID != "v1/ConfigMap//desired" {
				t.Fatalf("toolkit configuration disrupted raw-root inventory: %+v", response)
			}
		})
	}
}

func TestLogicalResourcesDoNotActivateFluxControllers(t *testing.T) {
	t.Parallel()
	s := newService(t)
	root := t.TempDir()
	writeFixture(t, root, "child/manifest.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: named-controller-output\n")
	id := openSession(t, s, root, nil, false)
	var logical []plugin.Resource
	for _, text := range []string{
		gitInput, helmInput,
		"apiVersion: source.toolkit.fluxcd.io/v1\nkind: HelmRepository\nmetadata:\n  name: source\nspec:\n  url: https://example.invalid\n",
		"apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: source\nspec:\n  path: missing\n",
	} {
		r := input(t, text)
		r.Logical = true
		r.ID = fmt.Sprintf("logical:parent/controller-%d", len(logical))
		name := "source"
		if strings.Contains(r.YAML, "kind: HelmRelease\n") {
			name = "app"
		}
		r.YAML = strings.Replace(r.YAML, "  name: "+name+"\n", "", 1)
		logical = append(logical, r)
	}
	response := expandSession(t, s, id, logical...)
	requireClean(t, response)
	if len(response.Expansions) != 0 {
		t.Fatalf("unnamed logical controllers activated discovery: %+v", response)
	}
	named := input(t, "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: named\nspec:\n  path: child\n")
	response = expandSession(t, s, id, append(logical, named)...)
	requireClean(t, response)
	if len(response.Expansions) != 1 || response.Expansions[0].Trigger != named.ID || len(outputs(response)) != 1 || outputs(response)[0].ID != "v1/ConfigMap//named-controller-output" {
		t.Fatalf("logical inputs interfered with named-controller discovery: %+v", response)
	}
}

func TestChangedValuesAndRetraction(t *testing.T) {
	t.Parallel()
	s := newService(t)
	root := t.TempDir()
	chartFixture(t, root, "default")
	id := openSession(t, s, root, nil, false)
	hr := input(t, helmInput+`  valuesFrom:
    - kind: ConfigMap
      name: values
`)
	values := func(value string) plugin.Resource {
		return input(t, fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata:
  name: values
  namespace: flux-system
data:
  values.yaml: |
    message: %s
`, value))
	}
	git := input(t, gitInput)
	first := expandSession(t, s, id, git, hr, values("first"))
	requireClean(t, first)
	old := outputs(first)
	secondInputs := append([]plugin.Resource{git, hr, values("second")}, old...)
	second := expandSession(t, s, id, secondInputs...)
	requireClean(t, second)
	changed := outputs(second)
	if len(changed) != 1 || !strings.Contains(changed[0].YAML, "message: second") || changed[0].ID != old[0].ID {
		t.Fatalf("same identity did not reevaluate values: %+v", changed)
	}
	missing := expandSession(t, s, id, git, hr)
	if len(outputs(missing)) != 0 || len(missing.Diagnostics) != 1 || missing.Diagnostics[0].Code != "pending-input" {
		t.Fatalf("missing values must retract output and report current pending input: %+v", missing)
	}
	for range 2 {
		retracted := expandSession(t, s, id, append([]plugin.Resource{git, values("second")}, old...)...)
		requireClean(t, retracted)
		if len(retracted.Expansions) != 0 {
			t.Fatalf("deleted HR retained producers: %+v", retracted)
		}
	}
	ready := expandSession(t, s, id, git, hr, values("third"))
	requireClean(t, ready)
	if len(outputs(ready)) != 1 {
		t.Fatalf("ready inputs did not recover: %+v", ready)
	}
}

func TestBootstrapAndTransformedBuildContexts(t *testing.T) {
	t.Parallel()
	for _, recursive := range []bool{false, true} {
		t.Run(fmt.Sprintf("recursive-%t", recursive), func(t *testing.T) {
			s := newService(t)
			root := t.TempDir()
			writeFixture(t, root, "bootstrap/kustomization.yaml", "resources:\n- objects.yaml\n")
			writeFixture(t, root, "bootstrap/objects.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: seed
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: bootstrap
  namespace: flux-system
spec:
  path: ./bootstrap
`)
			id := openSession(t, s, root, []string{"bootstrap"}, recursive)
			response := expandSession(t, s, id)
			requireClean(t, response)
			if len(outputs(response)) != 2 || len(response.Expansions) != 2 {
				t.Fatalf("bootstrap was duplicated: %+v", response)
			}
			writeFixture(t, root, "child/object.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: result\ndata:\n  value: ${value}\n")
			ks := func(name string) plugin.Resource {
				return input(t, fmt.Sprintf(`apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: %s
  namespace: flux-system
spec:
  path: child
  targetNamespace: %s
  postBuild:
    substitute:
      value: %s
`, name, name, name))
			}
			response = expandSession(t, s, id, ks("one"), ks("two"))
			requireClean(t, response)
			out := outputs(response)
			if len(out) != 4 {
				t.Fatalf("transformed contexts collapsed: %+v", out)
			}
			for _, namespace := range []string{"one", "two"} {
				found := false
				for _, resource := range out {
					if resource.ID == "v1/ConfigMap/"+namespace+"/result" {
						found = strings.Contains(resource.YAML, "value: "+namespace) && resource.Provenance.Name == namespace
					}
				}
				if !found {
					t.Fatalf("missing transformed %s build: %+v", namespace, out)
				}
			}
		})
	}
}

func TestRecursiveBootstrapAliasesIndependentDirectories(t *testing.T) {
	t.Parallel()
	s := newService(t)
	root := t.TempDir()
	writeFixture(t, root, "tree/controllers.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: child
spec:
  path: tree/child
`)
	writeFixture(t, root, "tree/child/object.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: recursive-leaf\n")
	id := openSession(t, s, root, []string{"tree", "./tree", "tree/../tree"}, true)
	r := expandSession(t, s, id)
	requireClean(t, r)
	if len(r.Expansions) != 2 || len(outputs(r)) != 2 {
		t.Fatalf("independent recursive bootstrap build was duplicated: %+v", r)
	}
	for _, expansion := range r.Expansions {
		if expansion.Trigger != "root" && len(expansion.Resources) != 0 {
			t.Fatalf("bootstrap-equivalent KS owns duplicate recursive output: %+v", expansion)
		}
	}
}

func TestInternalHelmOutputSupportsHelmRelease(t *testing.T) {
	t.Parallel()
	s := newService(t)
	root := t.TempDir()
	chartFixture(t, root, "default")
	writeFixture(t, root, "producer/Chart.yaml", "apiVersion: v2\nname: producer\nversion: 0.1.0\n")
	writeFixture(t, root, "producer/templates/values.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: values
  namespace: flux-system
data:
  values.yaml: |
    message: internally-discovered
`)
	id := openSession(t, s, root, nil, false)
	producer := input(t, strings.ReplaceAll(strings.ReplaceAll(helmInput, "name: app", "name: z-producer"), "chart: ./chart", "chart: ./producer"))
	consumer := input(t, helmInput+`  valuesFrom:
    - kind: ConfigMap
      name: values
`)
	r := expandSession(t, s, id, input(t, gitInput), consumer, producer)
	requireClean(t, r)
	if len(outputs(r)) != 2 {
		t.Fatalf("internal values did not settle: %+v", r)
	}
	found := false
	for _, output := range outputs(r) {
		if strings.Contains(output.YAML, "message: internally-discovered") && output.Provenance.Name == "app" {
			found = true
		}
	}
	if !found {
		t.Fatalf("consumer failed to use internal values: %+v", r)
	}
	removed := expandSession(t, s, id, append([]plugin.Resource{input(t, gitInput), consumer}, outputs(r)...)...)
	if len(outputs(removed)) != 0 || len(removed.Diagnostics) != 1 || removed.Diagnostics[0].Code != "pending-input" {
		t.Fatalf("producer removal kept stale internal values: %+v", removed)
	}
	foreignValues := input(t, `apiVersion: v1
kind: ConfigMap
metadata:
  name: values
  namespace: flux-system
data:
  values.yaml: |
    message: now-foreign
`)
	foreignValues.Provenance = plugin.Provenance{Kind: "XR", Name: "external-values"}
	foreign := expandSession(t, s, id, input(t, gitInput), consumer, foreignValues)
	requireClean(t, foreign)
	if len(outputs(foreign)) != 1 || !strings.Contains(outputs(foreign)[0].YAML, "message: now-foreign") {
		t.Fatalf("foreign takeover of a retracted identity was ignored or returned as owned: %+v", foreign)
	}
}

func TestDuplicateOwnershipAndLocalOnly(t *testing.T) {
	t.Parallel()
	s := newService(t)
	root := t.TempDir()
	writeFixture(t, root, "child/object.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: result\n")
	id := openSession(t, s, root, nil, false)
	ks := func(name, path string) plugin.Resource {
		return input(t, fmt.Sprintf("apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: %s\nspec:\n  path: %s\n", name, path))
	}
	r := expandSession(t, s, id, ks("one", "child"), ks("two", "child"))
	if len(outputs(r)) != 2 || len(r.Diagnostics) != 1 || r.Diagnostics[0].Code != "duplicate-resource" {
		t.Fatalf("duplicate owners must be surfaced: %+v", r)
	}
	r = expandSession(t, s, id, ks("escape", "../outside"))
	if len(outputs(r)) != 0 || len(r.Diagnostics) != 1 || r.Diagnostics[0].Code != "render-error" {
		t.Fatalf("local-only escape accepted: %+v", r)
	}
}

func TestInternalPathsDiscoverControllersAndRetract(t *testing.T) {
	t.Parallel()
	s := newService(t)
	root := t.TempDir()
	writeFixture(t, root, "first/control.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: child
  namespace: flux-system
spec:
  path: second
`)
	writeFixture(t, root, "second/object.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: leaf\n")
	id := openSession(t, s, root, nil, false)
	parent := input(t, `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: parent
  namespace: flux-system
spec:
  path: first
`)
	r := expandSession(t, s, id, parent)
	requireClean(t, r)
	if len(r.Expansions) != 2 || len(outputs(r)) != 2 {
		t.Fatalf("internal KS did not expand: %+v", r)
	}
	child := input(t, `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: child
  namespace: flux-system
spec:
  path: second
`)
	found := false
	for _, expansion := range r.Expansions {
		if expansion.Trigger == child.ID && len(expansion.Resources) == 1 && expansion.Resources[0].Provenance.Name == "child" {
			found = true
		}
	}
	if !found {
		t.Fatalf("incorrect nested output attribution: %+v", r)
	}
	writeFixture(t, root, "first/control.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: replacement\n")
	replaced := expandSession(t, s, id, append([]plugin.Resource{parent}, outputs(r)...)...)
	requireClean(t, replaced)
	if len(replaced.Expansions) != 1 || len(outputs(replaced)) != 1 || !strings.Contains(outputs(replaced)[0].ID, "replacement") {
		t.Fatalf("removed internal controller retained subtree: %+v", replaced)
	}
}

func TestContentAwareClosureAndOscillation(t *testing.T) {
	t.Parallel()
	t.Run("unchanged-inputs-random-template", func(t *testing.T) {
		s := newService(t)
		root := t.TempDir()
		chartFixture(t, root, "unused")
		writeFixture(t, root, "chart/templates/result.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: generated
data:
  value: {{ randAlphaNum 64 | quote }}
`)
		id := openSession(t, s, root, nil, false)
		r := expandSession(t, s, id, input(t, gitInput), input(t, helmInput))
		requireClean(t, r)
		if len(outputs(r)) != 1 {
			t.Fatalf("random-valued template did not settle: %+v", r)
		}
	})
	t.Run("same-count-changed-content", func(t *testing.T) {
		s := newService(t)
		root := t.TempDir()
		chartFixture(t, root, "default")
		writeFixture(t, root, "producer/Chart.yaml", "apiVersion: v2\nname: producer\nversion: 0.1.0\n")
		writeFixture(t, root, "producer/templates/values.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: values
  namespace: flux-system
data:
  values.yaml: |
    message: discovered
`)
		id := openSession(t, s, root, nil, false)
		producer := input(t, strings.ReplaceAll(strings.ReplaceAll(helmInput, "name: app", "name: z-producer"), "chart: ./chart", "chart: ./producer"))
		consumer := input(t, helmInput+`  valuesFrom:
    - kind: ConfigMap
      name: values
      optional: true
`)
		r := expandSession(t, s, id, input(t, gitInput), consumer, producer)
		requireClean(t, r)
		found := false
		for _, output := range outputs(r) {
			if output.Provenance.Name == "app" && strings.Contains(output.YAML, "message: discovered") {
				found = true
			}
		}
		if !found {
			t.Fatalf("unchanged output count hid changed values: %+v", r)
		}
	})
	t.Run("oscillating-values", func(t *testing.T) {
		s := newService(t)
		root := t.TempDir()
		chartFixture(t, root, "first")
		writeFixture(t, root, "chart/templates/result.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: values
  namespace: flux-system
data:
  values.yaml: |
    message: {{ if eq .Values.message "first" }}second{{ else }}first{{ end }}
`)
		id := openSession(t, s, root, nil, false)
		hr := input(t, helmInput+`  valuesFrom:
    - kind: ConfigMap
      name: values
      optional: true
`)
		r := expandSession(t, s, id, input(t, gitInput), hr)
		if len(r.Diagnostics) != 1 || r.Diagnostics[0].Code != "oscillation" {
			t.Fatalf("content oscillation was not detected: %+v", r)
		}
	})
	t.Run("unbounded-changing-content", func(t *testing.T) {
		s := newService(t)
		root := t.TempDir()
		chartFixture(t, root, "unused")
		writeFixture(t, root, "chart/templates/result.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: values
  namespace: flux-system
data:
  values.yaml: |
    count: {{ add (default 0 .Values.count) 1 }}
`)
		id := openSession(t, s, root, nil, false)
		hr := input(t, helmInput+`  valuesFrom:
    - kind: ConfigMap
      name: values
      optional: true
`)
		r := expandSession(t, s, id, input(t, gitInput), hr)
		if len(r.Diagnostics) != 1 || r.Diagnostics[0].Code != "iteration-limit" {
			t.Fatalf("non-repeating local closure was not bounded: %+v", r)
		}
	})
}

func TestOpenOptionsAndLifecycle(t *testing.T) {
	t.Parallel()
	s := newService(t)
	description, err := s.Describe(context.Background(), &plugin.DescribeRequest{ProtocolVersion: plugin.ProtocolVersion})
	if err != nil || description.Name != "flux" {
		t.Fatalf("Describe: %+v, %v", description, err)
	}
	if _, err := s.Describe(context.Background(), &plugin.DescribeRequest{ProtocolVersion: -1}); err == nil {
		t.Fatal("unsupported protocol accepted")
	}
	root := t.TempDir()
	ks := input(t, "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata:\n  name: app\nspec:\n  path: missing\n")
	opened, err := s.OpenRender(context.Background(), &plugin.OpenRequest{Root: root, Config: json.RawMessage(`{"fluxKS":false}`)})
	if err != nil {
		t.Fatal(err)
	}
	r := expandSession(t, s, opened.Session, ks)
	requireClean(t, r)
	if len(r.Expansions) != 0 {
		t.Fatalf("explicitly disabled KS expanded: %+v", r)
	}
	strict, err := s.OpenRender(context.Background(), &plugin.OpenRequest{Root: root, StrictInputs: true})
	if err != nil {
		t.Fatal(err)
	}
	unsupported := input(t, ks.YAML+"  namePrefix: unsupported\n")
	r = expandSession(t, s, strict.Session, unsupported)
	if len(r.Diagnostics) != 1 || !strings.Contains(r.Diagnostics[0].Message, "strict inputs") {
		t.Fatalf("strict inputs ignored: %+v", r)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenRender(context.Background(), &plugin.OpenRequest{Root: root}); err == nil {
		t.Fatal("closed service accepted a session")
	}
}

func TestRemoteChartAcquisitionSharedAcrossSessions(t *testing.T) {
	t.Parallel()
	s := newService(t)
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for _, file := range []struct{ name, data string }{
		{name: "example/Chart.yaml", data: "apiVersion: v2\nname: example\nversion: 0.1.0\n"},
		{name: "example/templates/config.yaml", data: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: remote\ndata:\n  value: {{ .Values.message | quote }}\n"},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: file.name, Mode: 0o644, Size: int64(len(file.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(file.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/index.yaml":
			if _, err := fmt.Fprint(w, "apiVersion: v1\nentries:\n  example:\n  - apiVersion: v2\n    name: example\n    version: 0.1.0\n    urls:\n    - example-0.1.0.tgz\n"); err != nil {
				t.Errorf("write chart index: %v", err)
			}
		case "/example-0.1.0.tgz":
			downloads.Add(1)
			_, _ = w.Write(archive.Bytes())
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()
	cache := t.TempDir()
	writeFixture(t, cache, "repositories.yaml", "apiVersion: v1\nrepositories: []\n")
	config, err := json.Marshal(Config{Helm: true, HelmSettings: HelmSettings{
		RepositoryCache: cache, RepositoryConfig: filepath.Join(cache, "repositories.yaml"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range []string{"base", "head"} {
		opened, err := s.OpenRender(context.Background(), &plugin.OpenRequest{Root: t.TempDir(), RunID: "shared-chart-run", Config: config})
		if err != nil {
			t.Fatal(err)
		}
		repo := input(t, fmt.Sprintf("apiVersion: source.toolkit.fluxcd.io/v1\nkind: HelmRepository\nmetadata:\n  name: source\n  namespace: flux-system\nspec:\n  url: %s\n", server.URL))
		hr := strings.ReplaceAll(strings.ReplaceAll(helmInput, "kind: GitRepository", "kind: HelmRepository"), "chart: ./chart", "chart: example\n      version: 0.1.0")
		hr += "  values:\n    message: " + message + "\n"
		r := expandSession(t, s, opened.Session, repo, input(t, hr))
		requireClean(t, r)
		if len(outputs(r)) != 1 || !strings.Contains(outputs(r)[0].YAML, "value: "+message) {
			t.Fatalf("cached acquisition reused rendered values: %+v", r)
		}
	}
	if downloads.Load() != 1 {
		t.Fatalf("archive downloaded %d times, want once", downloads.Load())
	}
}
