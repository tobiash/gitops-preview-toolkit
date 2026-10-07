package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/githubaction"
	"github.com/tobiash/gitops-preview-toolkit/pkg/preview"
)

func TestDescribeIncludesActualFlagMetadata(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIProcess$", "--", "describe")
	cmd.Env = append(os.Environ(), "FMP_TEST_PROCESS=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	var doc struct {
		Commands []struct {
			Name  string `json:"name"`
			Usage string `json:"usage"`
			Flags []struct {
				Name      string `json:"name"`
				Type      string `json:"type"`
				Default   string `json:"default"`
				Shorthand string `json:"shorthand"`
			} `json:"flags"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	for _, command := range doc.Commands {
		if command.Name != "diff" {
			continue
		}
		if !strings.Contains(command.Usage, "[<rev>|<source-a> <source-b>]") {
			t.Errorf("diff usage = %q, want actual argument syntax", command.Usage)
		}
		found := map[string]bool{}
		for _, flag := range command.Flags {
			if found[flag.Name] {
				t.Errorf("duplicate flag %q", flag.Name)
			}
			found[flag.Name] = true
			switch flag.Name {
			case "exit-code":
				if flag.Type != "bool" || flag.Default != "false" {
					t.Errorf("exit-code metadata = %#v", flag)
				}
			case "path":
				if flag.Type != "stringSlice" || flag.Shorthand != "k" {
					t.Errorf("inherited path metadata = %#v", flag)
				}
			case "output":
				if flag.Type != "string" || flag.Default != "yaml" || flag.Shorthand != "o" {
					t.Errorf("output metadata = %#v", flag)
				}
			}
		}
		for _, name := range []string{"exit-code", "path", "output"} {
			if !found[name] {
				t.Errorf("describe diff missing %q", name)
			}
		}
		return
	}
	t.Fatal("describe missing diff command")
}

func TestHTMLDiffExitBehavior(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exitCode bool
		policy   bool
		wantCode int
	}{
		{name: "default changes"},
		{name: "opt-in changes", exitCode: true, wantCode: 1},
		{name: "policy failure", policy: true, wantCode: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left, right, artifacts := t.TempDir(), t.TempDir(), t.TempDir()
			writeFile(t, left, "manifests/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: old\n")
			writeFile(t, right, "manifests/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: new\n")
			if tc.policy {
				writeFile(t, left, ".fmp.yaml", `policies:
  inline:
    - |
      package fmp
      import rego.v1
      violations contains {"id": "blocked", "message": "blocked"}
  fail-on: [blocked]
`)
			}
			args := []string{"-test.run=^TestCLIProcess$", "--", "diff", left, right, "-k", "manifests", "--html", "--html-open=false"}
			if tc.exitCode {
				args = append(args, "--exit-code")
			}
			cmd := exec.Command(os.Args[0], args...)
			cmd.Env = append(os.Environ(), "FMP_TEST_PROCESS=1", "TMPDIR="+artifacts)
			out, err := cmd.CombinedOutput()
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != tc.wantCode {
				t.Fatalf("HTML exit = %v (%v), want %d; output=%s", cmd.ProcessState, err, tc.wantCode, out)
			}
			files, err := filepath.Glob(filepath.Join(artifacts, "fmp-html-report-*", "index.html"))
			if err != nil || len(files) != 1 {
				t.Fatalf("HTML files = %v, error=%v; output=%s", files, err, out)
			}
			html, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			if len(html) == 0 || (tc.policy && !bytes.Contains(html, []byte("blocked"))) {
				t.Error("HTML report missing expected content")
			}
		})
	}
}

func TestJSONFailurePreservesTestIssueObjects(t *testing.T) {
	dir := t.TempDir()
	manifest := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: duplicate\n"
	writeFile(t, dir, "a.yaml", manifest)
	writeFile(t, dir, "b.yaml", manifest)
	p, err := preview.New(preview.WithLogger(logr.Discard()), preview.WithPaths([]string{"."}, false))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	want, err := p.TestJSON(context.Background(), dir)
	if err == nil || len(want.Errors) == 0 {
		t.Fatalf("TestJSON() = %#v, error=%v, want failure with issue objects", want, err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIProcess$", "--", "test", dir, "-k", ".", "-o", "json")
	cmd.Env = append(os.Environ(), "FMP_TEST_PROCESS=1")
	out, err := cmd.Output()
	if err == nil {
		t.Fatalf("test subprocess succeeded: %s", out)
	}
	var got struct {
		Warnings []preview.TestIssue `json:"warnings"`
		Errors   []preview.TestIssue `json:"errors"`
		Error    json.RawMessage     `json:"error"`
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("decode warning objects: %v; output=%s", err, out)
	}
	if !reflect.DeepEqual(got.Warnings, want.Warnings) {
		t.Errorf("CLI warnings = %#v, want TestJSON warnings %#v", got.Warnings, want.Warnings)
	}
	if !reflect.DeepEqual(got.Errors, want.Errors) {
		t.Errorf("CLI errors = %#v, want TestJSON errors %#v", got.Errors, want.Errors)
	}
	if len(got.Error) == 0 {
		t.Errorf("CLI output lacks failure info: %s", out)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		t.Errorf("trailing decode = %v, want EOF", err)
	}
}

func TestCLIProcess(t *testing.T) {
	if os.Getenv("FMP_TEST_PROCESS") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"fmp"}, os.Args[i+1:]...)
			os.Exit(Run("fmp", "dev"))
		}
	}
}

func TestDiffExitCodeOptIn(t *testing.T) {
	left, right := t.TempDir(), t.TempDir()
	writeFile(t, right, "cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: added\n")
	for _, optIn := range []bool{false, true} {
		args := []string{"-test.run=^TestCLIProcess$", "--", "diff", left, right, "-k", ".", "-o", "json"}
		if optIn {
			args = append(args, "--exit-code")
		}
		cmd := exec.Command(os.Args[0], args...)
		cmd.Env = append(os.Environ(), "FMP_TEST_PROCESS=1")
		out, err := cmd.Output()
		if (err != nil) != optIn {
			t.Fatalf("optIn=%v error=%v output=%s", optIn, err, out)
		}
		var doc map[string]any
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatal(err)
		}
		if doc["complete"] != true {
			t.Errorf("optIn=%v complete=%v, want true", optIn, doc["complete"])
		}
	}
}

func TestJSONFailuresAreOneDocument(t *testing.T) {
	dir := t.TempDir()
	policyDir := t.TempDir()
	writeFile(t, policyDir, ".fmp.yaml", `paths: [manifests]
policies:
  inline:
    - |
      package fmp
      import rego.v1
      violations contains {"id": "blocked", "message": "blocked"}
  fail-on: [blocked]
`)
	writeFile(t, policyDir, "manifests/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n")
	writeFile(t, dir, "hr.yaml", `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: broken
spec:
  chart:
    spec:
      chart: missing
      sourceRef:
        kind: HelmRepository
        name: missing
`)
	report, result, err := executeAction(t.Context(), logr.Discard(), &githubaction.Request{RepoA: dir, RepoB: dir, Paths: []string{"."}, RenderHelm: true})
	if err == nil || result != nil || report == nil || report.Status != githubaction.StatusError || report.ResourcesDeleted != 0 || len(report.Warnings) == 0 {
		t.Fatalf("executeAction(incomplete) report=%#v result=%#v error=%v", report, result, err)
	}
	for _, args := range [][]string{
		{"render", dir, "-k", ".", "-o", "json"},
		{"test", dir, "-k", ".", "-o", "json"},
		{"diff", dir, dir, "-k", ".", "-o", "json"},
		{"render", "-o", "json"},
		{"render", "--unknown", "-o", "json"},
		{"diff", policyDir, policyDir, "-o", "json"},
		{"test", "/does-not-exist", "-k", ".", "-o", "json"},
		{"get", "hr", dir, "-k", ".", "-o", "json"},
		{"render", dir, "-o", "json"},
	} {
		t.Run(args[0]+args[1], func(t *testing.T) {
			cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestCLIProcess$", "--"}, args...)...)
			cmd.Env = append(os.Environ(), "FMP_TEST_PROCESS=1")
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err == nil {
				t.Errorf("%v succeeded, want failure; stdout=%s", args, &stdout)
			}
			dec := json.NewDecoder(&stdout)
			var doc map[string]any
			if err := dec.Decode(&doc); err != nil {
				t.Fatalf("%v JSON error = %v; stderr=%s", args, err, &stderr)
			}
			if doc["error"] == nil {
				t.Errorf("%v = %v, want failure info", args, doc)
			}
			if err := dec.Decode(new(any)); err != io.EOF {
				t.Errorf("%v trailing decode = %v, want EOF", args, err)
			}
		})
	}
}
