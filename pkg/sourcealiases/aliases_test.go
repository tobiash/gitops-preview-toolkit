package sourcealiases

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNormalizeGitURL(t *testing.T) {
	for _, tt := range []struct {
		name, input, want string
		valid             bool
	}{
		{name: "https", input: "https://GitHub.com/org/repo.git", want: "github.com/org/repo", valid: true},
		{name: "ssh", input: "ssh://git@GitHub.com/org/repo.git", want: "github.com/org/repo", valid: true},
		{name: "scp", input: "git@GitHub.com:org/repo.git", want: "github.com/org/repo", valid: true},
		{name: "file", input: "file:///tmp/repo"},
		{name: "relative path", input: "repo"},
		{name: "empty repository", input: "https://github.com/"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, valid := NormalizeGitURL(tt.input)
			if got != tt.want || valid != tt.valid {
				t.Fatalf("NormalizeGitURL(%q) = %q, %t; want %q, %t", tt.input, got, valid, tt.want, tt.valid)
			}
		})
	}
}

func TestDiscoverCombinesCapturedAliasesAndCurrentRemotes(t *testing.T) {
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet", repo},
		{"-C", repo, "remote", "add", "origin", "git@GitHub.com:org/current.git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, FileName), []byte("https://github.com/org/archive.git\nfile:///local/repo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	aliases, err := Discover(t.Context(), repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 2 {
		t.Fatalf("discovered aliases = %v", aliases)
	}
	for _, want := range []string{"github.com/org/archive", "github.com/org/current"} {
		if _, exists := aliases[want]; !exists {
			t.Fatalf("missing alias %s: %v", want, aliases)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Discover(ctx, repo); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled discovery = %v", err)
	}
}

func TestWritePreservesRemoteURLForEngineNormalization(t *testing.T) {
	repo, captured := t.TempDir(), t.TempDir()
	for _, args := range [][]string{
		{"init", "--quiet", repo},
		{"-C", repo, "remote", "add", "origin", "git@github.com:org/repo.git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := WriteSourceRepoURLsContext(t.Context(), captured, repo); err != nil {
		t.Fatal(err)
	}
	urls := Read(captured)
	if len(urls) != 1 || urls[0] != "git@github.com:org/repo.git" {
		t.Fatalf("captured URLs = %v, want original SSH remote", urls)
	}
	if got, ok := NormalizeGitURL(urls[0]); !ok || got != "github.com/org/repo" {
		t.Fatalf("captured alias cannot be normalized: %q, %t", got, ok)
	}
}

func TestWriteCancellationAndNonGitSource(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := WriteSourceRepoURLsContext(ctx, t.TempDir(), t.TempDir()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled capture = %v", err)
	}
	dir := t.TempDir()
	if err := WriteSourceRepoURLsContext(t.Context(), dir, dir); err != nil {
		t.Fatal(err)
	}
	if urls := Read(dir); len(urls) != 0 {
		t.Fatalf("non-Git source has aliases: %v", urls)
	}
}
