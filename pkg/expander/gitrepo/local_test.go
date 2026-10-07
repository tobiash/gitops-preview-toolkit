package gitrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func TestCloseRemovesClonesIdempotently(t *testing.T) {
	e, err := NewExpander(logr.Discard())
	if err != nil {
		t.Fatal(err)
	}
	dir := e.shared.cloneDir
	if err := os.WriteFile(filepath.Join(dir, "clone"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := e.Close(); err != nil {
			t.Fatalf("Close() = %v", err)
		}
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat clone directory after Close() = %v, want not exist", err)
	}
}

func TestWriteSourceRepoURLsContextCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, script string
	}{
		{"remote discovery", "#!/bin/sh\nexec sleep 30\n"},
		{"remote URL lookup", "#!/bin/sh\nif [ \"$4\" = get-url ]; then exec sleep 30; fi\nprintf 'origin\\n'\n"},
		{"inherited pipes", "#!/bin/sh\nsleep 3\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, dest := t.TempDir(), t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(tc.script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			if err := WriteSourceRepoURLsContext(ctx, dest, dest); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("WriteSourceRepoURLsContext(%s) = %v, want deadline exceeded", tc.name, err)
			}
			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("WriteSourceRepoURLsContext(%s) blocked for %v, want bounded cancellation", tc.name, elapsed)
			}
			if _, err := os.Stat(filepath.Join(dest, sourceRepoURLsFile)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("alias file stat after cancellation = %v, want not exist", err)
			}
		})
	}
}

func TestLocalAliasesDoNotReadIncludedConfig(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(outside, []byte("[remote \"evil\"]\nurl = https://example.invalid/evil.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := "[remote \"origin\"]\nurl = https://example.invalid/self.git\n[include]\npath = " + outside + "\n"
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	e := NewLocalExpander(root, logr.Discard())
	if err := e.loadLocalAliases(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !e.matchesCurrentSource("https://example.invalid/self.git") || e.matchesCurrentSource("https://example.invalid/evil.git") {
		t.Fatalf("local aliases = %#v, want only self", e.sourceRepoURLs)
	}
}
