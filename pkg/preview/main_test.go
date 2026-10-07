package preview

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Tests exercise the production subprocess boundary. Build once and expose the
// actual Flux executable through the same PATH discovery used by installations.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "preview-plugin-test-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	command := exec.Command("go", "build", "-buildvcs=false", "-o", filepath.Join(dir, "gitops-preview-flux"), "../../cmd/gitops-preview-flux")
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	if err := command.Run(); err != nil {
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

func newTestPreview(t *testing.T, opts ...Opt) (*Preview, error) {
	t.Helper()
	p, err := New(opts...)
	if p != nil {
		t.Cleanup(func() {
			if err := p.Close(); err != nil {
				t.Errorf("closing preview plugin host: %v", err)
			}
		})
	}
	return p, err
}
