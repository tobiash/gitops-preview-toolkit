package crossplanerender

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	cprender "github.com/crossplane/cli/v2/cmd/crossplane/render"
	renderproto "github.com/crossplane/cli/v2/proto/render/v1alpha1"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	pkgv1 "github.com/crossplane/crossplane/apis/v2/pkg/v1"
	fnv1 "github.com/crossplane/function-sdk-go/proto/v1"
	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
)

const cleanupTimeout = 5 * time.Second

// Service owns isolated render sessions. Runtime handles and dependency-keyed
// XR evaluations persist across sweeps so unchanged random functions converge.
type Service struct {
	log      logging.Logger
	mu       sync.Mutex
	sessions map[string]*session
	closed   bool
	// execute is a unit-test seam. New always installs the real implementation.
	execute         func(context.Context, *session, cprender.CompositionInputs, []pkgv1.Function) (*rendered, error)
	removeContainer func(context.Context, string) error
}

type session struct {
	id          string
	config      Config
	timeout     time.Duration
	gate        chan struct{}
	mu          sync.Mutex
	closed      bool
	cancel      context.CancelFunc
	enginePath  string
	runtimes    map[string]cprender.RuntimeContext
	evaluations map[string]*evaluation
}

type rendered struct {
	response          *renderproto.RenderResponse
	declaredNames     map[string]string
	functionTrace     []json.RawMessage
	resourceSelectors []*fnv1.ResourceSelector
	schemaSelectors   []*fnv1.SchemaSelector
}

var _ plugin.Service = (*Service)(nil)

// New creates a service that executes official Crossplane function runtimes
// and the pinned external controller's real reconciler.
func New(log logr.Logger) *Service {
	s := &Service{log: logging.NewLogrLogger(log), sessions: map[string]*session{}}
	s.execute = s.executeReal
	s.removeContainer = removeOwnedContainer
	return s
}

func (s *Service) Describe(ctx context.Context, req *plugin.DescribeRequest) (*plugin.DescribeResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil || req.ProtocolVersion != plugin.ProtocolVersion {
		return nil, fmt.Errorf("unsupported plugin protocol version")
	}
	return &plugin.DescribeResponse{
		ProtocolVersion: plugin.ProtocolVersion, Name: "crossplane", Version: "cli-v2.5.0/core-" + EngineVersion,
	}, nil
}

func (s *Service) OpenRender(ctx context.Context, req *plugin.OpenRequest) (*plugin.OpenResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req == nil {
		return nil, fmt.Errorf("OpenRender request is required")
	}
	if req.LocalOnly {
		return nil, fmt.Errorf("crossplane execution cannot enforce localOnly filesystem and network confinement")
	}
	cfg, duration, err := parseConfig(req.Config)
	if err != nil {
		return nil, err
	}
	if req.Fresh && (cfg.Runtime == "Development" || len(cfg.DevelopmentTargets) != 0) {
		return nil, unresolved("fresh-development-unsupported",
			"fresh-development-unsupported: externally owned Development functions cannot guarantee Fresh isolation")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("crossplane service is closed")
	}
	if len(s.sessions) >= 64 {
		return nil, fmt.Errorf("crossplane session limit exceeded")
	}
	id := uuid.NewString()
	sess := &session{
		id: id, config: cfg, timeout: duration, gate: make(chan struct{}, 1),
		runtimes:    map[string]cprender.RuntimeContext{},
		evaluations: map[string]*evaluation{},
	}
	sess.gate <- struct{}{}
	// Fresh Docker sessions have plugin-owned runtime identities. Development
	// is rejected above rather than claiming freshness for external processes.
	s.sessions[id] = sess
	return &plugin.OpenResponse{Session: id}, nil
}

func (s *Service) Expand(ctx context.Context, req *plugin.ExpandRequest) (*plugin.ExpandResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("Expand request is required")
	}
	s.mu.Lock()
	sess := s.sessions[req.Session]
	s.mu.Unlock()
	if sess == nil {
		return nil, fmt.Errorf("unknown Crossplane session %q", req.Session)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-sess.gate:
	}
	ctx, cancel := context.WithTimeout(ctx, sess.timeout)
	defer cancel()
	sess.mu.Lock()
	if sess.closed {
		sess.mu.Unlock()
		sess.gate <- struct{}{}
		return nil, fmt.Errorf("crossplane session is closed")
	}
	sess.cancel = cancel
	sess.mu.Unlock()
	defer func() {
		sess.mu.Lock()
		sess.cancel = nil
		closed := sess.closed
		sess.mu.Unlock()
		if closed {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			defer cleanupCancel()
			if err := sess.cleanup(cleanupCtx); err != nil {
				s.log.Info("Crossplane runtime cleanup failed", "error", err)
			}
		}
		sess.gate <- struct{}{}
	}()
	response := &plugin.ExpandResponse{
		Expansions: []plugin.Expansion{}, Diagnostics: []plugin.Diagnostic{}, Evidence: []json.RawMessage{},
	}
	inventory := make([]inventoryResource, 0, len(req.Resources))
	seen := map[string]bool{}
	for _, resource := range req.Resources {
		if resource.Logical {
			continue
		}
		object, err := plugin.Object(resource)
		if err != nil || resource.ID == "" || seen[resource.ID] {
			return nil, fmt.Errorf("invalid or duplicate inventory resource %q: %v", resource.ID, err)
		}
		seen[resource.ID] = true
		inventory = append(inventory, inventoryResource{
			resource: resource, object: &unstructured.Unstructured{Object: object},
		})
	}
	slices.SortFunc(inventory, func(a, b inventoryResource) int { return strings.Compare(a.resource.ID, b.resource.ID) })
	definitions := []definition{}
	for _, item := range inventory {
		if item.object.GetKind() != "CompositeResourceDefinition" {
			continue
		}
		d, err := readDefinition(item.object)
		if err != nil {
			response.Diagnostics = append(response.Diagnostics, diagnostic(item.resource.ID, err))
			continue
		}
		definitions = append(definitions, d)
		conditions, found, _ := unstructured.NestedSlice(item.object.Object, "status", "conditions")
		if found {
			appendEvidence(response, map[string]any{
				"resourceId": item.resource.ID, "kind": "XRDConditions", "conditions": conditions,
			})
			for _, value := range conditions {
				condition, _ := value.(map[string]any)
				if condition["status"] != "True" {
					response.Diagnostics = append(response.Diagnostics, plugin.Diagnostic{
						Code: "xrd-condition", Severity: "warning", ResourceID: item.resource.ID,
						Message: fmt.Sprintf("XRD condition %v=%v: %v", condition["type"], condition["status"], condition["message"]),
					})
				}
			}
		}
	}
	for _, xr := range inventory {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		matches := []definition{}
		for _, d := range definitions {
			if d.matches(xr.object) {
				matches = append(matches, d)
			}
		}
		if len(matches) == 0 {
			for _, d := range definitions {
				claimKind := stringField(d.object, "spec", "claimNames", "kind")
				if claimKind != "" && xr.object.GetKind() == claimKind &&
					strings.HasPrefix(xr.object.GetAPIVersion(), d.group+"/") {
					response.Diagnostics = append(response.Diagnostics, diagnostic(xr.resource.ID,
						unresolved("unsupported-claim", "legacy claims require claim-to-XR reconciliation, which this plugin does not implement")))
				}
			}
			if looksLikeXR(xr.object, inventory) {
				response.Expansions = append(response.Expansions, emptyExpansion(xr.resource.ID))
				response.Diagnostics = append(response.Diagnostics, diagnostic(xr.resource.ID,
					unresolved("xrd-unavailable", "no served XRD defines %s %s", xr.object.GetAPIVersion(), xr.object.GetKind())))
			}
			continue
		}
		response.Expansions = append(response.Expansions, emptyExpansion(xr.resource.ID))
		expansion := &response.Expansions[len(response.Expansions)-1]
		if len(matches) > 1 {
			response.Diagnostics = append(response.Diagnostics, diagnostic(xr.resource.ID,
				unresolved("xrd-ambiguous", "multiple XRDs define this XR")))
			continue
		}
		if err := s.expandXR(ctx, sess, xr, matches[0], inventory, response, expansion); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			response.Diagnostics = append(response.Diagnostics, diagnostic(xr.resource.ID, err))
		}
	}
	current := map[string]bool{}
	for _, expansion := range response.Expansions {
		current[expansion.ID] = true
	}
	for id := range sess.evaluations {
		if !current[id] {
			delete(sess.evaluations, id)
		}
	}
	return response, nil
}

func emptyExpansion(id string) plugin.Expansion {
	return plugin.Expansion{ID: id, Trigger: id, Resources: []plugin.Resource{}}
}

func looksLikeXR(o *unstructured.Unstructured, inventory []inventoryResource) bool {
	for _, item := range inventory {
		if item.object.GetKind() == "Composition" || item.object.GetKind() == "CompositionRevision" {
			if compatible(item.object, o) {
				return true
			}
		}
	}
	return stringField(o, "spec", "crossplane", "compositionRef", "name") != "" ||
		stringField(o, "spec", "compositionRef", "name") != ""
}

func diagnostic(id string, err error) plugin.Diagnostic {
	code := "render-failed"
	var resolution *resolutionError
	if errors.As(err, &resolution) {
		code = resolution.code
	}
	return plugin.Diagnostic{Code: code, Message: err.Error(), Severity: "error", ResourceID: id}
}

func appendEvidence(response *plugin.ExpandResponse, value any) {
	if data, err := json.Marshal(value); err == nil {
		response.Evidence = append(response.Evidence, data)
	}
}

func (s *Service) expandXR(
	ctx context.Context, sess *session, xr inventoryResource, d definition,
	inventory []inventoryResource, response *plugin.ExpandResponse, expansion *plugin.Expansion,
) error {
	selected, err := selectComposition(xr.object, d, inventory)
	if err != nil {
		return err
	}
	in, err := compositionInputs(xr, d, selected, inventory)
	if err != nil {
		return err
	}
	functions, err := resolveFunctions(in.Composition, inventory, sess.config)
	if err != nil {
		return err
	}
	result, renderErr := s.evaluate(ctx, sess, xr.resource.ID, in, functions)
	if result != nil {
		appendEvidence(response, map[string]any{
			"resourceId": xr.resource.ID, "kind": "FunctionTrace", "functions": result.functionTrace,
		})
	}
	if result == nil || result.response == nil || result.response.GetComposite() == nil {
		if renderErr != nil {
			return renderErr
		}
		return fmt.Errorf("render engine returned no composite response")
	}
	compositeOut := result.response.GetComposite()
	out, parseErr := cprender.ParseCompositeResponse(compositeOut)
	if parseErr != nil {
		return errors.Join(renderErr, fmt.Errorf("parse render response: %w", parseErr))
	}
	appendEvidence(response, map[string]any{
		"resourceId": xr.resource.ID, "kind": "CrossplaneRender", "composition": selected.GetName(),
		"composite": stableCompositeEvidence(out.CompositeResource.Object), "events": out.Results,
		"requiredResources": compositeOut.GetRequiredResources(), "requiredSchemas": compositeOut.GetRequiredSchemas(),
		"deletedResources": compositeOut.GetDeletedResources(), "partial": renderErr != nil,
	})
	if renderErr != nil {
		// The official local engine returns response+error only for exit 3,
		// the pinned controller's pipeline-FATAL partial response contract.
		return unresolved("pipeline-fatal", "%v", renderErr)
	}
	resources, err := desiredResources(xr, out.ComposedResources, result.declaredNames)
	if err != nil {
		return err
	}
	expansion.Resources = resources
	return nil
}

func runtimeKey(fn pkgv1.Function) string {
	// No manifest-controlled settings survive effectiveFunction. The cache
	// key contains every setting that affects a runtime's behavior.
	annotations := map[string]string{}
	for key, value := range fn.Annotations {
		if strings.HasPrefix(key, "render.crossplane.io/") {
			annotations[key] = value
		}
	}
	data, _ := json.Marshal(struct {
		Name        string            `json:"name"`
		Package     string            `json:"package"`
		Annotations map[string]string `json:"annotations"`
	}{Name: fn.Name, Package: fn.Spec.Package, Annotations: annotations})
	return string(data)
}

func (s *Service) executeReal(
	ctx context.Context, sess *session, in cprender.CompositionInputs, functions []pkgv1.Function,
) (*rendered, error) {
	if sess.enginePath == "" {
		path, err := exec.LookPath(sess.config.EngineBinary)
		if err != nil {
			return nil, unresolved("engine-unavailable", "find trusted controller binary: %v", err)
		}
		versionCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		data, err := exec.CommandContext(versionCtx, path, "--version").Output()
		cancel()
		if err != nil {
			return nil, unresolved("engine-version", "check controller version: %v", err)
		}
		if strings.TrimSpace(string(data)) != EngineVersion {
			return nil, unresolved("engine-version", "controller must be %s, got %q", EngineVersion, strings.TrimSpace(string(data)))
		}
		sess.enginePath = path
	}
	trace := &functionTrace{declaredNames: map[string]string{}, evidence: []json.RawMessage{}}
	needed := map[string]bool{}
	for _, fn := range functions {
		needed[runtimeKey(fn)] = true
	}
	// Evict obsolete entries before constructing proxies. Eviction during
	// construction could stop a runtime another step in this pipeline uses.
	if len(sess.runtimes)+len(needed) > sess.config.MaxFunctions {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		for key, runtime := range sess.runtimes {
			if needed[key] {
				continue
			}
			if runtime.Stop != nil {
				if err := runtime.Stop(cleanupCtx); err != nil {
					return nil, fmt.Errorf("evict obsolete function runtime: %w", err)
				}
			}
			delete(sess.runtimes, key)
		}
	}
	proxies := []*functionProxy{}
	defer func() {
		for _, proxy := range proxies {
			proxy.close()
		}
	}()
	for _, fn := range functions {
		key := runtimeKey(fn)
		rctx, found := sess.runtimes[key]
		if found && rctx.Target == "" {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			err := rctx.Stop(cleanupCtx)
			cancel()
			if err != nil {
				return nil, fmt.Errorf("remove failed function runtime: %w", err)
			}
			delete(sess.runtimes, key)
			found = false
		}
		if !found {
			if len(sess.runtimes) >= sess.config.MaxFunctions {
				return nil, unresolved("runtime-limit", "persistent function runtime limit exceeded")
			}
			rt, err := cprender.GetRuntime(fn, s.log)
			if err != nil {
				return nil, err
			}
			rctx, err = s.startRuntime(ctx, sess, rt, key)
			if err != nil {
				return nil, fmt.Errorf("start Function %q: %w", fn.Name, err)
			}
			sess.runtimes[key] = rctx
		}
		proxy, addr, err := startProxy(fn.Name, rctx.Target, trace)
		if err != nil {
			return nil, err
		}
		proxies = append(proxies, proxy)
		in.FunctionAddrs[fn.Name] = addr
	}
	req, err := cprender.BuildCompositeRequest(in)
	if err != nil {
		return nil, fmt.Errorf("build render request: %w", err)
	}
	engine := cprender.NewEngineFromFlags(&cprender.EngineFlags{CrossplaneBinary: sess.enginePath}, s.log)
	rsp, renderErr := engine.Render(ctx, req)
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return &rendered{
		response: rsp, declaredNames: maps.Clone(trace.declaredNames), functionTrace: slices.Clone(trace.evidence),
		resourceSelectors: slices.Clone(trace.resourceSelectors), schemaSelectors: slices.Clone(trace.schemaSelectors),
	}, renderErr
}

func (sess *session) cleanup(ctx context.Context) error {
	errs := []error{}
	for key, rt := range sess.runtimes {
		if rt.Stop != nil {
			if err := rt.Stop(ctx); err != nil {
				errs = append(errs, err)
				continue // Retain ownership so a later close can retry removal.
			}
		}
		delete(sess.runtimes, key)
	}
	clear(sess.evaluations)
	return errors.Join(errs...)
}

func (sess *session) close(ctx context.Context) error {
	sess.mu.Lock()
	sess.closed = true
	if sess.cancel != nil {
		sess.cancel()
	}
	sess.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-sess.gate:
		defer func() { sess.gate <- struct{}{} }()
		return sess.cleanup(ctx)
	}
}

func (s *Service) CloseRender(ctx context.Context, req *plugin.CloseRequest) (*plugin.CloseResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("CloseRender request is required")
	}
	s.mu.Lock()
	sess := s.sessions[req.Session]
	s.mu.Unlock()
	if sess == nil {
		return &plugin.CloseResponse{}, nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := sess.close(cleanupCtx); err != nil {
		return &plugin.CloseResponse{}, err
	}
	s.removeClosedSession(req.Session, sess)
	return &plugin.CloseResponse{}, nil
}

func (s *Service) removeClosedSession(id string, sess *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[id] == sess {
		delete(s.sessions, id)
	}
}

// Close cancels active renders and stops all plugin-owned runtimes. It is safe
// to call repeatedly. Development fixture processes are owned by their caller.
func (s *Service) Close() error {
	s.mu.Lock()
	s.closed = true
	sessions := maps.Clone(s.sessions)
	s.mu.Unlock()
	// Close is the transport's lifecycle hook, independent of an RPC context.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	errs := []error{}
	for _, sess := range sessions {
		sess.mu.Lock()
		sess.closed = true
		if sess.cancel != nil {
			sess.cancel()
		}
		sess.mu.Unlock()
	}
	for id, sess := range sessions {
		if err := sess.close(cleanupCtx); err != nil {
			errs = append(errs, err)
			continue
		}
		s.removeClosedSession(id, sess)
	}
	return errors.Join(errs...)
}
