package agentmcp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// MCP tests use the same real subprocess boundary as agent sessions.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agentmcp-plugin-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", filepath.Join(dir, "gitops-preview-flux"), "../../cmd/gitops-preview-flux")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dir)
		fmt.Fprintln(os.Stderr, "building real Flux test plugin:", err)
		os.Exit(1)
	}
	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
		_ = os.RemoveAll(dir)
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
