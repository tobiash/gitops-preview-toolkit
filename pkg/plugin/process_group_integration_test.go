//go:build integration && linux

package plugin

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCloseKillsEntireProcessGroup(t *testing.T) {
	c := helperClient(t, "tree", Options{ShutdownTimeout: 100 * time.Millisecond})
	s, err := c.OpenRender(t.Context(), &OpenRequest{})
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(s.Session)
	if err != nil {
		t.Fatal(err)
	}
	// If an assertion fails, still avoid leaving an intentionally stubborn
	// descendant behind. This child inherits the engine's process group.
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	pgid, err := syscall.Getpgid(pid)
	if err != nil || pgid != c.process.Process.Pid {
		t.Fatalf("descendant group %d, want %d: %v", pgid, c.process.Process.Pid, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile("/proc/" + s.Session + "/stat")
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		// A zombie is terminated; only its new parent can reap it. Some
		// container PID 1 implementations do not promptly reap orphans.
		if end := strings.LastIndex(string(data), ") "); end >= 0 && strings.HasPrefix(string(data[end+2:]), "Z ") {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("descendant survived process-group shutdown")
		case <-ticker.C:
		}
	}
}
