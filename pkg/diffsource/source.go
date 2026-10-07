package diffsource

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tobiash/gitops-preview-toolkit/pkg/sourcealiases"
)

// Kind describes how a diff source should be resolved.
type Kind string

const (
	KindAuto     Kind = ""
	KindPath     Kind = "path"
	KindRevision Kind = "revision"
	KindWorktree Kind = "worktree"
)

// Source is one side of a rendered manifest diff.
type Source struct {
	Kind     Kind
	Raw      string
	RepoRoot string
}

// Plan describes the two materialized inputs and the config root for a diff run.
type Plan struct {
	ConfigRoot string
	Left       Source
	Right      Source
}

// MaterializedPlan is a diff plan with both sources available as filesystem paths.
type MaterializedPlan struct {
	ConfigRoot string
	LeftPath   string
	RightPath  string
	LeftLabel  string
	RightLabel string
}

// ResolvePlan maps CLI diff arguments to concrete diff sources.
func ResolvePlan(args []string, cwd string) (*Plan, error) {
	switch len(args) {
	case 0:
		repoRoot, err := gitRepoRoot(cwd)
		if err != nil {
			return nil, fmt.Errorf("default diff requires a git worktree: %w", err)
		}
		return &Plan{
			ConfigRoot: repoRoot,
			Left:       Source{Kind: KindRevision, Raw: "HEAD", RepoRoot: repoRoot},
			Right:      Source{Kind: KindWorktree, Raw: repoRoot, RepoRoot: repoRoot},
		}, nil
	case 1:
		repoRoot, err := gitRepoRoot(cwd)
		if err != nil {
			return nil, fmt.Errorf("single-argument diff requires a git worktree: %w", err)
		}
		left, err := resolveSingleRevisionArg(args[0], repoRoot)
		if err != nil {
			return nil, err
		}
		return &Plan{
			ConfigRoot: repoRoot,
			Left:       left,
			Right:      Source{Kind: KindWorktree, Raw: repoRoot, RepoRoot: repoRoot},
		}, nil
	case 2:
		return resolveTwoDiffSources(args[0], args[1], cwd)
	default:
		return nil, fmt.Errorf("accepts at most 2 arg(s), received %d", len(args))
	}
}

// Materialize returns filesystem paths for both sources and one cleanup function.
func (p *Plan) Materialize(ctx context.Context) (*MaterializedPlan, func(), error) {
	leftPath, leftCleanup, err := p.Left.Materialize(ctx)
	if err != nil {
		return nil, nil, err
	}

	rightPath, rightCleanup, err := p.Right.Materialize(ctx)
	if err != nil {
		if leftCleanup != nil {
			leftCleanup()
		}
		return nil, nil, err
	}

	cleanup := func() {
		if leftCleanup != nil {
			leftCleanup()
		}
		if rightCleanup != nil {
			rightCleanup()
		}
	}

	return &MaterializedPlan{
		ConfigRoot: p.ConfigRoot,
		LeftPath:   leftPath,
		RightPath:  rightPath,
		LeftLabel:  p.Left.Label(),
		RightLabel: p.Right.Label(),
	}, cleanup, nil
}

// Label returns a human-readable source label for reports.
func (s Source) Label() string {
	switch s.Kind {
	case KindRevision:
		return s.Raw
	case KindWorktree:
		return "worktree"
	case KindPath:
		return s.Raw
	default:
		return s.Raw
	}
}

// Materialize returns a filesystem path for the source and an optional cleanup function.
func (s Source) Materialize(ctx context.Context) (string, func(), error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	switch s.Kind {
	case KindPath:
		return s.Raw, nil, nil
	case KindWorktree:
		return s.RepoRoot, nil, nil
	case KindRevision:
		return materializeRevision(ctx, s.RepoRoot, s.Raw)
	default:
		return "", nil, fmt.Errorf("unsupported diff source kind %q", s.Kind)
	}
}

func resolveSingleRevisionArg(arg, repoRoot string) (Source, error) {
	kind, value := splitDiffArg(arg)
	if kind == KindPath {
		return Source{}, fmt.Errorf("single diff argument must be a git revision; use two paths for directory diffs")
	}
	if !gitRevisionExists(repoRoot, value) {
		return Source{}, fmt.Errorf("%q is not a valid git revision; use two paths for directory diffs", arg)
	}
	return Source{Kind: KindRevision, Raw: value, RepoRoot: repoRoot}, nil
}

func resolveTwoDiffSources(leftArg, rightArg, cwd string) (*Plan, error) {
	leftKind, leftValue := splitDiffArg(leftArg)
	rightKind, rightValue := splitDiffArg(rightArg)
	leftExplicit := leftKind != KindAuto
	rightExplicit := rightKind != KindAuto

	leftPath, leftPathOK := existingPathCandidate(leftKind, leftValue, cwd)
	rightPath, rightPathOK := existingPathCandidate(rightKind, rightValue, cwd)
	if leftKind != KindRevision && rightKind != KindRevision && leftPathOK && rightPathOK {
		return &Plan{
			ConfigRoot: leftPath,
			Left:       Source{Kind: KindPath, Raw: leftPath},
			Right:      Source{Kind: KindPath, Raw: rightPath},
		}, nil
	}

	repoRoot, err := gitRepoRoot(cwd)
	if err != nil {
		if leftKind == KindRevision || rightKind == KindRevision {
			return nil, fmt.Errorf("git revision diff requires a git worktree: %w", err)
		}
	} else {
		leftRev, leftRevOK := revisionCandidate(leftKind, leftValue, repoRoot)
		rightRev, rightRevOK := revisionCandidate(rightKind, rightValue, repoRoot)
		if leftKind != KindPath && rightKind != KindPath && leftRevOK && rightRevOK {
			return &Plan{
				ConfigRoot: repoRoot,
				Left:       Source{Kind: KindRevision, Raw: leftRev, RepoRoot: repoRoot},
				Right:      Source{Kind: KindRevision, Raw: rightRev, RepoRoot: repoRoot},
			}, nil
		}
	}

	if !leftExplicit && !rightExplicit {
		return nil, fmt.Errorf("mixed or ambiguous diff inputs require explicit git: or path: prefixes")
	}

	left, err := resolveExplicitOrUnambiguousSource(leftArg, repoRoot, cwd)
	if err != nil {
		return nil, err
	}
	right, err := resolveExplicitOrUnambiguousSource(rightArg, repoRoot, cwd)
	if err != nil {
		return nil, err
	}

	configRoot := left.Raw
	if left.Kind == KindRevision || left.Kind == KindWorktree || right.Kind == KindRevision || right.Kind == KindWorktree {
		if repoRoot == "" {
			return nil, fmt.Errorf("git revision diff requires a git worktree")
		}
		configRoot = repoRoot
	}

	return &Plan{ConfigRoot: configRoot, Left: left, Right: right}, nil
}

func resolveExplicitOrUnambiguousSource(arg, repoRoot, cwd string) (Source, error) {
	kind, value := splitDiffArg(arg)
	switch kind {
	case KindPath:
		path, ok := existingPathCandidate(kind, value, cwd)
		if !ok {
			return Source{}, fmt.Errorf("path %q does not exist", value)
		}
		return Source{Kind: KindPath, Raw: path}, nil
	case KindRevision:
		if repoRoot == "" {
			return Source{}, fmt.Errorf("git revision %q requires a git worktree", value)
		}
		rev, ok := revisionCandidate(kind, value, repoRoot)
		if !ok {
			return Source{}, fmt.Errorf("%q is not a valid git revision", value)
		}
		return Source{Kind: KindRevision, Raw: rev, RepoRoot: repoRoot}, nil
	default:
		path, pathOK := existingPathCandidate(kind, value, cwd)
		rev, revOK := revisionCandidate(kind, value, repoRoot)
		switch {
		case pathOK && !revOK:
			return Source{Kind: KindPath, Raw: path}, nil
		case revOK && !pathOK:
			return Source{Kind: KindRevision, Raw: rev, RepoRoot: repoRoot}, nil
		case pathOK && revOK:
			return Source{}, fmt.Errorf("%q is ambiguous: it is both an existing path and a git revision; use path:%s or git:%s", arg, value, value)
		default:
			return Source{}, fmt.Errorf("%q is neither an existing path nor a valid git revision", arg)
		}
	}
}

func splitDiffArg(arg string) (Kind, string) {
	switch {
	case strings.HasPrefix(arg, "git:"):
		return KindRevision, strings.TrimPrefix(arg, "git:")
	case strings.HasPrefix(arg, "path:"):
		return KindPath, strings.TrimPrefix(arg, "path:")
	default:
		return KindAuto, arg
	}
}

func existingPathCandidate(kind Kind, value, cwd string) (string, bool) {
	if kind == KindRevision {
		return "", false
	}
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	if _, err := os.Stat(path); err != nil {
		return "", false
	}
	return path, true
}

func revisionCandidate(kind Kind, value, repoRoot string) (string, bool) {
	if repoRoot == "" || kind == KindPath {
		return "", false
	}
	if !gitRevisionExists(repoRoot, value) {
		return "", false
	}
	return value, true
}

func gitRepoRoot(cwd string) (string, error) {
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolving git repo root: %s", strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func gitRevisionExists(repoRoot, rev string) bool {
	if rev == "" {
		return false
	}
	cmd := exec.Command("git", "-C", repoRoot, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{tree}")
	return cmd.Run() == nil
}

func materializeRevision(ctx context.Context, repoRoot, rev string) (string, func(), error) {
	// Resolve to an object ID before archive so a revision cannot inject options.
	resolved, err := exec.CommandContext(ctx, "git", "-C", repoRoot, "rev-parse", "--verify", "--end-of-options", rev+"^{object}").Output()
	if err != nil {
		return "", nil, fmt.Errorf("resolving revision %q: %w", rev, err)
	}
	tmpDir, err := os.MkdirTemp("", "fmp-diff-*")
	if err != nil {
		return "", nil, fmt.Errorf("creating temp dir for %s: %w", rev, err)
	}
	cleanup := func() { _ = os.RemoveAll(tmpDir) }

	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "archive", "--format=tar", strings.TrimSpace(string(resolved)))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cleanup()
		return "", nil, fmt.Errorf("creating archive pipe for %s: %w", rev, err)
	}
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("starting git archive %s: %w", rev, err)
	}
	if err := extractTar(tmpDir, stdout); err != nil {
		_ = stdout.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		cleanup()
		return "", nil, fmt.Errorf("extracting git archive %s: %w", rev, err)
	}
	if err := cmd.Wait(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("git archive %s: %s: %w", rev, strings.TrimSpace(stderr.String()), err)
	}
	if err := sourcealiases.WriteSourceRepoURLsContext(ctx, tmpDir, repoRoot); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("writing source repo metadata for %s: %w", rev, err)
	}
	return tmpDir, cleanup, nil
}

func extractTar(dest string, r io.Reader) error {
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(r)
	var symlinks []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			// Validate after extraction so forward links can resolve. Root.Stat
			// follows the actual chain (including .. after symlinks), unlike a
			// lexical filepath.Join check. Dangling and cyclic links fail closed.
			for _, link := range symlinks {
				if _, err := root.Stat(link); err != nil {
					return fmt.Errorf("unsafe or unresolved tar symlink %q: %w", link, err)
				}
			}
			return nil
		}
		if err != nil {
			return err
		}

		if !filepath.IsLocal(hdr.Name) || strings.Contains(hdr.Name, "\\") {
			return fmt.Errorf("unsafe tar entry %q", hdr.Name)
		}
		target := filepath.Clean(hdr.Name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := root.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := root.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(hdr.Linkname) || strings.Contains(hdr.Linkname, "\\") || !filepath.IsLocal(filepath.Join(filepath.Dir(target), hdr.Linkname)) {
				return fmt.Errorf("unsafe tar symlink %q -> %q", hdr.Name, hdr.Linkname)
			}
			if err := root.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := root.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
			symlinks = append(symlinks, target)
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			continue
		default:
			return fmt.Errorf("unsupported tar entry type %d for %s", hdr.Typeflag, hdr.Name)
		}
	}
}
