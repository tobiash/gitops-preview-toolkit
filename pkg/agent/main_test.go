package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Exercise the installed subprocess boundary, building the real Flux plugin
// once for this package rather than introducing a production test fallback.
func TestMain(m *testing.M) {
	if runLogicalPluginProcess() {
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "agent-plugin-test-*")
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
