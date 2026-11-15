package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorJSON(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		args   []string
		code   int
		check  string
		status string
	}{
		{name: "missing roots", code: 2, check: "roots", status: "failure"},
		{name: "no config explicit root", args: []string{"-k", "."}, check: "config", status: "warning"},
		{name: "discovered config", config: "paths: [.]\n", check: "config", status: "success"},
		{name: "missing directory", config: "paths: [absent]\n", code: 2, check: "root[:1]", status: "failure"},
		{name: "missing plugin", args: []string{"-k", ".", "--flux-plugin", "/nonexistent/gitops-preview-flux"}, code: 3, check: "plugin.flux", status: "failure"},
		{name: "invalid config", config: "paths: [\n", code: 2, check: "config", status: "failure"},
		{name: "explicit missing config", args: []string{"-k", ".", "--config", "/nonexistent/config.yaml"}, code: 2, check: "config", status: "failure"},
		{name: "missing crossplane engine", args: []string{"-k", ".", "--crossplane", "--crossplane-plugin", "/nonexistent/crossplane-plugin", "--crossplane-engine", "/nonexistent/engine"}, code: 3, check: "crossplane.engine", status: "failure"},
		{name: "crossplane intent not execution", config: "paths: [.]\ncrossplane:\n  enabled: true\n", check: "crossplane", status: "skipped"},
		{name: "crossplane override without grant", args: []string{"-k", ".", "--crossplane-engine", "/nonexistent/engine"}, code: 2, check: "runtime", status: "failure"},
		{name: "config clusters override flat CLI", config: "clusters:\n  prod: absent\n", args: []string{"-k", "."}, code: 2, check: "root[prod:1]", status: "failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			if tc.config != "" {
				writeFile(t, repo, ".gitops-preview.yaml", tc.config)
			}
			args := append([]string{"doctor", repo, "--output", "json"}, tc.args...)
			cmd := exec.Command(filepath.Join(testBinaryDir, "gitops-preview"), args...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != tc.code {
				t.Fatalf("doctor(%v) exit = %v (%v), want %d; stdout=%s stderr=%s", args, cmd.ProcessState, err, tc.code, out, &stderr)
			}
			decoder := json.NewDecoder(bytes.NewReader(out))
			var doc doctorReport
			if err := decoder.Decode(&doc); err != nil {
				t.Fatalf("doctor(%v) JSON decode = %v; output=%s", args, err, out)
			}
			var extra any
			if err := decoder.Decode(&extra); err != io.EOF {
				t.Errorf("doctor(%v) trailing JSON = %v, want EOF", args, err)
			}
			if doc.Scope != "preflight" || !doc.Complete {
				t.Errorf("doctor(%v) scope/complete = %q/%v, want preflight/true", args, doc.Scope, doc.Complete)
			}
			for _, check := range doc.Checks {
				if check.ID == tc.check {
					if check.Status != tc.status {
						t.Errorf("doctor(%v) check %s = %s, want %s", args, tc.check, check.Status, tc.status)
					}
					return
				}
			}
			t.Errorf("doctor(%v) missing check %s: %s", args, tc.check, out)
		})
	}
}

func TestDoctorCrossplaneEngineVersion(t *testing.T) {
	for _, version := range []string{"v2.4.2", "v9.9.9"} {
		t.Run(version, func(t *testing.T) {
			dir := t.TempDir()
			engine := filepath.Join(dir, "engine")
			marker := filepath.Join(dir, "called")
			script := "#!/bin/sh\n[ \"$1\" = --version ] || exit 99\necho called > '" + marker + "'\necho " + version + "\n"
			if err := os.WriteFile(engine, []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(filepath.Join(testBinaryDir, "gitops-preview"), "doctor", dir, "-k", ".", "--crossplane", "--crossplane-plugin", "/nonexistent/plugin", "--crossplane-engine", engine, "--output", "json")
			out, _ := cmd.Output() // The deliberately absent Crossplane plugin makes the command fail.
			var doc doctorReport
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatalf("doctor engine JSON: %v; output=%s", err, out)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("doctor did not invoke engine --version: %v", err)
			}
			want := "success"
			if version != "v2.4.2" {
				want = "failure"
			}
			for _, check := range doc.Checks {
				if check.ID == "crossplane.engine" {
					if check.Status != want {
						t.Errorf("doctor(engine %s) = %s, want %s", version, check.Status, want)
					}
					return
				}
			}
			t.Fatal("doctor missing crossplane.engine check")
		})
	}
}

func TestDoctorJSONUsageFailure(t *testing.T) {
	for _, args := range [][]string{
		{"doctor", "one", "two", "-o", "json"},
		{"doctor", "--unknown", "-o", "json"},
	} {
		cmd := exec.Command(filepath.Join(testBinaryDir, "gitops-preview"), args...)
		out, err := cmd.Output()
		if err == nil {
			t.Fatalf("doctor(%v) succeeded, want failure", args)
		}
		var doc struct {
			Status string    `json:"status"`
			Error  jsonError `json:"error"`
		}
		if err := json.Unmarshal(out, &doc); err != nil || doc.Status != "failure" || doc.Error.Message == "" {
			t.Errorf("doctor(%v) = %s (decode %v), want single JSON failure", args, out, err)
		}
	}
}

func TestDoctorAbsentDefaultPlugin(t *testing.T) {
	// Copy the host away from its installed siblings and remove PATH fallback.
	dir := t.TempDir()
	data, err := os.ReadFile(filepath.Join(testBinaryDir, "gitops-preview"))
	if err != nil {
		t.Fatal(err)
	}
	host := filepath.Join(dir, "gitops-preview")
	if err := os.WriteFile(host, data, 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(host, "doctor", dir, "-k", ".")
	cmd.Env = append(os.Environ(), "PATH="+dir)
	out, err := cmd.CombinedOutput()
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 3 || !strings.Contains(string(out), "Cannot find executable") {
		t.Fatalf("doctor without installed plugins = %v (%v), want exit 3 and remediation: %s", cmd.ProcessState, err, out)
	}
}

func TestDoctorTextDeterministic(t *testing.T) {
	repo := t.TempDir()
	writeFile(t, repo, ".gitops-preview.yaml", "clusters:\n  z: .\n  a: .\n")
	args := []string{"doctor", repo}
	first, err := exec.Command(filepath.Join(testBinaryDir, "gitops-preview"), args...).Output()
	if err != nil {
		t.Fatal(err)
	}
	second, err := exec.Command(filepath.Join(testBinaryDir, "gitops-preview"), args...).Output()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) || !bytes.Contains(first, []byte("Describe only")) || !bytes.Contains(first, []byte("preflight only")) {
		t.Errorf("doctor text differs or omits scope: first=%s second=%s", first, second)
	}
}

func TestDoctorProtocolFailure(t *testing.T) {
	dir := t.TempDir()
	// An executable that exits instead of implementing Describe must not pass.
	pluginPath := filepath.Join(dir, "bad-plugin")
	if err := os.WriteFile(pluginPath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(testBinaryDir, "gitops-preview"), "doctor", dir, "-k", ".", "--flux-plugin", pluginPath)
	out, err := cmd.CombinedOutput()
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 3 || !bytes.Contains(out, []byte("protocol compatibility")) {
		t.Fatalf("doctor with non-protocol plugin = %v (%v), want exit 3: %s", cmd.ProcessState, err, out)
	}
}
