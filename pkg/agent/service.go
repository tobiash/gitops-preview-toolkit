package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/diff"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	"github.com/tobiash/gitops-preview-toolkit/pkg/pluginhost"
	"github.com/tobiash/gitops-preview-toolkit/pkg/preview"
	"gopkg.in/yaml.v3"
)

const maxResponseBytes = 64 << 10
const maxRequestBytes = 64 << 10

// Service serializes operations with a cancelable gate. Cache entries are private
// immutable facts; the short mutex protects lifecycle state, not rendering.
type Service struct {
	root      string
	workspace *os.Root
	opts      Options
	host      *pluginhost.Host
	gate      chan struct{}
	done      chan struct{}
	mu        sync.Mutex
	closed    bool
	cancel    context.CancelFunc
	entries   map[string]*cachedEntry
	bytes     int64
}

type entry struct {
	id       string
	snapshot *preview.Snapshot
	changes  *diff.DiffResult
	policies *config.PolicyConfig
	records  []record
}

type limitedBuffer struct {
	bytes.Buffer
	limit int64
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if int64(len(p)) > b.limit-int64(b.Len()) {
		return 0, errCapacity
	}
	return b.Buffer.Write(p)
}

func (b *limitedBuffer) ReadFrom(r io.Reader) (int64, error) {
	// Hide bytes.Buffer.ReadFrom so io.Copy cannot bypass the limit.
	return io.Copy(struct{ io.Writer }{b}, r)
}

// New binds a service to an existing workspace. Zero limits default to ten
// minutes TTL, sixteen handles, 64 MiB serialized artifacts, and two minutes per call.
func New(root string, opts Options) (*Service, error) {
	if opts.TTL < 0 || opts.MaxSnapshots < 0 || opts.MaxBytes < 0 || opts.Timeout < 0 {
		return nil, errInput
	}
	if opts.TTL == 0 {
		opts.TTL = 10 * time.Minute
	}
	if opts.MaxSnapshots == 0 {
		opts.MaxSnapshots = 16
	}
	if opts.MaxBytes == 0 {
		opts.MaxBytes = 64 << 20
	}
	if opts.Timeout == 0 {
		opts.Timeout = 2 * time.Minute
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, errors.New("invalid workspace root")
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, errors.New("workspace root is inaccessible")
	}
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return nil, errors.New("workspace root must be a directory")
	}
	opts.PluginCommands = slices.Clone(opts.PluginCommands)
	for i := range opts.PluginCommands {
		opts.PluginCommands[i].Args = slices.Clone(opts.PluginCommands[i].Args)
		opts.PluginCommands[i].Config = slices.Clone(opts.PluginCommands[i].Config)
	}
	opts.CrossplaneConfig = slices.Clone(opts.CrossplaneConfig)
	commands := opts.PluginCommands
	if len(commands) == 0 {
		commands = []plugin.Command{{Name: "flux", Command: "gitops-preview-flux"}}
	}
	if opts.Crossplane {
		commands = append(slices.Clone(commands), plugin.Command{
			Name: "crossplane", Command: "gitops-preview-crossplane", Config: opts.CrossplaneConfig,
		})
	}
	for _, command := range commands {
		if command.Command == "" {
			return nil, errInput
		}
	}
	host, err := pluginhost.New(commands)
	if err != nil {
		return nil, errInput
	}
	workspace, err := os.OpenRoot(abs)
	if err != nil {
		return nil, errors.Join(errors.New("workspace root is inaccessible"), host.Close())
	}
	return &Service{
		root: abs, workspace: workspace, opts: opts, host: host,
		gate: make(chan struct{}, 1), done: make(chan struct{}), entries: make(map[string]*cachedEntry),
	}, nil
}

// Close cancels active work, waits for cleanup, and releases all retained facts.
// It is safe to call repeatedly and concurrently with Execute.
func (s *Service) Close() error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.done)
		if s.cancel != nil {
			s.cancel()
		}
	}
	s.mu.Unlock()
	s.gate <- struct{}{}
	var err error
	if s.host != nil {
		err = s.host.Close()
		s.host = nil
	}
	s.entries = make(map[string]*cachedEntry)
	s.bytes = 0
	s.opts.PluginCommands = nil
	s.opts.CrossplaneConfig = nil
	if s.workspace != nil {
		err = errors.Join(err, s.workspace.Close())
		s.workspace = nil
	}
	<-s.gate
	return err
}

func failure(op, code, phase, message string) Response {
	return Response{SchemaVersion: "1", Operation: op, Status: "failure", Diagnostics: []Diagnostic{{code, phase, "error", message}}, Error: &Error{code, message}}
}

func success(op string, data any) Response {
	return Response{SchemaVersion: "1", Operation: op, Status: "success", Complete: true, Data: data, Diagnostics: []Diagnostic{}}
}

func publicError(op, phase string, err error) Response {
	switch {
	case errors.Is(err, errCleanup):
		return failure(op, "CleanupFailed", "cleanup", "Private source cleanup failed; details are withheld.")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return failure(op, "Canceled", phase, "Operation canceled or timed out.")
	case errors.Is(err, errInput):
		return failure(op, "InvalidInput", phase, "Invalid source, configuration, selector, or request parameters.")
	case errors.Is(err, errPermission):
		return failure(op, "PermissionDenied", phase, "Custom policy modules require startup trust.")
	case errors.Is(err, errCapacity):
		return failure(op, "CapacityExceeded", phase, "Source or session capacity exceeded; release handles or reduce input.")
	default:
		return failure(op, "OperationFailed", phase, "Operation failed; underlying details are withheld because they may contain sensitive data.")
	}
}

// Execute runs one version 1 request. Every returned response is valid JSON of
// at most 64 KiB; callers should also bound transport input before decoding it.
func (s *Service) Execute(ctx context.Context, req Request) (result Response) {
	if len(req.Operation) > 32 {
		return failure("", "InvalidInput", "request", "Invalid operation name.")
	}
	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()
	select {
	case <-ctx.Done():
		return publicError(req.Operation, "queue", ctx.Err())
	case <-s.done:
		return failure(req.Operation, "Closed", "session", "Session is closed.")
	case s.gate <- struct{}{}:
	}
	defer func() { <-s.gate }()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return failure(req.Operation, "Closed", "session", "Session is closed.")
	}
	s.cancel = cancel
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.cancel = nil; s.mu.Unlock() }()
	for id, e := range s.entries {
		if time.Since(e.created) >= s.opts.TTL {
			s.bytes -= int64(len(e.data))
			delete(s.entries, id)
		}
	}
	if err := ctx.Err(); err != nil {
		return publicError(req.Operation, "queue", err)
	}
	var input limitedBuffer
	input.limit = maxRequestBytes
	if !requestFits(req) {
		return failure(req.Operation, "RequestTooLarge", "request", "Request exceeds input limits.")
	}
	if json.NewEncoder(&input).Encode(req) != nil {
		return failure(req.Operation, "RequestTooLarge", "request", "Request exceeds 64 KiB.")
	}
	if req.Offset < 0 || req.Limit < 0 || req.Limit > 100 {
		return publicError(req.Operation, "request", errInput)
	}
	if _, err := matchOptions(req.Query); err != nil {
		return publicError(req.Operation, "request", errInput)
	}
	if err := validateFields(req.Fields); err != nil {
		return publicError(req.Operation, "request", err)
	}
	if len(req.Fields) != 0 && req.Operation != "inspect" {
		return publicError(req.Operation, "request", errInput)
	}
	if req.Operation == "render" || req.Operation == "preview" || req.Operation == "compare" {
		existing := make(map[string]bool, len(s.entries))
		for id := range s.entries {
			existing[id] = true
		}
		defer func() {
			if result.Status == "success" {
				return
			}
			// Never strand newly allocated handles when cancellation or output
			// limits prevent the caller from receiving them.
			for id, e := range s.entries {
				if !existing[id] {
					s.bytes -= int64(len(e.data))
					delete(s.entries, id)
				}
			}
		}()
	}
	var response Response
	switch req.Operation {
	case "discover":
		response = s.discover(req)
	case "render", "preview":
		response = s.render(ctx, req)
	case "compare":
		response = s.compare(ctx, req)
	case "query", "inspect", "check", "release":
		cached := s.entries[req.ID]
		if cached == nil {
			return failure(req.Operation, "NotFound", "session", "Handle is unknown, expired, or released.")
		}
		if req.Operation == "release" {
			delete(s.entries, req.ID)
			s.bytes -= int64(len(cached.data))
			response = success(req.Operation, ReleaseData{req.ID, true})
			break
		}
		e, err := cached.decode(req.ID, false)
		if err != nil {
			return publicError(req.Operation, "cache", err)
		}
		switch req.Operation {
		case "query":
			response = s.query(ctx, req, e)
		case "inspect":
			response = s.inspect(req, e)
		case "check":
			response = s.check(ctx, req, e)
		}
	default:
		response = publicError(req.Operation, "request", errInput)
	}
	if err := ctx.Err(); err != nil {
		return publicError(req.Operation, "operation", err)
	}
	return boundResponse(response)
}

func requestFits(req Request) bool {
	if len(req.Paths) > 1000 || len(req.Fields) > 100 {
		return false
	}
	strings := []string{req.Operation, req.Source, req.Base, req.Target, req.ID, req.BeforeID, req.AfterID, req.ResourceID,
		req.Query.Cluster, req.Query.Kind, req.Query.Name, req.Query.Namespace, req.Query.Group, req.Query.Labels, req.Query.Producer, req.Query.Action,
		req.MaxCPURequests, req.MaxMemRequests, req.MaxCPULimits, req.MaxMemLimits}
	strings = append(strings, req.Paths...)
	strings = append(strings, req.Fields...)
	remaining := maxRequestBytes
	for _, value := range strings {
		if len(value) > remaining {
			return false
		}
		remaining -= len(value)
	}
	return true
}

func boundResponse(r Response) Response {
	var b limitedBuffer
	b.limit = maxResponseBytes
	if err := json.NewEncoder(&b).Encode(r); err != nil {
		return failure(r.Operation, "ResponseTooLarge", "response", "Response exceeds 64 KiB; use a smaller page or field projection.")
	}
	return r
}

func opaqueID() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (s *Service) store(entries ...*entry) error {
	if len(s.entries)+len(entries) > s.opts.MaxSnapshots {
		return errCapacity
	}
	var total int64
	encoded := make([][]byte, len(entries))
	for i, e := range entries {
		var b limitedBuffer
		b.limit = s.opts.MaxBytes - s.bytes - total
		artifact := artifact{Records: e.records, Policies: e.policies, Snapshot: e.snapshot != nil}
		if e.snapshot != nil {
			if !e.snapshot.Complete {
				return errInput
			}
			artifact.Evidence = e.snapshot.Evidence
			for cluster := range e.snapshot.Clusters {
				artifact.Clusters = append(artifact.Clusters, cluster)
			}
			slices.Sort(artifact.Clusters)
		}
		if err := json.NewEncoder(&b).Encode(artifact); err != nil {
			return errCapacity
		}
		// Exact-sized storage avoids retaining bytes.Buffer's spare capacity.
		encoded[i] = make([]byte, b.Len())
		copy(encoded[i], b.Bytes())
		total += int64(len(encoded[i]))
	}
	for i, e := range entries {
		e.id = opaqueID()
		s.entries[e.id] = &cachedEntry{created: time.Now(), data: encoded[i]}
	}
	s.bytes += total
	return nil
}

func (s *Service) render(ctx context.Context, req Request) (result Response) {
	source := req.Source
	if source == "" {
		source = "worktree"
	}
	if req.Operation == "preview" {
		source = req.Base
		if source == "" {
			source = "git:HEAD"
		}
	}
	left, diagnostics, err := s.capture(ctx, source)
	if err != nil {
		return publicError(req.Operation, "capture", err)
	}
	defer func() { cleanupResult(&result, req.Operation, os.RemoveAll(left)) }()
	cfg, err := loadConfig(left)
	if err != nil {
		return publicError(req.Operation, "config", err)
	}
	if config.BoolOr(cfg.SOPSDecrypt, false) {
		return failure(req.Operation, "PermissionDenied", "config", "Required SOPS decryption is not permitted in agent sessions.")
	}
	diagnostics = append(diagnostics, configDiagnostics(cfg)...)
	policies, err := s.capturePolicy(cfg.Policies, left)
	if err != nil {
		return publicError(req.Operation, "config", err)
	}
	p, err := s.newPreview(cfg, req)
	if err != nil {
		return publicError(req.Operation, "config", err)
	}
	defer func() { cleanupResult(&result, req.Operation, p.Close()) }()
	if req.Operation == "render" {
		snapshot, err := p.RenderSnapshot(ctx, left)
		if err != nil || snapshot == nil || !snapshot.Complete {
			return failure(req.Operation, "Incomplete", "render", "Render is incomplete; no authoritative snapshot or analysis was published.")
		}
		e, err := snapshotEntry(snapshot, policies)
		if err != nil {
			return publicError(req.Operation, "render", err)
		}
		if err = s.store(e); err != nil {
			return publicError(req.Operation, "cache", err)
		}
		r := success(req.Operation, RenderData{e.id, len(e.records)})
		r.Diagnostics = append(r.Diagnostics, diagnostics...)
		if len(snapshot.Warnings) > 0 {
			r.Diagnostics = append(r.Diagnostics, warningDiagnostic())
		}
		return r
	}
	target := req.Target
	if target == "" {
		target = "worktree"
	}
	right, more, err := s.capture(ctx, target)
	if err != nil {
		return publicError(req.Operation, "capture", err)
	}
	defer func() { cleanupResult(&result, req.Operation, os.RemoveAll(right)) }()
	diagnostics = append(diagnostics, more...)
	run, err := p.RunDiff(ctx, preview.DiffRunOptions{LeftPath: left, RightPath: right, RetainSnapshots: true})
	if err != nil || run == nil || !run.Complete || run.Before == nil || run.After == nil {
		return failure(req.Operation, "Incomplete", "render", "Comparison is incomplete; no policy or AI analysis was performed.")
	}
	before, err := snapshotEntry(run.Before, policies)
	if err != nil {
		return publicError(req.Operation, "render", err)
	}
	after, err := snapshotEntry(run.After, policies)
	if err != nil {
		return publicError(req.Operation, "render", err)
	}
	changes := diffEntry(run.Result, policies)
	if err := s.store(before, after, changes); err != nil {
		return publicError(req.Operation, "cache", err)
	}
	r := success(req.Operation, previewData(changes, before.id, after.id))
	r.Diagnostics = append(r.Diagnostics, diagnostics...)
	if len(run.Warnings) > 0 {
		r.Diagnostics = append(r.Diagnostics, warningDiagnostic())
	}
	return r
}

func cleanupResult(result *Response, operation string, err error) {
	if err == nil {
		return
	}
	failed := failure(operation, "CleanupFailed", "cleanup", "Private rendering resource cleanup failed; details are withheld.")
	if result.Status == "failure" {
		result.Diagnostics = append(result.Diagnostics, failed.Diagnostics...)
		return
	}
	*result = failed
}

func warningDiagnostic() Diagnostic {
	return Diagnostic{"RenderWarning", "render", "warning", "Rendering reported warnings; details are withheld because they may contain sensitive data."}
}

func previewData(e *entry, before, after string) PreviewData {
	d := e.changes
	return PreviewData{e.id, before, after, Summary{len(d.Added), len(d.Modified), len(d.Deleted), d.TotalChanged()}, d.TotalChanged() > 0}
}

func (s *Service) compare(ctx context.Context, req Request) Response {
	ca, cb := s.entries[req.BeforeID], s.entries[req.AfterID]
	if ca == nil || cb == nil {
		return failure(req.Operation, "NotFound", "session", "Snapshot handle is unknown, expired, or released.")
	}
	a, err := ca.decode(req.BeforeID, true)
	if err != nil {
		return publicError(req.Operation, "cache", err)
	}
	b, err := cb.decode(req.AfterID, true)
	if err != nil {
		return publicError(req.Operation, "cache", err)
	}
	if a.snapshot == nil || b.snapshot == nil {
		return publicError(req.Operation, "compare", errInput)
	}
	result, err := preview.CompareSnapshots(ctx, a.snapshot, b.snapshot)
	if err != nil {
		return publicError(req.Operation, "compare", err)
	}
	restoreOrigins(result, a.records, b.records)
	e := diffEntry(result, a.policies)
	if err := s.store(e); err != nil {
		return publicError(req.Operation, "cache", err)
	}
	return success(req.Operation, previewData(e, a.id, b.id))
}

func (s *Service) discover(req Request) Response {
	if req.Source != "" && req.Source != "worktree" || req.Base != "" || req.Target != "" {
		return publicError(req.Operation, "discover", errInput)
	}
	root, err := s.workspace.OpenRoot(".")
	if err != nil {
		return publicError(req.Operation, "config", err)
	}
	// This root is read-only; closing it cannot invalidate captured data.
	defer func() { _ = root.Close() }()
	cfg := &config.Config{}
	for _, path := range []string{".gitops-preview.yaml", ".gitops-preview.yml", ".fmp.yaml", ".fmp.yml", ".github/fmp.yaml"} {
		info, err := root.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxRequestBytes {
			return publicError(req.Operation, "config", errInput)
		}
		f, err := root.Open(path)
		if err != nil {
			return publicError(req.Operation, "config", err)
		}
		var b limitedBuffer
		b.limit = maxRequestBytes
		_, readErr := io.Copy(&b, io.LimitReader(f, maxRequestBytes+1))
		closeErr := f.Close()
		if readErr != nil || closeErr != nil || b.Len() > maxRequestBytes {
			return publicError(req.Operation, "config", errInput)
		}
		if err := yaml.Unmarshal(b.Bytes(), cfg); err != nil {
			return publicError(req.Operation, "config", err)
		}
		cfg.SourcePath = path
		break
	}
	paths, err := configuredPaths(cfg, req)
	if err != nil {
		return publicError(req.Operation, "config", err)
	}
	if _, err := s.capturePolicyForDiscovery(cfg.Policies); err != nil {
		return publicError(req.Operation, "config", err)
	}
	profile := "local"
	if s.opts.Trusted {
		profile = "trusted"
	}
	r := success(req.Operation, DiscoverData{[]string{"discover", "render", "preview", "query", "inspect", "compare", "check", "release"}, []string{"1"}, profile, paths, config.BoolOr(req.Recursive, config.BoolOr(cfg.Recursive, false)), config.BoolOr(cfg.Helm, true), cfg.SourcePath != "", 20, 100, maxResponseBytes})
	r.Diagnostics = append(r.Diagnostics, configDiagnostics(cfg)...)
	return r
}

func (s *Service) capturePolicyForDiscovery(cfg *config.PolicyConfig) (bool, error) {
	if cfg != nil {
		if !s.opts.Trusted && (len(cfg.Modules) > 0 || len(cfg.Inline) > 0) {
			return false, errPermission
		}
		for _, path := range cfg.Modules {
			if !relative(path) {
				return false, errInput
			}
		}
	}
	return true, nil
}
