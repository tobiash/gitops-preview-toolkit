package gitrepo

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	fluxgit "github.com/fluxcd/pkg/git"
	fluxgogit "github.com/fluxcd/pkg/git/gogit"
	gitrepository "github.com/fluxcd/pkg/git/repository"
	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/expander"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"github.com/tobiash/gitops-preview-toolkit/pkg/sourcealiases"
	"golang.org/x/sync/singleflight"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/kustomize/kyaml/resid"
)

var gitRepoGVK = resid.NewGvk("source.toolkit.fluxcd.io", "v1", "GitRepository")

type cloneClient interface {
	Clone(ctx context.Context, url string, cfg gitrepository.CloneConfig) (*fluxgit.Commit, error)
	Close()
}

// Expander discovers GitRepository resources, clones external repos to temp
// directories, and makes their paths available for the expansion loop.
type Expander struct {
	log            logr.Logger
	shared         *sharedState
	localPaths     map[string]string
	sourceRoot     string
	sourceRepoURLs map[string]struct{}
	localOnly      bool
}

type sharedState struct {
	clones    cloneCache
	cloneDir  string // parent directory for clones
	group     singleflight.Group
	closeOnce sync.Once
	closeErr  error
}

type cloneCache struct {
	mu    sync.Mutex
	paths map[string]string // acquisition digest (URL and full CloneConfig) -> local path
}

const sourceRepoURLsFile = sourcealiases.FileName

var gitCloneFunc = gitClone

var newCloneClient = func(dest string, authOpts *fluxgit.AuthOptions) (cloneClient, error) {
	return fluxgogit.NewClient(
		dest,
		authOpts,
		fluxgogit.WithDiskStorage(),
		fluxgogit.WithFallbackToDefaultKnownHosts(),
	)
}

// WriteSourceRepoURLs writes source-repo aliases for a materialized tree.
// This lets archived git revision snapshots resolve self-referential GitRepository URLs.
func WriteSourceRepoURLs(path, repoRoot string) error {
	return sourcealiases.WriteSourceRepoURLs(path, repoRoot)
}

// WriteSourceRepoURLsContext writes source aliases while honoring cancellation
// during Git discovery. Like WriteSourceRepoURLs, non-Git paths have no aliases.
func WriteSourceRepoURLsContext(ctx context.Context, path, repoRoot string) error {
	return sourcealiases.WriteSourceRepoURLsContext(ctx, path, repoRoot)
}

// NewExpander creates a GitRepository expander.
// Close must be called when done to remove cloned repositories.
func NewExpander(log logr.Logger) (*Expander, error) {
	tmpDir, err := os.MkdirTemp("", "fmp-gitrepo-*")
	if err != nil {
		return nil, fmt.Errorf("creating temp dir: %w", err)
	}
	return &Expander{
		log: log,
		shared: &sharedState{
			cloneDir: tmpDir,
			clones: cloneCache{
				paths: make(map[string]string),
			},
		},
		localPaths: make(map[string]string),
	}, nil
}

// Cleanup removes all cloned repositories.
func (e *Expander) Cleanup() {
	_ = e.Close()
}

// Close removes cloned repositories once, after all rendering has stopped.
func (e *Expander) Close() error {
	if e.shared == nil {
		return nil
	}
	e.shared.closeOnce.Do(func() {
		e.shared.closeErr = os.RemoveAll(e.shared.cloneDir)
	})
	return e.shared.closeErr
}

// NewLocalExpander resolves only local sources, without allocating a clone directory.
func NewLocalExpander(root string, log logr.Logger) *Expander {
	e := &Expander{sourceRoot: root, log: log}
	e.SetLocalOnly()
	return e
}

// SetLocalOnly disables cloning and scopes local sources to the current root.
func (e *Expander) SetLocalOnly() {
	e.localOnly = true
	e.sourceRepoURLs = make(map[string]struct{})
}

func (e *Expander) loadLocalAliases(ctx context.Context) error {
	// Do not invoke Git discovery here: repository config can include outside files.
	fs := filesys.MakeFsOnDisk()
	metadata := filepath.Join(e.sourceRoot, sourceRepoURLsFile)
	if fs.Exists(metadata) {
		if err := render.ValidateLocalPath(fs, e.sourceRoot, metadata); err != nil {
			return err
		}
		for _, raw := range readSourceRepoURLsFile(e.sourceRoot) {
			if normalized, ok := normalizeGitURL(raw); ok {
				e.sourceRepoURLs[normalized] = struct{}{}
			}
		}
	}
	config := filepath.Join(e.sourceRoot, ".git", "config")
	if fs.Exists(config) {
		if err := render.ValidateLocalPath(fs, e.sourceRoot, config); err != nil {
			return err
		}
		out, err := exec.CommandContext(ctx, "git", "config", "--file", config, "--no-includes", "--get-regexp", `^remote\..*\.url$`).Output()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
				return fmt.Errorf("reading local git aliases: %w", err)
			}
		}
		for _, line := range strings.Split(string(out), "\n") {
			_, raw, ok := strings.Cut(line, " ")
			if normalized, valid := normalizeGitURL(raw); ok && valid {
				e.sourceRepoURLs[normalized] = struct{}{}
			}
		}
	}
	return nil
}

// WithSourceRoot returns a copy of the expander scoped to one source root.
// External clones remain shared, while local path aliases are per invocation.
func (e *Expander) WithSourceRoot(path string) *Expander {
	clone := &Expander{
		log:        e.log,
		shared:     e.shared,
		localPaths: make(map[string]string),
		sourceRoot: path,
	}
	if e.localOnly {
		clone.SetLocalOnly()
	} else {
		clone.sourceRepoURLs = discoverSourceRepoURLs(path)
	}
	return clone
}

// Expand implements expander.Expander.
func (e *Expander) Expand(ctx context.Context, r *render.Render) (*expander.ExpandResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	type gitRepoInfo struct {
		url   string
		clone gitrepository.CloneConfig
	}

	// Collect GitRepository resources.
	result := &expander.ExpandResult{}
	if e.localOnly {
		if err := e.loadLocalAliases(ctx); err != nil {
			result.Errors = append(result.Errors, err)
			return result, nil
		}
	}
	e.localPaths = make(map[string]string)
	repos := make(map[string]gitRepoInfo) // "namespace/name" -> info
	for _, res := range r.Resources() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		gvk := res.GetGvk()
		if gvk.Group != gitRepoGVK.Group || gvk.Kind != gitRepoGVK.Kind {
			continue
		}
		m, err := res.Map()
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("GitRepository %s: %w", res.CurId(), err))
			continue
		}
		spec, ok := m["spec"].(map[string]any)
		if !ok {
			result.Errors = append(result.Errors, fmt.Errorf("GitRepository %s has invalid spec", res.CurId()))
			continue
		}
		url, _, err := unstructured.NestedString(spec, "url")
		if err != nil || url == "" {
			result.Errors = append(result.Errors, fmt.Errorf("GitRepository %s has missing or invalid spec.url", res.CurId()))
			continue
		}
		key := res.GetNamespace() + "/" + res.GetName()
		cloneCfg, err := cloneConfigForSpec(spec)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("GitRepository %s: parsing spec: %w", key, err))
			continue
		}
		repos[key] = gitRepoInfo{url: url, clone: cloneCfg}
	}

	if len(repos) == 0 {
		return result, nil
	}

	for key, info := range repos {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.matchesCurrentSource(info.url) {
			e.log.V(1).Info("using current repo for GitRepository", "key", key, "path", e.sourceRoot)
			e.localPaths[key] = e.sourceRoot
			continue
		}

		// Only clone non-local URLs. file:// and local paths are skipped.
		if isLocalURL(info.url) {
			localPath := stripFilePrefix(info.url)
			if !filepath.IsAbs(localPath) {
				localPath = filepath.Join(e.sourceRoot, localPath)
			}
			if e.localOnly {
				if err := render.ValidateLocalPath(filesys.MakeFsOnDisk(), e.sourceRoot, localPath); err != nil {
					result.Errors = append(result.Errors, fmt.Errorf("GitRepository %s: %w", key, err))
					continue
				}
			}
			e.log.V(1).Info("using local GitRepository", "key", key, "path", localPath)
			e.localPaths[key] = localPath
			continue
		}
		if e.localOnly {
			result.Errors = append(result.Errors, fmt.Errorf("GitRepository %s: local-only mode denies remote clone %q", key, info.url))
			continue
		}

		acquisition, err := json.Marshal(struct {
			URL    string
			Config gitrepository.CloneConfig
		}{info.url, info.clone})
		if err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("GitRepository %s: acquisition key: %w", key, err))
			continue
		}
		acquisitionKey := fmt.Sprintf("%x", sha256.Sum256(acquisition))
		if err := e.ensureSharedClone(ctx, acquisitionKey, info.url, info.clone); err != nil {
			result.Errors = append(result.Errors, fmt.Errorf("GitRepository %s: %w", key, err))
			continue
		}
		e.localPaths[key], _ = e.sharedClonePath(acquisitionKey)
	}

	return result, nil
}

// ResolvePath returns the local filesystem path for a GitRepository source.
// Returns ("", false) if the repository hasn't been cloned.
func (e *Expander) ResolvePath(namespace, name string) (string, bool) {
	if e == nil {
		return "", false
	}
	key := namespace + "/" + name
	if path, ok := e.localPaths[key]; ok {
		return path, true
	}
	return "", false
}

func (e *Expander) matchesCurrentSource(rawURL string) bool {
	if e.sourceRoot == "" || len(e.sourceRepoURLs) == 0 {
		return false
	}
	normalized, ok := normalizeGitURL(rawURL)
	if !ok {
		return false
	}
	_, exists := e.sourceRepoURLs[normalized]
	return exists
}

func (e *Expander) sharedClonePath(key string) (string, bool) {
	e.shared.clones.mu.Lock()
	defer e.shared.clones.mu.Unlock()
	path, ok := e.shared.clones.paths[key]
	return path, ok
}

func (e *Expander) setSharedClonePath(key, path string) {
	e.shared.clones.mu.Lock()
	defer e.shared.clones.mu.Unlock()
	e.shared.clones.paths[key] = path
}

func (e *Expander) ensureSharedClone(ctx context.Context, key, rawURL string, cloneCfg gitrepository.CloneConfig) error {
	if _, exists := e.sharedClonePath(key); exists {
		return nil
	}

	_, err, _ := e.shared.group.Do(key, func() (any, error) {
		if _, exists := e.sharedClonePath(key); exists {
			return nil, nil
		}

		clonePath := filepath.Join(e.shared.cloneDir, key)
		if err := os.MkdirAll(filepath.Dir(clonePath), 0o755); err != nil {
			return nil, fmt.Errorf("creating clone directory: %w", err)
		}

		e.log.V(1).Info("cloning GitRepository", "key", key, "url", rawURL, "ref", describeCloneConfig(cloneCfg))
		if err := gitCloneFunc(ctx, rawURL, clonePath, cloneCfg); err != nil {
			_ = os.RemoveAll(clonePath)
			return nil, err
		}

		e.setSharedClonePath(key, clonePath)
		return nil, nil
	})
	return err
}

func cloneConfigForSpec(spec map[string]any) (gitrepository.CloneConfig, error) {
	ref, ok := spec["ref"].(map[string]any)
	if spec["ref"] != nil && !ok {
		return gitrepository.CloneConfig{}, fmt.Errorf("spec.ref must be an object")
	}
	for _, field := range []string{"branch", "tag", "semver", "name", "commit"} {
		if _, _, err := unstructured.NestedString(ref, field); err != nil {
			return gitrepository.CloneConfig{}, err
		}
	}
	if _, _, err := unstructured.NestedBool(spec, "recurseSubmodules"); err != nil {
		return gitrepository.CloneConfig{}, err
	}
	cloneCfg := gitrepository.CloneConfig{
		CheckoutStrategy:  gitrepository.CheckoutStrategy{},
		RecurseSubmodules: nestedBool(spec, "recurseSubmodules"),
		ShallowClone:      true,
	}
	if branch, _, _ := unstructured.NestedString(ref, "branch"); branch != "" {
		cloneCfg.Branch = branch
	}
	if tag, _, _ := unstructured.NestedString(ref, "tag"); tag != "" {
		cloneCfg.Tag = tag
	}
	if semver, _, _ := unstructured.NestedString(ref, "semver"); semver != "" {
		cloneCfg.SemVer = semver
	}
	if name, _, _ := unstructured.NestedString(ref, "name"); name != "" {
		cloneCfg.RefName = name
	}
	if commit, _, _ := unstructured.NestedString(ref, "commit"); commit != "" {
		cloneCfg.Commit = commit
	}
	if sparseCheckout, ok, err := unstructured.NestedStringSlice(spec, "sparseCheckout"); err != nil {
		return gitrepository.CloneConfig{}, err
	} else if ok {
		cloneCfg.SparseCheckoutDirectories = append([]string(nil), sparseCheckout...)
	}
	return cloneCfg, nil
}

func gitClone(ctx context.Context, rawURL, dest string, cloneCfg gitrepository.CloneConfig) error {
	if ctx == nil {
		ctx = context.Background()
	}
	authOpts, err := authOptionsForURL(rawURL)
	if err != nil {
		return err
	}
	client, err := newCloneClient(dest, authOpts)
	if err != nil {
		return fmt.Errorf("creating git client for %s: %w", rawURL, err)
	}
	defer client.Close()
	if _, err := client.Clone(ctx, rawURL, cloneCfg); err != nil {
		return fmt.Errorf("cloning %s: %w", rawURL, err)
	}
	return nil
}

func authOptionsForURL(rawURL string) (*fluxgit.AuthOptions, error) {
	u, err := parseGitURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parsing git url %q: %w", rawURL, err)
	}
	if u.Scheme == string(fluxgit.SSH) {
		username := u.User.Username()
		if username == "" {
			username = fluxgit.DefaultPublicKeyAuthUser
		}
		return &fluxgit.AuthOptions{
			Transport: fluxgit.SSH,
			Host:      u.Host,
			Username:  username,
		}, nil
	}
	return fluxgit.NewAuthOptions(*u, nil)
}

func parseGitURL(raw string) (*url.URL, error) {
	if strings.Contains(raw, "://") {
		return url.Parse(raw)
	}
	if at := strings.Index(raw, "@"); at >= 0 {
		remainder := raw[at+1:]
		parts := strings.SplitN(remainder, ":", 2)
		if len(parts) == 2 {
			return &url.URL{
				Scheme: string(fluxgit.SSH),
				User:   url.User(raw[:at]),
				Host:   parts[0],
				Path:   "/" + strings.TrimPrefix(parts[1], "/"),
			}, nil
		}
	}
	return url.Parse(raw)
}

func nestedBool(spec map[string]any, key string) bool {
	v, ok := spec[key].(bool)
	return ok && v
}

func describeCloneConfig(cfg gitrepository.CloneConfig) string {
	switch {
	case cfg.Commit != "":
		return cfg.Commit
	case cfg.RefName != "":
		return cfg.RefName
	case cfg.SemVer != "":
		return cfg.SemVer
	case cfg.Tag != "":
		return cfg.Tag
	case cfg.Branch != "":
		return cfg.Branch
	default:
		return "default"
	}
}

func isLocalURL(url string) bool {
	return filepath.IsAbs(url) || strings.HasPrefix(url, "file:") || (!strings.ContainsAny(url, ":@") && url != "")
}

func stripFilePrefix(url string) string {
	if len(url) > 5 && url[:5] == "file://" {
		return url[7:]
	}
	if len(url) > 5 && url[:5] == "file:" {
		return url[5:]
	}
	return url
}

func discoverSourceRepoURLs(path string) map[string]struct{} {
	aliases, _ := sourcealiases.Discover(context.Background(), path)
	return aliases
}

func readSourceRepoURLsFile(path string) []string {
	return sourcealiases.Read(path)
}

func normalizeGitURL(raw string) (string, bool) {
	return sourcealiases.NormalizeGitURL(raw)
}
