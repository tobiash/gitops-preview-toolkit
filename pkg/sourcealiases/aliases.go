// Package sourcealiases preserves source-repository URLs in materialized trees
// without importing a rendering engine or a Git SDK.
package sourcealiases

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const FileName = ".fmp-source-repo-urls"

// WriteSourceRepoURLs writes remote aliases into a materialized tree.
func WriteSourceRepoURLs(path, repoRoot string) error {
	return WriteSourceRepoURLsContext(context.Background(), path, repoRoot)
}

// WriteSourceRepoURLsContext honors cancellation during Git discovery. Sources
// outside a Git repository have no aliases.
func WriteSourceRepoURLsContext(ctx context.Context, path, repoRoot string) error {
	urls, err := gitRemoteURLs(ctx, repoRoot)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(urls) == 0 {
		return nil
	}
	return os.WriteFile(filepath.Join(path, FileName), []byte(strings.Join(urls, "\n")+"\n"), 0o644)
}

// Read returns aliases already captured in a materialized source tree.
func Read(path string) []string {
	data, err := os.ReadFile(filepath.Join(path, FileName))
	if err != nil {
		return nil
	}
	var urls []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			urls = append(urls, line)
		}
	}
	return urls
}

// Discover combines captured aliases and current Git remotes. Callers enforcing
// local-only reads must validate captured paths and avoid unrestricted Git discovery.
func Discover(ctx context.Context, path string) (map[string]struct{}, error) {
	remotes, err := gitRemoteURLs(ctx, path)
	if err != nil {
		return nil, err
	}
	aliases := make(map[string]struct{})
	for _, raw := range append(Read(path), remotes...) {
		if normalized, ok := NormalizeGitURL(raw); ok {
			aliases[normalized] = struct{}{}
		}
	}
	return aliases, ctx.Err()
}

func gitRemoteURLs(ctx context.Context, path string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", path, "remote")
	cmd.WaitDelay = 100 * time.Millisecond
	out, err := cmd.Output()
	if err != nil {
		return nil, ctx.Err()
	}
	var urls []string
	for _, remote := range strings.Fields(string(out)) {
		cmd := exec.CommandContext(ctx, "git", "-C", path, "remote", "get-url", "--all", remote)
		cmd.WaitDelay = 100 * time.Millisecond
		remoteOut, err := cmd.Output()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		urls = append(urls, strings.Fields(string(remoteOut))...)
	}
	return urls, ctx.Err()
}

// NormalizeGitURL matches network repository identities across HTTPS and SSH
// URL forms. Local paths and file URLs are deliberately not remote aliases.
func NormalizeGitURL(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, "file:") || !strings.ContainsAny(trimmed, ":@") {
		return "", false
	}
	if strings.Contains(trimmed, "://") {
		u, err := url.Parse(trimmed)
		if err != nil || u.Host == "" {
			return "", false
		}
		host := strings.ToLower(u.Hostname())
		path := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
		if path == "" {
			return "", false
		}
		return host + "/" + path, true
	}
	if at := strings.Index(trimmed, "@"); at >= 0 {
		trimmed = trimmed[at+1:]
	}
	parts := strings.SplitN(trimmed, ":", 2)
	if len(parts) != 2 {
		return "", false
	}
	host := strings.ToLower(strings.TrimSpace(parts[0]))
	path := strings.TrimSuffix(strings.Trim(strings.TrimSpace(parts[1]), "/"), ".git")
	if host == "" || path == "" {
		return "", false
	}
	return host + "/" + path, true
}
