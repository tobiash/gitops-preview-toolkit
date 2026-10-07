package cli

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/preview"
)

func TestBuildOpts_ExplicitConfigOverridesDiscoveredConfig(t *testing.T) {
	resetGlobals()

	repoDir := t.TempDir()
	writeFile(t, repoDir, ".fmp.yaml", `paths:
  - from-discovered
sort: false
helm: true
`)
	explicitConfigPath := filepath.Join(repoDir, "explicit.yaml")
	writeFile(t, repoDir, "explicit.yaml", `paths:
  - from-explicit
sort: true
helm: false
exclude-crds: true
`)

	configFile = explicitConfigPath

	opts, err := buildOpts(nil, logr.Discard(), repoDir)
	if err != nil {
		t.Fatalf("buildOpts() error = %v", err)
	}
	p, err := preview.New(opts...)
	if err != nil {
		t.Fatalf("preview.New() error = %v", err)
	}
	t.Cleanup(func() { _ = p.Close() })

	pv := reflect.ValueOf(p).Elem()
	pathsField := pv.FieldByName("paths")
	paths := make([]string, pathsField.Len())
	for i := range paths {
		paths[i] = pathsField.Index(i).String()
	}
	if !reflect.DeepEqual(paths, []string{"from-explicit"}) {
		t.Fatalf("paths = %v, want [from-explicit]", paths)
	}
	if !pv.FieldByName("sortOutput").Bool() {
		t.Fatal("expected sortOutput=true from explicit config")
	}
	if !pv.FieldByName("excludeCRDs").Bool() {
		t.Fatal("expected excludeCRDs=true from explicit config")
	}
	if !pv.FieldByName("helmSettings").IsNil() {
		t.Fatal("expected helm to be disabled by explicit config")
	}
}

func TestCIReportsMissingEnv(t *testing.T) {
	cmd := exec.Command(filepath.Join(testBinaryDir, "fmp"), "ci")
	cmd.Env = envWithout("FMP_REPO_A", "FMP_REPO_B", "FMP_CONFIG", "FMP_FILTER", "FMP_KUSTOMIZATIONS", "FMP_RENDER_HELM")

	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected ci command to fail without required env vars")
	}
	if !bytes.Contains(out, []byte("must set both FMP_REPO_A and FMP_REPO_B")) {
		t.Fatalf("expected missing env error, got %q", string(out))
	}
}

func TestDiffOutputs(t *testing.T) {
	t.Run("raw diff only default", func(t *testing.T) {
		resetGlobals()
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		summaryOut, diffOut := diffOutputs(stdout, stderr)
		if summaryOut != io.Discard {
			t.Fatal("expected summary output to be discarded by default")
		}
		if diffOut != stdout {
			t.Fatal("expected diff output to go to stdout by default")
		}
	})

	t.Run("summary to stderr", func(t *testing.T) {
		resetGlobals()
		diffSummary = true
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		summaryOut, diffOut := diffOutputs(stdout, stderr)
		if summaryOut != stderr {
			t.Fatal("expected summary output to go to stderr")
		}
		if diffOut != stdout {
			t.Fatal("expected diff output to remain on stdout")
		}
	})

	t.Run("summary only", func(t *testing.T) {
		resetGlobals()
		diffSummaryOnly = true
		stdout := &bytes.Buffer{}
		stderr := &bytes.Buffer{}
		summaryOut, diffOut := diffOutputs(stdout, stderr)
		if summaryOut != stdout {
			t.Fatal("expected summary-only output on stdout")
		}
		if diffOut != io.Discard {
			t.Fatal("expected diff output to be discarded in summary-only mode")
		}
	})
}

func resetGlobals() {
	crossplaneEnabled = false
	crossplaneEngine = ""
	crossplaneFunctions = nil
	fluxPlugin = ""
	crossplanePlugin = ""
	kustomizations = nil
	recursive = false
	renderHelm = true
	filtersFile = ""
	filterYAML = ""
	sortOutput = false
	excludeCRDs = false
	quiet = false
	resolveGit = false
	sopsDecrypt = false
	configFile = ""
	outputFormat = ""
	helmRelease = ""
	diffSummary = false
	diffSummaryOnly = false
	initConfig = false
	helmRegistryConfig = ""
	helmRepositoryConfig = ""
	helmRepositoryCache = ""
}

func envWithout(keys ...string) []string {
	skip := make(map[string]bool, len(keys))
	for _, key := range keys {
		skip[key] = true
	}

	var env []string
	for _, entry := range os.Environ() {
		key, _, found := strings.Cut(entry, "=")
		if found && skip[key] {
			continue
		}
		env = append(env, entry)
	}
	return env
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	fullPath := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("creating directory for %s: %v", name, err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}
