package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Exercise the production subprocess boundary with the real Flux renderer.
var testBinaryDir string

func TestMain(m *testing.M) {
	if os.Getenv("FMP_TEST_PROCESS") == "1" {
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "cli-plugin-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testBinaryDir = dir
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", dir+string(filepath.Separator),
		"../../cmd/gitops-preview-flux", "../../cmd/fmp", "../../cmd/gitops-preview")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dir)
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
