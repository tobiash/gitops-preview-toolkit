package agent

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/diffsource"
	"github.com/tobiash/gitops-preview-toolkit/pkg/preview"
	"github.com/tobiash/gitops-preview-toolkit/pkg/sourcealiases"
)

var errInput = errors.New("invalid input")
var errCapacity = errors.New("capacity exceeded")
var errPermission = errors.New("capability denied")
var errCleanup = errors.New("private source cleanup failed")

const maxSourceBytes int64 = 32 << 20
const maxSourceFiles = 20000

func relative(path string) bool {
	return path != "" && filepath.IsLocal(path) && !strings.ContainsAny(path, "\\\x00")
}

// capture copies the entire source root, not just render roots: relative bases
// and Flux self-source paths must keep their original repository layout.
func (s *Service) capture(ctx context.Context, source string) (captured string, diagnostics []Diagnostic, resultErr error) {
	sourceRoot := s.root
	sourcePath := "."
	if strings.HasPrefix(source, "path:") {
		path := strings.TrimPrefix(source, "path:")
		if !relative(path) {
			return "", nil, errInput
		}
		sourceRoot = filepath.Join(s.root, path)
		sourcePath = path
		// Reject symlinks in every component, including the selected root.
		current := "."
		for _, part := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
			current = filepath.Join(current, part)
			info, err := s.workspace.Lstat(current)
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return "", nil, errInput
			}
		}
	} else if strings.HasPrefix(source, "git:") {
		ref := strings.TrimPrefix(source, "git:")
		if ref == "" || strings.ContainsAny(ref, "\x00\r\n") {
			return "", nil, errInput
		}
		cmd := exec.CommandContext(ctx, "git", "-C", s.root, "rev-parse", "--show-toplevel")
		out, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(out)) != s.root {
			return "", nil, errInput
		}
		cmd = exec.CommandContext(ctx, "git", "-C", s.root, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
		out, err = cmd.Output()
		if err != nil {
			return "", nil, errInput
		}
		sha := strings.TrimSpace(string(out))
		if len(sha) != 40 && len(sha) != 64 {
			return "", nil, errInput
		}
		for _, c := range sha {
			if !strings.ContainsRune("0123456789abcdef", c) {
				return "", nil, errInput
			}
		}
		// Bound archive extraction before calling the shared materializer.
		cmd = exec.CommandContext(ctx, "git", "-C", s.root, "ls-tree", "-r", "-l", "-z", sha)
		var listing limitedBuffer
		listing.limit = 4 << 20
		cmd.Stdout = &listing
		if err := cmd.Run(); err != nil {
			return "", nil, errCapacity
		}
		var size int64
		entries := strings.Split(listing.String(), "\x00")
		if len(entries) > maxSourceFiles+1 {
			return "", nil, errCapacity
		}
		for _, entry := range entries {
			if entry == "" {
				continue
			}
			header, _, ok := strings.Cut(entry, "\t")
			fields := strings.Fields(header)
			if !ok || len(fields) != 4 || fields[0] == "120000" || fields[1] != "blob" {
				return "", nil, errInput
			}
			n, err := strconv.ParseInt(fields[3], 10, 64)
			if err != nil || n < 0 || n > s.sourceLimit()-size {
				return "", nil, errCapacity
			}
			size += n
		}
		materialized, cleanup, err := (diffsource.Source{Kind: diffsource.KindRevision, Raw: sha, RepoRoot: s.root}).Materialize(ctx)
		if err != nil {
			return "", nil, err
		}
		defer cleanup()
		sourceRoot = materialized
	} else if source != "worktree" {
		return "", nil, errInput
	}
	var root *os.Root
	var err error
	if strings.HasPrefix(source, "git:") {
		root, err = os.OpenRoot(sourceRoot)
	} else {
		root, err = s.workspace.OpenRoot(sourcePath)
	}
	if err != nil {
		return "", nil, err
	}
	// The source root is read-only; no pending writes can be lost on close.
	defer func() { _ = root.Close() }()
	dest, err := os.MkdirTemp("", "fmp-agent-*")
	if err != nil {
		return "", nil, err
	}
	keep := false
	defer func() {
		if !keep {
			if err := os.RemoveAll(dest); err != nil {
				resultErr = errors.Join(resultErr, errCleanup)
			}
		}
	}()
	var used int64
	files, skipped := 0, false
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == "." {
			return nil
		}
		if slices.Contains([]string{".git", "node_modules", "vendor", ".venv", "venv", ".terraform", ".cache"}, entry.Name()) {
			skipped = true
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		files++
		if files > maxSourceFiles {
			return errCapacity
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errInput
		}
		to := filepath.Join(dest, path)
		if entry.IsDir() {
			return os.MkdirAll(to, 0o700)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errInput
		}
		if info.Size() > s.sourceLimit()-used {
			return errCapacity
		}
		in, err := root.Open(path)
		if err != nil {
			return err
		}
		// All input bytes are consumed before this read-only descriptor closes.
		defer func() { _ = in.Close() }()
		opened, err := in.Stat()
		if err != nil || !opened.Mode().IsRegular() {
			return errInput
		}
		out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		n, copyErr := io.Copy(out, io.LimitReader(in, s.sourceLimit()-used+1))
		closeErr := out.Close()
		used += n
		if used > s.sourceLimit() {
			return errCapacity
		}
		return errors.Join(copyErr, closeErr)
	})
	if err != nil {
		return "", nil, err
	}
	if err := sourcealiases.WriteSourceRepoURLsContext(ctx, dest, s.root); err != nil {
		return "", nil, err
	}
	keep = true
	if skipped {
		diagnostics = append(diagnostics, Diagnostic{"DirectoriesSkipped", "capture", "info", "Capture excludes .git, node_modules, vendor, virtualenv, Terraform and cache directories."})
	}
	return dest, diagnostics, nil
}

func (s *Service) sourceLimit() int64 { return min(maxSourceBytes, s.opts.MaxBytes) }

func loadConfig(root string) (*config.Config, error) {
	cfg, err := config.LoadConfig(root)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	return cfg, nil
}

func configuredPaths(cfg *config.Config, req Request) ([]string, error) {
	paths := slices.Clone(req.Paths)
	if len(paths) == 0 {
		paths = slices.Clone(cfg.Paths)
		var clusters []string
		for cluster := range cfg.Clusters {
			clusters = append(clusters, cluster)
		}
		slices.Sort(clusters)
		for _, cluster := range clusters {
			if cluster == "" || strings.Contains(cluster, ":") || len(cfg.Clusters[cluster].Paths) == 0 {
				return nil, errInput
			}
			for _, path := range cfg.Clusters[cluster].Paths {
				paths = append(paths, cluster+":"+path)
			}
		}
	}
	for _, path := range paths {
		_, p := config.ParseClusterPath(path)
		if !relative(p) {
			return nil, errInput
		}
	}
	return paths, nil
}

func configDiagnostics(cfg *config.Config) []Diagnostic {
	var diagnostics []Diagnostic
	if cfg.SOPSDecrypt != nil && *cfg.SOPSDecrypt {
		diagnostics = append(diagnostics, Diagnostic{"SOPSDisabled", "config", "warning", "SOPS decryption is never enabled in agent sessions."})
	}
	if cfg.AI != nil {
		diagnostics = append(diagnostics, Diagnostic{"AIDisabled", "config", "info", "Repository AI configuration is ignored; no nested AI is invoked."})
	}
	if cfg.HelmSettings != nil {
		diagnostics = append(diagnostics, Diagnostic{"HelmSettingsIgnored", "config", "warning", "Repository credential and cache paths are ignored; startup Helm defaults apply."})
	}
	if cfg.Crossplane != nil {
		diagnostics = append(diagnostics, Diagnostic{"CrossplaneSettingsIgnored", "config", "info", "Crossplane execution is controlled by startup options; repository runtime configuration is ignored."})
	}
	return diagnostics
}

func (s *Service) newPreview(cfg *config.Config, req Request) (*preview.Preview, error) {
	paths, err := configuredPaths(cfg, req)
	if err != nil || len(paths) == 0 {
		return nil, errInput
	}
	opts := []preview.Opt{preview.WithLogger(logr.Discard()), preview.WithPaths(paths, config.BoolOr(req.Recursive, config.BoolOr(cfg.Recursive, false))), preview.WithFluxKS(), preview.WithFilterConfig(&cfg.Filters), preview.WithStrictInputs()}
	opts = append(opts, preview.WithPluginHost(s.host))
	if config.BoolOr(cfg.Sort, false) {
		opts = append(opts, preview.WithSort())
	}
	if config.BoolOr(cfg.ExcludeCRDs, false) {
		opts = append(opts, preview.WithExcludeCRDs())
	}
	if config.BoolOr(cfg.Helm, true) {
		opts = append(opts, preview.WithHelm(&config.HelmSettings{}))
	}
	if config.BoolOr(cfg.ResolveGit, false) {
		opts = append(opts, preview.WithGitRepo())
	}
	if !s.opts.Trusted {
		opts = append(opts, preview.WithLocalOnly())
	}
	return preview.New(opts...)
}

// Freeze custom modules as inline source before deleting a private capture.
func (s *Service) capturePolicy(cfg *config.PolicyConfig, root string) (*config.PolicyConfig, error) {
	if cfg == nil {
		return nil, nil
	}
	if !s.opts.Trusted && (len(cfg.Modules) > 0 || len(cfg.Inline) > 0) {
		return nil, errPermission
	}
	copy := *cfg
	copy.Inline = slices.Clone(cfg.Inline)
	for _, pattern := range cfg.Modules {
		if !relative(pattern) {
			return nil, errInput
		}
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil || len(matches) == 0 {
			return nil, errInput
		}
		for _, path := range matches {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			copy.Inline = append(copy.Inline, string(data))
		}
	}
	copy.Modules = nil
	return &copy, nil
}
