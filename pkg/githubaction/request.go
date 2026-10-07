package githubaction

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
)

// Request captures normalized inputs for the GitHub Action mode.
type Request struct {
	// Diff targets
	Repo    string
	BaseRef string
	BaseSHA string
	RepoA   string
	RepoB   string
	Paths   []string

	// Cluster mode: map of cluster name → list of paths.
	// Derived from the "clusters" input and from prefixed "paths" entries.
	ClusterPaths map[string][]string

	// Render options
	Recursive           bool
	RenderHelm          bool
	ResolveGit          bool
	Sort                bool
	ExcludeCRDs         bool
	SOPSDecrypt         bool
	HelmRelease         string
	ConfigFile          string
	FilterFile          string
	FilterYAML          string
	RegistryConfig      string
	RepositoryConfig    string
	RepositoryCache     string
	Crossplane          bool
	CrossplaneEngine    string
	CrossplaneFunctions []string
	FluxPlugin          string
	CrossplanePlugin    string

	// Export
	ExportDir         string
	ExportChangedOnly bool

	// Reporting
	WriteSummary                   bool
	Comment                        bool
	CommentMode                    string // changes, always, failure
	MaxInlineDiffBytes             int
	DiffPreviewLines               int
	HTMLReport                     bool
	HTMLReportName                 string
	HTMLReportRetentionDays        int
	HTMLReportMaxResourceDiffBytes int
	AIAssessment                   string
	AIProvider                     string
	AIModel                        string
	AIFailOnError                  string

	// Policy
	FailOnWarning bool
	FailOnError   bool
}

// ParseRequestFromEnv builds a Request from GitHub Actions inputs (passed as env vars).
func ParseRequestFromEnv() (*Request, error) {
	r := &Request{
		Repo:                           getInput("repo", "."),
		BaseRef:                        getInput("base-ref", ""),
		BaseSHA:                        getInput("base-sha", ""),
		RepoA:                          getInput("repo-a", ""),
		RepoB:                          getInput("repo-b", ""),
		Paths:                          parseLines(getInput("paths", "")),
		Recursive:                      parseBool(getInput("recursive", "false")),
		RenderHelm:                     parseBool(getInput("helm", "true")),
		ResolveGit:                     parseBool(getInput("resolve-git", "false")),
		Sort:                           parseBool(getInput("sort", "false")),
		ExcludeCRDs:                    parseBool(getInput("exclude-crds", "false")),
		SOPSDecrypt:                    parseBool(getInput("sops-decrypt", "false")),
		HelmRelease:                    getInput("helm-release", ""),
		ConfigFile:                     getInput("config", ""),
		FilterFile:                     getInput("filter-file", ""),
		FilterYAML:                     getInput("filter-yaml", ""),
		RegistryConfig:                 getInput("registry-config", ""),
		RepositoryConfig:               getInput("repository-config", ""),
		RepositoryCache:                getInput("repository-cache", ""),
		Crossplane:                     parseBool(getInput("crossplane", "false")),
		CrossplaneEngine:               getInput("crossplane-engine", ""),
		CrossplaneFunctions:            parseLines(getInput("crossplane-function", "")),
		FluxPlugin:                     getInput("flux-plugin", ""),
		CrossplanePlugin:               getInput("crossplane-plugin", ""),
		ExportDir:                      getInput("export-dir", ""),
		ExportChangedOnly:              parseBool(getInput("export-changed-only", "false")),
		WriteSummary:                   parseBool(getInput("write-summary", "true")),
		Comment:                        parseBool(getInput("comment", "false")),
		CommentMode:                    getInput("comment-mode", "changes"),
		MaxInlineDiffBytes:             parseInt(getInput("max-inline-diff-bytes", "50000")),
		DiffPreviewLines:               parseInt(getInput("diff-preview-lines", "200")),
		HTMLReport:                     parseBool(getInput("html-report", "false")),
		HTMLReportName:                 getInput("html-report-name", "flux-manifest-preview-report"),
		HTMLReportRetentionDays:        parseInt(getInput("html-report-retention-days", "7")),
		HTMLReportMaxResourceDiffBytes: parseInt(getInput("html-report-max-resource-diff-bytes", "2000000")),
		AIAssessment:                   getInput("ai-assessment", ""),
		AIProvider:                     getInput("ai-provider", ""),
		AIModel:                        getInput("ai-model", ""),
		AIFailOnError:                  getInput("ai-fail-on-error", ""),
		FailOnWarning:                  parseBool(getInput("fail-on-warning", "false")),
		FailOnError:                    parseBool(getInput("fail-on-error", "true")),
	}
	if r.HTMLReportRetentionDays <= 0 {
		r.HTMLReportRetentionDays = 7
	}
	if r.HTMLReportMaxResourceDiffBytes <= 0 {
		r.HTMLReportMaxResourceDiffBytes = 2000000
	}

	if r.SOPSDecrypt {
		return nil, fmt.Errorf("sops-decrypt is intentionally unsupported in GitHub Action mode to avoid leaking decrypted content into logs, summaries, comments, or artifacts")
	}

	if r.RepoA != "" && r.RepoB != "" && len(r.Paths) == 0 {
		// Legacy mode: normalize to Repo/RepoA/RepoB fields
		r.Paths = parseLines(getInput("kustomizations", ""))
	}

	// Parse clusters input and merge with prefixed paths.
	r.ClusterPaths = r.buildClusterPaths()

	return r, nil
}

// buildClusterPaths merges the explicit "clusters" input with any prefixed
// entries found in Paths. It returns nil when no cluster mode is active.
func (r *Request) buildClusterPaths() map[string][]string {
	result := make(map[string][]string)

	// Parse prefixed paths.
	for _, p := range r.Paths {
		cluster, path := config.ParseClusterPath(p)
		if cluster != "" {
			result[cluster] = append(result[cluster], path)
		}
	}

	// Parse explicit clusters input: each line is "cluster:path".
	for _, line := range parseLines(getInput("clusters", "")) {
		cluster, path := config.ParseClusterPath(line)
		if cluster != "" && path != "" {
			result[cluster] = append(result[cluster], path)
		}
	}

	if len(result) == 0 {
		return nil
	}

	// Any unprefixed paths go into the empty cluster.
	for _, p := range r.Paths {
		cluster, path := config.ParseClusterPath(p)
		if cluster == "" {
			result[""] = append(result[""], path)
		}
	}

	return result
}

// DiffLeft returns the base/left path or ref for diffing.
func (r *Request) DiffLeft() string {
	if r.RepoA != "" {
		return r.RepoA
	}
	if r.BaseRef != "" {
		return r.BaseRef
	}
	if r.BaseSHA != "" {
		return r.BaseSHA
	}
	return "HEAD"
}

// DiffRight returns the target/right path or ref for diffing.
func (r *Request) DiffRight() string {
	if r.RepoB != "" {
		return r.RepoB
	}
	return r.Repo
}

// HasConfig returns true if an explicit config file or auto-discoverable config should be used.
func (r *Request) HasConfig() bool {
	return r.ConfigFile != "" || config.DiscoverConfigPath(r.ConfigRoot()) != ""
}

// ConfigRoot returns the directory to search for repository configuration.
func (r *Request) ConfigRoot() string {
	if r.ConfigFile != "" {
		return ""
	}
	if r.RepoA != "" {
		return r.RepoA
	}
	return r.Repo
}

// IsLegacy returns true when the legacy two-repo mode is used.
func (r *Request) IsLegacy() bool {
	return r.RepoA != "" && r.RepoB != ""
}

func getInput(name, def string) string {
	underscore := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	v := os.Getenv("INPUT_" + underscore)
	if v == "" && strings.Contains(name, "-") {
		hyphen := strings.ToUpper(name)
		v = os.Getenv("INPUT_" + hyphen)
	}
	if v == "" {
		return def
	}
	return strings.TrimSpace(v)
}

func parseBool(v string) bool {
	b, _ := strconv.ParseBool(v)
	return b
}

func parseInt(v string) int {
	if v == "" {
		return 0
	}
	n, _ := strconv.Atoi(v)
	return n
}

func parseLines(v string) []string {
	if v == "" {
		return nil
	}
	lines := strings.Split(v, "\n")
	var out []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
