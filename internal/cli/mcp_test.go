package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tobiash/gitops-preview-toolkit/pkg/agent"
)

func agentProcess(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestCLIProcess$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), "FMP_TEST_PROCESS=1")
	return cmd
}

func TestMCPCommandTransport(t *testing.T) {
	for _, signal := range []bool{false, true} {
		t.Run(map[bool]string{false: "eof", true: "signal"}[signal], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			cmd := agentProcess(ctx, "mcp", "--root", t.TempDir())
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
			session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: 5 * time.Second}, nil)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer func() { _ = session.Close() }()
			tools, err := session.ListTools(ctx, nil)
			if err != nil || len(tools.Tools) != 8 {
				t.Fatalf("list: %+v %v", tools, err)
			}
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "manifest_discover", Arguments: map[string]any{}})
			if err != nil || result.IsError {
				t.Fatalf("call: %+v %v", result, err)
			}
			if signal {
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				_ = session.Wait()
			}
			closeErr := session.Close()
			if !signal && closeErr != nil {
				t.Fatalf("close: %v; stderr=%s", closeErr, &stderr)
			}
			if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
				t.Fatalf("process did not exit: %v", cmd.ProcessState)
			}
		})
	}
}

func TestAgentProcessOutput(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "pod.yaml"), []byte("apiVersion: v1\nkind: Pod\nmetadata:\n  name: example\nspec:\n  containers:\n  - name: app\n    image: app:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name       string
		args       []string
		input      string
		code       int
		jsonOutput bool
	}{
		{"discover", []string{"agent", "discover", "--root", root}, "", 0, true},
		{"schema", []string{"agent", "schema"}, "", 0, true},
		{"policy verdict", []string{"agent", "--root", root}, `{"requests":[{"operation":"render","paths":["."]},{"operation":"check","id":"@0.id","requireRequests":true}]}`, 0, true},
		{"bad operation", []string{"agent", "wrong", "--root", root}, `{}`, 2, true},
		{"mismatch", []string{"agent", "query", "--root", root}, `{"operation":"discover"}`, 2, true},
		{"mcp output", []string{"mcp", "--root", root, "-o=json"}, "", 2, false},
		{"leading output", []string{"-o=json", "mcp", "--root", root}, "", 2, false},
		{"agent output", []string{"agent", "--root", root, "-o=json"}, "", 2, false},
		{"legacy flag", []string{"mcp", "--root", root, "--sops-decrypt"}, "", 2, false},
		{"crossplane requires trust", []string{"agent", "--root", root, "--crossplane"}, "{}", 2, false},
		{"mcp crossplane requires trust", []string{"mcp", "--root", root, "--crossplane"}, "", 2, false},
		{"runtime requires opt-in", []string{"agent", "--root", root, "--crossplane-engine", "/trusted/engine"}, "{}", 2, false},
		{"trusted crossplane schema", []string{"agent", "schema", "--trusted", "--crossplane", "--crossplane-engine", "/trusted/engine", "--crossplane-function", "fn=localhost:9443", "--crossplane-plugin", "/trusted/crossplane"}, "", 0, true},
		{"no root", []string{"mcp"}, "", 2, false},
		{"invalid root", []string{"mcp", "--root", root + "/missing"}, "", 2, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := agentProcess(ctx, tt.args...)
			cmd.Stdin = strings.NewReader(tt.input)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			code := 0
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatal(err)
				}
				code = exit.ExitCode()
			}
			if code != tt.code {
				t.Fatalf("exit=%d want=%d stdout=%s stderr=%s", code, tt.code, &stdout, &stderr)
			}
			if tt.jsonOutput {
				if !json.Valid(stdout.Bytes()) {
					t.Fatalf("stdout not one JSON doc: %s", &stdout)
				}
			} else if stdout.Len() != 0 || stderr.Len() == 0 {
				t.Fatalf("stdout=%s stderr=%s", &stdout, &stderr)
			}
		})
	}
}

func TestAgentAndMCPFluxPluginStartupSelection(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "manifests/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: startup\n")
	writeFile(t, root, ".gitops-preview.yaml", `paths: [manifests]
crossplane:
  enabled: true
  engine-binary: /untrusted/engine
  development-targets:
    fn: untrusted.example:9443
plugins:
  - command: /untrusted/plugin
`)
	pluginPath := filepath.Join(testBinaryDir, "gitops-preview-flux")
	t.Run("agent", func(t *testing.T) {
		cmd := agentProcess(t.Context(), "agent", "--root", root, "--flux-plugin", pluginPath)
		cmd.Stdin = strings.NewReader(`{"operation":"render"}`)
		out, err := cmd.CombinedOutput()
		if err != nil || !json.Valid(out) || !bytes.Contains(out, []byte(`"status":"success"`)) {
			t.Fatalf("startup override render: %v output=%s", err, out)
		}
	})
	t.Run("mcp", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		cmd := agentProcess(ctx, "mcp", "--root", root, "--flux-plugin", pluginPath)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		client := mcp.NewClient(&mcp.Implementation{Name: "startup-test", Version: "1"}, nil)
		session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: 5 * time.Second}, nil)
		if err != nil {
			t.Fatalf("connect: %v stderr=%s", err, &stderr)
		}
		t.Cleanup(func() { _ = session.Close() })
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "manifest_render", Arguments: map[string]any{}})
		if err != nil || result.IsError {
			t.Fatalf("render: %+v error=%v stderr=%s", result, err, &stderr)
		}
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
	})
	// An explicit invalid executable must fail instead of falling back to a
	// sibling or accepting a repository-selected executable.
	t.Run("missing explicit plugin", func(t *testing.T) {
		cmd := agentProcess(t.Context(), "agent", "--root", root, "--flux-plugin", filepath.Join(root, "missing-plugin"))
		cmd.Stdin = strings.NewReader(`{"operation":"render"}`)
		out, err := cmd.CombinedOutput()
		if err == nil || !json.Valid(out) || !bytes.Contains(out, []byte(`"status":"failure"`)) {
			t.Fatalf("missing startup executable: %v output=%s", err, out)
		}
	})
}

func TestMCPStdioArgumentAndFrameLimits(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := agentProcess(ctx, "mcp", "--root", t.TempDir())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: 5 * time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	for _, value := range []string{"credential-CANARY", strings.Repeat("credential-CANARY", 5000)} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "manifest_discover", Arguments: map[string]any{"paths": value}})
		if err != nil {
			t.Fatalf("expected sanitized failure, got protocol error (%d bytes)", len(err.Error()))
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if !result.IsError || len(encoded) > 1024 || strings.Contains(string(encoded), "CANARY") {
			t.Fatalf("unsafe failure: bytes=%d isError=%v", len(encoded), result.IsError)
		}
	}
	_, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "manifest_discover", Arguments: map[string]any{"paths": strings.Repeat("credential-CANARY", 100000)}})
	if err == nil {
		t.Fatal("oversized frame did not end the session")
	}
	if strings.Contains(err.Error(), "CANARY") || len(err.Error()) > 1024 {
		t.Fatalf("unsafe transport error (%d bytes)", len(err.Error()))
	}
	_ = session.Close()
	if strings.Contains(stderr.String(), "CANARY") || stderr.Len() > 1024 {
		t.Fatalf("unsafe stderr (%d bytes)", stderr.Len())
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatal("server did not exit after oversized frame")
	}
}

func TestMCPStdioEscapedRequestPreservesFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := agentProcess(ctx, "mcp", "--root", t.TempDir())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("MCP process: %v; stderr=%s", err, &stderr)
		}
	}()
	if _, err := io.WriteString(stdin, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(stdout)
	var initialized struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := decoder.Decode(&initialized); err != nil {
		t.Fatal(err)
	}
	if initialized.ID != 1 || len(initialized.Error) != 0 || len(initialized.Result) == 0 {
		t.Fatalf("initialize: %+v", initialized)
	}
	if _, err := io.WriteString(stdin, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	// Do not use the SDK client's default JSON encoder: it HTML-escapes '&'
	// before sending and would exercise the adapter limit, not the service limit.
	id := strings.Repeat("&", 12000)
	arguments := `{"id":"` + id + `"}`
	escaped, err := json.Marshal(agent.Request{Operation: "query", ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if len(arguments) >= 64<<10 || len(escaped) <= 64<<10 {
		t.Fatalf("test does not straddle limits: raw=%d encoded=%d", len(arguments), len(escaped))
	}
	frame := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"manifest_query","arguments":` + arguments + "}}\n"
	if _, err := io.WriteString(stdin, frame); err != nil {
		t.Fatal(err)
	}
	var response struct {
		ID     int                 `json:"id"`
		Method string              `json:"method"`
		Result *mcp.CallToolResult `json:"result"`
		Error  json.RawMessage     `json:"error"`
	}
	for {
		if err := decoder.Decode(&response); err != nil {
			t.Fatal(err)
		}
		if response.ID != 0 || !strings.HasPrefix(response.Method, "notifications/") {
			break
		}
	}
	if response.ID != 2 || len(response.Error) != 0 || response.Result == nil || !response.Result.IsError {
		t.Fatalf("tool call: %+v", response)
	}
	data, err := json.Marshal(response.Result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var result agent.Response
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.Operation != "query" || result.Status != "failure" || result.Error == nil || result.Error.Code != "RequestTooLarge" {
		t.Fatalf("escaped request response=%s, want query/RequestTooLarge", data)
	}
	validateAgentDocument(t, data, "response")
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	for {
		var notification map[string]any
		err := decoder.Decode(&notification)
		if err == io.EOF {
			break
		}
		method, _ := notification["method"].(string)
		if err != nil || notification["jsonrpc"] != "2.0" || notification["id"] != nil || !strings.HasPrefix(method, "notifications/") {
			t.Fatalf("stdout after shutdown: %v, %v, want only MCP notifications then EOF", notification, err)
		}
	}
}
