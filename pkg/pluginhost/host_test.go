package pluginhost_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	"github.com/tobiash/gitops-preview-toolkit/pkg/pluginhost"
)

type fakeEngine struct {
	name          string
	expand        func(context.Context, *plugin.ExpandRequest, int) (*plugin.ExpandResponse, error)
	openError     error
	closeError    error
	describeError error
	version       int
	opens         []plugin.OpenRequest
	closed        []string
	inputs        [][]plugin.Resource
	sessions      []string
	starts        int
	stops         int
	describes     int
}

func (f *fakeEngine) Describe(context.Context, *plugin.DescribeRequest) (*plugin.DescribeResponse, error) {
	f.describes++
	version := plugin.ProtocolVersion
	if f.version != 0 {
		version = f.version
	}
	return &plugin.DescribeResponse{Name: f.name, ProtocolVersion: version}, f.describeError
}

func (f *fakeEngine) OpenRender(_ context.Context, request *plugin.OpenRequest) (*plugin.OpenResponse, error) {
	f.opens = append(f.opens, *request)
	if f.openError != nil {
		return nil, f.openError
	}
	return &plugin.OpenResponse{Session: fmt.Sprintf("%s-%d", f.name, len(f.opens))}, nil
}

func (f *fakeEngine) Expand(ctx context.Context, request *plugin.ExpandRequest) (*plugin.ExpandResponse, error) {
	f.inputs = append(f.inputs, slices.Clone(request.Resources))
	f.sessions = append(f.sessions, request.Session)
	return f.expand(ctx, request, len(f.inputs))
}

func (f *fakeEngine) CloseRender(ctx context.Context, request *plugin.CloseRequest) (*plugin.CloseResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.closed = append(f.closed, request.Session)
	return &plugin.CloseResponse{}, f.closeError
}

func starter(engines ...*fakeEngine) pluginhost.Starter {
	return func(_ context.Context, command plugin.Command) (plugin.Service, func() error, error) {
		for _, engine := range engines {
			if engine.name == command.Name {
				engine.starts++
				return engine, func() error { engine.stops++; return nil }, nil
			}
		}
		return nil, nil, errors.New("unknown fake engine")
	}
}

func host(t *testing.T, engines ...*fakeEngine) *pluginhost.Host {
	t.Helper()
	commands := make([]plugin.Command, 0, len(engines))
	for _, engine := range engines {
		commands = append(commands, plugin.Command{Name: engine.name})
	}
	h, err := pluginhost.NewWithStarter(commands, starter(engines...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	return h
}

func resource(t *testing.T, name, value string) plugin.Resource {
	t.Helper()
	r, err := plugin.ParseResource([]byte(fmt.Sprintf(
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: %s\n  namespace: default\ndata:\n  value: %q\n", name, value)), plugin.Provenance{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func value(t *testing.T, inventory []plugin.Resource, id string) string {
	t.Helper()
	for _, r := range inventory {
		if r.ID == id {
			object, err := plugin.Object(r)
			if err != nil {
				t.Fatal(err)
			}
			return object["data"].(map[string]any)["value"].(string)
		}
	}
	return ""
}

func expansion(id, trigger string, resources ...plugin.Resource) *plugin.ExpandResponse {
	return &plugin.ExpandResponse{Expansions: []plugin.Expansion{{ID: id, Trigger: trigger, Resources: resources}}}
}

func fixed(name string, response *plugin.ExpandResponse) *fakeEngine {
	return &fakeEngine{name: name, expand: func(context.Context, *plugin.ExpandRequest, int) (*plugin.ExpandResponse, error) {
		return response, nil
	}}
}

func complete(t *testing.T, result *pluginhost.Result, err error) {
	t.Helper()
	if err != nil || result == nil || !result.Complete {
		t.Fatalf("expected complete render, got %+v, %v", result, err)
	}
}

func incomplete(t *testing.T, result *pluginhost.Result) {
	t.Helper()
	if result == nil || result.Complete || len(result.Resources) != 0 {
		t.Fatalf("incomplete rendering exposed inventory: %+v", result)
	}
}

func TestHostPersistentProcessesFreshSessionsAndConfig(t *testing.T) {
	t.Parallel()
	root := resource(t, "root", "ok")
	flux := fixed("flux", expansion("root", "root", root))
	commands := []plugin.Command{{Name: "flux", Config: json.RawMessage(`{"keep":true,"override":1}`)}}
	h, err := pluginhost.NewWithStarter(commands, starter(flux))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	if flux.starts != 0 {
		t.Fatal("constructor started process")
	}
	commands[0].Config[0] = '!'
	for range 2 {
		result, err := h.Render(t.Context(), plugin.OpenRequest{
			Root: "repo", Paths: []string{"a"}, Cluster: "test", LocalOnly: true,
			Config: json.RawMessage(`{"flux":{"override":2,"extra":"x"}}`),
		})
		complete(t, result, err)
		if len(result.Resources) != 1 {
			t.Fatal("missing root")
		}
	}
	if flux.starts != 1 || flux.describes != 1 || len(flux.opens) != 2 || len(flux.closed) != 2 {
		t.Fatalf("bad lifecycle: %+v", flux)
	}
	if flux.closed[0] == flux.closed[1] {
		t.Fatal("session reused")
	}
	for _, open := range flux.opens {
		if string(open.Config) != `{"extra":"x","keep":true,"override":2}` {
			t.Fatalf("config: %s", open.Config)
		}
		if open.Root != "repo" || open.Cluster != "test" || !open.LocalOnly {
			t.Fatal("request configuration lost")
		}
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if flux.stops != 1 {
		t.Fatalf("process stopped %d times", flux.stops)
	}
	result, err := h.Render(t.Context(), plugin.OpenRequest{})
	if err == nil {
		t.Fatal("closed host allowed rendering")
	}
	incomplete(t, result)
}

func TestHostBidirectionalSameSizeContentChanges(t *testing.T) {
	t.Parallel()
	a0 := resource(t, "a", "0")
	b0 := resource(t, "b", "0")
	flux := &fakeEngine{name: "flux", expand: func(_ context.Context, request *plugin.ExpandRequest, _ int) (*plugin.ExpandResponse, error) {
		v := "0"
		if value(t, request.Resources, b0.ID) == "1" {
			v = "2"
		}
		return expansion("shared", "root", resource(t, "a", v)), nil
	}}
	crossplane := &fakeEngine{name: "crossplane", expand: func(_ context.Context, request *plugin.ExpandRequest, _ int) (*plugin.ExpandResponse, error) {
		if value(t, request.Resources, a0.ID) == "" {
			return &plugin.ExpandResponse{}, nil
		}
		return expansion("shared", a0.ID, resource(t, "b", "1")), nil
	}}
	h := host(t, flux, crossplane)
	result, err := h.Render(t.Context(), plugin.OpenRequest{})
	complete(t, result, err)
	if value(t, result.Resources, a0.ID) != "2" || value(t, result.Resources, b0.ID) != "1" {
		t.Fatalf("content changes did not propagate: %+v", result.Resources)
	}
	if len(flux.inputs) != 4 {
		t.Fatalf("expected four sweeps, got %d", len(flux.inputs))
	}
	for i := range flux.inputs {
		a, _ := json.Marshal(flux.inputs[i])
		b, _ := json.Marshal(crossplane.inputs[i])
		if string(a) != string(b) {
			t.Fatalf("engines observed different sweep %d inputs", i)
		}
	}
}

func TestHostPropagatesAdjacentLargeIntegersWithoutChangingInventoryCount(t *testing.T) {
	t.Parallel()
	const initial int64 = 9007199254740992
	const updated int64 = 9007199254740993
	makeCounter := func(count int64) plugin.Resource {
		t.Helper()
		r, err := plugin.ParseResource([]byte(fmt.Sprintf(
			"apiVersion: example.org/v1\nkind: Counter\nmetadata:\n  name: root\nspec:\n  count: %d\n", count)), plugin.Provenance{})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	root := makeCounter(initial)
	child := resource(t, "child", "ready")
	flux := &fakeEngine{name: "flux", expand: func(_ context.Context, request *plugin.ExpandRequest, _ int) (*plugin.ExpandResponse, error) {
		count := initial
		if value(t, request.Resources, child.ID) != "" {
			count = updated
		}
		return expansion("root", "root", makeCounter(count)), nil
	}}
	other := &fakeEngine{name: "crossplane", expand: func(_ context.Context, request *plugin.ExpandRequest, _ int) (*plugin.ExpandResponse, error) {
		for _, r := range request.Resources {
			if r.ID != root.ID {
				continue
			}
			object, err := plugin.Object(r)
			if err != nil {
				t.Fatal(err)
			}
			state := "ready"
			if object["spec"].(map[string]any)["count"] == updated {
				state = "observed-update"
			}
			return expansion("child", root.ID, resource(t, "child", state)), nil
		}
		return &plugin.ExpandResponse{}, nil
	}}
	result, err := host(t, flux, other).Render(t.Context(), plugin.OpenRequest{})
	complete(t, result, err)
	if len(result.Resources) != 2 || value(t, result.Resources, child.ID) != "observed-update" {
		t.Fatalf("large integer change was not delivered to the dependent engine: %+v", result.Resources)
	}
	for _, r := range result.Resources {
		if r.ID == root.ID {
			object, err := plugin.Object(r)
			if err != nil {
				t.Fatal(err)
			}
			if object["spec"].(map[string]any)["count"] != updated {
				t.Fatalf("final count lost precision: %+v", object)
			}
		}
	}
}

func TestHostAtomicRemovalAndDisconnectedCycle(t *testing.T) {
	t.Parallel()
	root := resource(t, "root", "on")
	a := resource(t, "a", "on")
	b := resource(t, "b", "on")
	flux := &fakeEngine{name: "flux"}
	// This engine models a source whose desired roots disappear after the child
	// cycle has first been observed.
	flux.expand = func(_ context.Context, _ *plugin.ExpandRequest, call int) (*plugin.ExpandResponse, error) {
		if call >= 3 {
			return &plugin.ExpandResponse{}, nil
		}
		return expansion("root", "root", root), nil
	}
	crossplane := &fakeEngine{name: "crossplane", expand: func(_ context.Context, request *plugin.ExpandRequest, call int) (*plugin.ExpandResponse, error) {
		if value(t, request.Resources, root.ID) == "" && value(t, request.Resources, a.ID) == "" {
			return &plugin.ExpandResponse{}, nil
		}
		trigger := root.ID
		if call >= 3 {
			trigger = b.ID
		}
		return &plugin.ExpandResponse{Expansions: []plugin.Expansion{
			{ID: "a", Trigger: trigger, Resources: []plugin.Resource{a}},
			{ID: "b", Trigger: a.ID, Resources: []plugin.Resource{b}},
		}}, nil
	}}
	result, err := host(t, flux, crossplane).Render(t.Context(), plugin.OpenRequest{})
	complete(t, result, err)
	if len(result.Resources) != 0 {
		t.Fatalf("disconnected cycle retained: %+v", result.Resources)
	}
}

func TestHostInternalRootClosureAndScopedExpansionIDs(t *testing.T) {
	t.Parallel()
	root := resource(t, "root", "x")
	a := resource(t, "a", "x")
	b := resource(t, "b", "x")
	c := resource(t, "c", "x")
	flux := fixed("flux", &plugin.ExpandResponse{Expansions: []plugin.Expansion{
		{ID: "same", Trigger: a.ID, Resources: []plugin.Resource{b}},
		{ID: "same", Trigger: root.ID, Resources: []plugin.Resource{a}},
		{ID: "same", Trigger: "root", Resources: []plugin.Resource{root}},
	}})
	other := fixed("crossplane", expansion("same", b.ID, c))
	result, err := host(t, other, flux).Render(t.Context(), plugin.OpenRequest{})
	complete(t, result, err)
	if len(result.Resources) != 4 {
		t.Fatalf("internal closure failed: %+v", result.Resources)
	}
}

func TestHostPrunesReplacementOfOwnedProducer(t *testing.T) {
	t.Parallel()
	root := resource(t, "root", "x")
	a := resource(t, "a", "x")
	b := resource(t, "b", "x")
	flux := &fakeEngine{name: "flux", expand: func(_ context.Context, _ *plugin.ExpandRequest, call int) (*plugin.ExpandResponse, error) {
		response := expansion("root", "root", root)
		if call == 1 {
			response.Expansions = append(response.Expansions, plugin.Expansion{ID: "old", Trigger: root.ID, Resources: []plugin.Resource{a}})
		}
		return response, nil
	}}
	other := &fakeEngine{name: "crossplane", expand: func(_ context.Context, request *plugin.ExpandRequest, _ int) (*plugin.ExpandResponse, error) {
		if value(t, request.Resources, a.ID) == "" {
			return &plugin.ExpandResponse{}, nil
		}
		return expansion("child", a.ID, b), nil
	}}
	result, err := host(t, flux, other).Render(t.Context(), plugin.OpenRequest{})
	complete(t, result, err)
	if len(result.Resources) != 1 || result.Resources[0].ID != root.ID {
		t.Fatalf("stale descendants: %+v", result.Resources)
	}
}

func TestHostDiagnosticsReplaceAndEvidenceNotInventory(t *testing.T) {
	t.Parallel()
	root := resource(t, "root", "x")
	flux := &fakeEngine{name: "flux", expand: func(_ context.Context, request *plugin.ExpandRequest, call int) (*plugin.ExpandResponse, error) {
		for _, r := range request.Resources {
			if r.ID != root.ID {
				t.Fatal("evidence fed back into inventory")
			}
		}
		response := expansion("root", "root", root)
		response.Evidence = []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"call":%d}`, call))}
		if call == 1 {
			response.Diagnostics = []plugin.Diagnostic{{Code: "pending", Severity: "error", Message: "waiting"}}
		}
		return response, nil
	}}
	result, err := host(t, flux).Render(t.Context(), plugin.OpenRequest{})
	complete(t, result, err)
	if len(flux.inputs) != 3 || len(result.Diagnostics) != 0 {
		t.Fatalf("diagnostics not replaced: %+v", result)
	}
	if len(result.Evidence) != 1 || string(result.Evidence[0]) != `{"call":3}` {
		t.Fatalf("bad evidence: %s", result.Evidence)
	}
}

func TestHostStableErrorSuppressesInventory(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"pending", "invalid-input"} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			root := resource(t, "root", "x")
			response := expansion("root", "root", root)
			response.Diagnostics = []plugin.Diagnostic{{Code: code, Severity: "error", ResourceID: "absent", Message: "waiting"}}
			result, err := host(t, fixed("flux", response)).Render(t.Context(), plugin.OpenRequest{})
			if err != nil {
				t.Fatal(err)
			}
			incomplete(t, result)
			if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != code {
				t.Fatalf("engine error lost: %+v", result)
			}
		})
	}
}

func TestHostSemanticYAMLAndRealMetadata(t *testing.T) {
	t.Parallel()
	root := resource(t, "root", "x")
	object, err := plugin.Object(root)
	if err != nil {
		t.Fatal(err)
	}
	jsonYAML, _ := json.Marshal(object)
	flux := &fakeEngine{name: "flux", expand: func(_ context.Context, _ *plugin.ExpandRequest, call int) (*plugin.ExpandResponse, error) {
		r := root
		if call%2 == 0 {
			r.YAML = string(jsonYAML)
		}
		return expansion("root", "root", r), nil
	}}
	result, err := host(t, flux).Render(t.Context(), plugin.OpenRequest{})
	complete(t, result, err)
	if len(flux.inputs) != 2 {
		t.Fatal("YAML formatting prevented convergence")
	}
	flux = &fakeEngine{name: "flux", expand: func(_ context.Context, _ *plugin.ExpandRequest, call int) (*plugin.ExpandResponse, error) {
		r := root
		if call%2 == 0 {
			r.YAML = strings.ReplaceAll(r.YAML, "namespace: default", "namespace: default\n  annotations:\n    observed: yes")
		}
		return expansion("root", "root", r), nil
	}}
	result, err = host(t, flux).Render(t.Context(), plugin.OpenRequest{})
	if err == nil || !strings.Contains(err.Error(), "oscillat") {
		t.Fatalf("real metadata stripped: %v", err)
	}
	incomplete(t, result)
}

func TestHostLogicalUnnamedResource(t *testing.T) {
	t.Parallel()
	r := plugin.Resource{ID: "logical:composition/child", Logical: true,
		YAML: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  generateName: child-\ndata:\n  value: x\n"}
	result, err := host(t, fixed("flux", expansion("root", "root", r))).Render(t.Context(), plugin.OpenRequest{})
	complete(t, result, err)
	if len(result.Resources) != 1 || result.Resources[0].YAML != r.YAML {
		t.Fatal("logical output changed")
	}
	object, err := plugin.Object(result.Resources[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := object["metadata"].(map[string]any)["name"]; exists {
		t.Fatal("concrete name synthesized")
	}
}

func TestHostValidationIsAtomic(t *testing.T) {
	t.Parallel()
	root := resource(t, "root", "x")
	badID := root
	badID.ID = "wrong"
	unnamed := plugin.Resource{ID: "logical:child", YAML: "apiVersion: v1\nkind: ConfigMap\n"}
	cases := []struct {
		name    string
		engines []*fakeEngine
	}{
		{name: "identity mismatch", engines: []*fakeEngine{fixed("flux", expansion("root", "root", root, badID))}},
		{name: "duplicate in producer", engines: []*fakeEngine{fixed("flux", expansion("root", "root", root, root))}},
		{name: "duplicate across producers", engines: []*fakeEngine{fixed("flux", &plugin.ExpandResponse{Expansions: []plugin.Expansion{
			{ID: "one", Trigger: "root", Resources: []plugin.Resource{root}},
			{ID: "two", Trigger: root.ID, Resources: []plugin.Resource{root}},
		}})}},
		{name: "duplicate across engines", engines: []*fakeEngine{
			fixed("flux", expansion("root", "root", root)), fixed("crossplane", expansion("other", root.ID, root)),
		}},
		{name: "untrusted root", engines: []*fakeEngine{fixed("crossplane", expansion("root", "root", root))}},
		{name: "unnamed nonlogical", engines: []*fakeEngine{fixed("flux", expansion("root", "root", unnamed))}},
		{name: "primitive", engines: []*fakeEngine{fixed("flux", expansion("root", "root", plugin.Resource{ID: "x", YAML: "42"}))}},
		{name: "invalid YAML", engines: []*fakeEngine{fixed("flux", expansion("root", "root", plugin.Resource{ID: "x", YAML: "["}))}},
		{name: "multiple documents", engines: []*fakeEngine{fixed("flux", expansion("root", "root", plugin.Resource{ID: root.ID, YAML: root.YAML + "---\n" + root.YAML}))}},
		{name: "malformed trailing document", engines: []*fakeEngine{fixed("flux", expansion("root", "root", plugin.Resource{ID: root.ID, YAML: root.YAML + "---\n["}))}},
		{name: "numeric namespace", engines: []*fakeEngine{fixed("flux", expansion("root", "root", plugin.Resource{ID: "v1/ConfigMap//root", YAML: strings.Replace(root.YAML, "namespace: default", "namespace: 42", 1)}))}},
		{name: "numeric generateName", engines: []*fakeEngine{fixed("flux", expansion("root", "root", plugin.Resource{ID: root.ID, YAML: strings.Replace(root.YAML, "name: root", "name: root\n  generateName: 42", 1)}))}},
		{name: "duplicate expansion", engines: []*fakeEngine{fixed("flux", &plugin.ExpandResponse{Expansions: []plugin.Expansion{
			{ID: "one", Trigger: "root"}, {ID: "one", Trigger: "root"},
		}})}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result, err := host(t, tc.engines...).Render(t.Context(), plugin.OpenRequest{})
			if err == nil {
				t.Fatal("invalid sweep accepted")
			}
			incomplete(t, result)
		})
	}
}

func TestHostApprovedRootAndMissingTrigger(t *testing.T) {
	t.Parallel()
	root := resource(t, "root", "x")
	engine := fixed("source", expansion("root", "root", root))
	h, err := pluginhost.NewWithOptions([]plugin.Command{{Name: "source"}}, pluginhost.Options{
		Start: starter(engine), RootPlugins: []string{"source"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	result, err := h.Render(t.Context(), plugin.OpenRequest{})
	complete(t, result, err)
	engine = fixed("crossplane", expansion("orphan", "nonexistent", root))
	result, err = host(t, engine).Render(t.Context(), plugin.OpenRequest{})
	if err != nil {
		t.Fatal(err)
	}
	incomplete(t, result)
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Code != "missing-trigger" {
		t.Fatalf("missing trigger not diagnosed: %+v", result)
	}
}

func TestHostRPCFailuresAndCleanup(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"describe", "version", "open", "expand", "close", "timeout"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			root := resource(t, "root", "x")
			flux := fixed("flux", expansion("root", "root", root))
			other := fixed("z-other", &plugin.ExpandResponse{})
			switch phase {
			case "describe":
				other.describeError = errors.New("crash")
			case "version":
				other.version = plugin.ProtocolVersion + 1
			case "open":
				other.openError = errors.New("crash")
			case "close":
				other.closeError = errors.New("crash")
			case "expand":
				other.expand = func(context.Context, *plugin.ExpandRequest, int) (*plugin.ExpandResponse, error) {
					return nil, errors.New("crash")
				}
			case "timeout":
				other.expand = func(ctx context.Context, _ *plugin.ExpandRequest, _ int) (*plugin.ExpandResponse, error) {
					<-ctx.Done()
					return nil, ctx.Err()
				}
			}
			timeout := time.Minute
			if phase == "timeout" {
				timeout = time.Second
			}
			h, err := pluginhost.NewWithOptions([]plugin.Command{{Name: "flux"}, {Name: "z-other"}}, pluginhost.Options{
				Start: starter(flux, other), Timeout: timeout,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = h.Close() })
			result, err := h.Render(t.Context(), plugin.OpenRequest{})
			if err == nil {
				t.Fatal("RPC failure accepted")
			}
			incomplete(t, result)
			if len(flux.closed) != 1 {
				t.Fatal("prior opened session leaked")
			}
			opened := phase != "describe" && phase != "version" && phase != "open"
			if opened && len(other.closed) != 1 {
				t.Fatal("failing engine session leaked")
			}
		})
	}
}

func TestHostBoundedIterationAndOwnershipOscillation(t *testing.T) {
	t.Parallel()
	root := resource(t, "root", "x")
	for _, test := range []struct {
		name      string
		oscillate bool
	}{
		{name: "iteration bound"}, {name: "ownership oscillation", oscillate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			flux := &fakeEngine{name: "flux", expand: func(_ context.Context, _ *plugin.ExpandRequest, call int) (*plugin.ExpandResponse, error) {
				if test.oscillate {
					return expansion(fmt.Sprintf("producer-%d", call%2), "root", root), nil
				}
				return expansion("root", "root", resource(t, "root", fmt.Sprint(call))), nil
			}}
			h, err := pluginhost.NewWithOptions([]plugin.Command{{Name: "flux"}}, pluginhost.Options{Start: starter(flux), MaxIterations: 4})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = h.Close() })
			result, err := h.Render(t.Context(), plugin.OpenRequest{})
			if err == nil {
				t.Fatal("unbounded evaluation accepted")
			}
			incomplete(t, result)
			code := "iteration-limit"
			if test.oscillate {
				code = "oscillation"
			}
			if result.Diagnostics[len(result.Diagnostics)-1].Code != code {
				t.Fatalf("bad failure diagnostic: %+v", result)
			}
		})
	}
}

func TestHostStartupContextSurvivesRender(t *testing.T) {
	t.Parallel()
	engine := fixed("flux", &plugin.ExpandResponse{})
	var processContext context.Context
	h, err := pluginhost.NewWithStarter([]plugin.Command{{Name: "flux"}},
		func(ctx context.Context, command plugin.Command) (plugin.Service, func() error, error) {
			processContext = ctx
			return starter(engine)(ctx, command)
		})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	result, err := h.Render(ctx, plugin.OpenRequest{})
	complete(t, result, err)
	cancel()
	if err := processContext.Err(); err != nil {
		t.Fatalf("render cancellation killed persistent process: %v", err)
	}
	result, err = h.Render(t.Context(), plugin.OpenRequest{})
	complete(t, result, err)
	if engine.starts != 1 {
		t.Fatal("persistent process restarted")
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if processContext.Err() == nil {
		t.Fatal("process lifetime not canceled on host close")
	}
}

func TestHostConstructorValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		commands []plugin.Command
		options  pluginhost.Options
	}{
		{name: "empty name", commands: []plugin.Command{{}}},
		{name: "duplicate name", commands: []plugin.Command{{Name: "flux"}, {Name: "flux"}}},
		{name: "invalid config", commands: []plugin.Command{{Name: "flux", Config: json.RawMessage(`{`)}}},
		{name: "primitive config", commands: []plugin.Command{{Name: "flux", Config: json.RawMessage(`1`)}}},
		{name: "null config", commands: []plugin.Command{{Name: "flux", Config: json.RawMessage(`null`)}}},
		{name: "negative bound", options: pluginhost.Options{MaxIterations: -1}},
		{name: "excessive bound", options: pluginhost.Options{MaxIterations: 65}},
		{name: "negative timeout", options: pluginhost.Options{Timeout: -time.Second}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := pluginhost.NewWithOptions(test.commands, test.options); err == nil {
				t.Fatal("invalid host configuration accepted")
			}
		})
	}
}

func TestHostWarningIsNotPending(t *testing.T) {
	t.Parallel()
	root := resource(t, "root", "x")
	response := expansion("root", "root", root)
	response.Diagnostics = []plugin.Diagnostic{{Code: "pending", Severity: "warning", Message: "informational"}}
	result, err := host(t, fixed("flux", response)).Render(t.Context(), plugin.OpenRequest{})
	complete(t, result, err)
	if len(result.Resources) != 1 || len(result.Diagnostics) != 1 {
		t.Fatal("warning changed completeness")
	}
}

func TestHostFailureAfterAcceptedSweepDoesNotExposePartialInventory(t *testing.T) {
	t.Parallel()
	root := resource(t, "root", "x")
	flux := fixed("flux", expansion("root", "root", root))
	other := &fakeEngine{name: "crossplane", expand: func(_ context.Context, _ *plugin.ExpandRequest, call int) (*plugin.ExpandResponse, error) {
		if call > 1 {
			return nil, errors.New("process crashed")
		}
		return &plugin.ExpandResponse{}, nil
	}}
	result, err := host(t, flux, other).Render(t.Context(), plugin.OpenRequest{})
	if err == nil {
		t.Fatal("crash accepted")
	}
	incomplete(t, result)
	if len(flux.closed) != 1 || len(other.closed) != 1 {
		t.Fatal("sessions leaked on crash")
	}
}

func TestHostStartupFailureCleansReturnedProcess(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"error", "nil service", "cancellation"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			stopped := 0
			var processContext context.Context
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			h, err := pluginhost.NewWithStarter([]plugin.Command{{Name: "flux"}},
				func(startCtx context.Context, _ plugin.Command) (plugin.Service, func() error, error) {
					processContext = startCtx
					closeProcess := func() error { stopped++; return nil }
					if mode == "cancellation" {
						cancel()
						<-startCtx.Done()
						return nil, closeProcess, startCtx.Err()
					}
					if mode == "error" {
						return nil, closeProcess, errors.New("startup failed")
					}
					return nil, closeProcess, nil
				})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = h.Close() })
			result, err := h.Render(ctx, plugin.OpenRequest{})
			if err == nil {
				t.Fatal("failed startup accepted")
			}
			incomplete(t, result)
			if stopped != 1 || processContext.Err() == nil {
				t.Fatal("failed process not cleaned up")
			}
		})
	}
}

func TestHostEngineSpecificConfiguration(t *testing.T) {
	t.Parallel()
	flux := fixed("flux", &plugin.ExpandResponse{})
	other := fixed("crossplane", &plugin.ExpandResponse{})
	h, err := pluginhost.NewWithStarter([]plugin.Command{
		{Name: "flux", Config: json.RawMessage(`{"base":"flux","old":true}`)},
		{Name: "crossplane", Config: json.RawMessage(`{"base":"crossplane","old":false}`)},
	}, starter(flux, other))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	result, err := h.Render(t.Context(), plugin.OpenRequest{Config: json.RawMessage(
		`{"flux":{"base":"override"},"crossplane":{"added":1}}`)})
	complete(t, result, err)
	if string(flux.opens[0].Config) != `{"base":"override","old":true}` {
		t.Fatalf("wrong Flux configuration: %s", flux.opens[0].Config)
	}
	if string(other.opens[0].Config) != `{"added":1,"base":"crossplane","old":false}` {
		t.Fatalf("wrong Crossplane configuration: %s", other.opens[0].Config)
	}
	result, err = h.Render(t.Context(), plugin.OpenRequest{Config: json.RawMessage(`{"flux":17}`)})
	if err == nil {
		t.Fatal("nonobject engine configuration accepted")
	}
	incomplete(t, result)
	if len(other.closed) != 2 {
		t.Fatal("session leaked on another engine's config error")
	}
}

func TestHostLogicalResourceValidation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		resource plugin.Resource
	}{
		{name: "missing logical prefix", resource: plugin.Resource{ID: "child", Logical: true, YAML: "apiVersion: v1\nkind: ConfigMap\n"}},
		{name: "empty logical key", resource: plugin.Resource{ID: "logical:", Logical: true, YAML: "apiVersion: v1\nkind: ConfigMap\n"}},
		{name: "missing kind", resource: plugin.Resource{ID: "logical:child", Logical: true, YAML: "apiVersion: v1\n"}},
		{name: "invalid name type", resource: plugin.Resource{ID: "logical:child", Logical: true,
			YAML: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: 42\n"}},
		{name: "invalid metadata type", resource: plugin.Resource{ID: "logical:child", Logical: true,
			YAML: "apiVersion: v1\nkind: ConfigMap\nmetadata: 42\n"}},
		{name: "numeric namespace", resource: plugin.Resource{ID: "logical:child", Logical: true,
			YAML: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  namespace: 42\n"}},
		{name: "numeric generateName", resource: plugin.Resource{ID: "logical:child", Logical: true,
			YAML: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  generateName: 42\n"}},
		{name: "named logical", resource: plugin.Resource{ID: "logical:child", Logical: true,
			YAML: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: child\n"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result, err := host(t, fixed("flux", expansion("root", "root", test.resource))).Render(t.Context(), plugin.OpenRequest{})
			if err == nil {
				t.Fatal("invalid logical resource accepted")
			}
			incomplete(t, result)
		})
	}
}

func TestHostEmptyExpansionFormattingIsSemantic(t *testing.T) {
	t.Parallel()
	flux := &fakeEngine{name: "flux", expand: func(_ context.Context, _ *plugin.ExpandRequest, call int) (*plugin.ExpandResponse, error) {
		response := expansion("empty", "root")
		if call%2 == 0 {
			response.Expansions[0].Resources = []plugin.Resource{}
		}
		return response, nil
	}}
	result, err := host(t, flux).Render(t.Context(), plugin.OpenRequest{})
	complete(t, result, err)
	if len(flux.inputs) != 2 {
		t.Fatal("empty output formatting prevented convergence")
	}
}
