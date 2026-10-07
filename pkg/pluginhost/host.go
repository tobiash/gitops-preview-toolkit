// Package pluginhost evaluates persistent render plugins using bounded,
// synchronous desired-inventory sweeps. It never exposes an incomplete inventory.
package pluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
)

// Result contains only a converged inventory. Diagnostics and evidence describe
// the last accepted sweep; evidence is never supplied to an engine as inventory.
type Result struct {
	Resources   []plugin.Resource
	Diagnostics []plugin.Diagnostic
	Evidence    []json.RawMessage
	Complete    bool
}

// Starter starts a persistent service and returns its process cleanup function.
// A nil cleanup is permitted for in-process services. Services must honor context
// cancellation and must not retain or mutate request or response values.
type Starter func(context.Context, plugin.Command) (plugin.Service, func() error, error)

// Options configures trusted startup policy, not resource-derived instructions.
type Options struct {
	Start Starter
	// RootPlugins approves additional root-producing command names. Flux is always trusted.
	RootPlugins   []string
	MaxIterations int           // Defaults to 64, and cannot exceed 64.
	Timeout       time.Duration // Defaults to five minutes, including startup and sweeps.
}

type engine struct {
	command plugin.Command
	service plugin.Service
	close   func() error
	ready   bool
}

// Host serializes evaluations and retains each lazily started process until Close.
type Host struct {
	gate    chan struct{}
	engines []engine
	options Options
	roots   map[string]bool
	closed  bool
}

// New constructs a host without starting any processes.
func New(commands []plugin.Command) (*Host, error) {
	return NewWithOptions(commands, Options{})
}

// NewWithStarter supports deterministic in-process engines and custom transports.
func NewWithStarter(commands []plugin.Command, start Starter) (*Host, error) {
	return NewWithOptions(commands, Options{Start: start})
}

// NewWithOptions constructs a host with explicit trusted root producers and bounds.
func NewWithOptions(commands []plugin.Command, options Options) (*Host, error) {
	if options.MaxIterations == 0 {
		options.MaxIterations = 64
	}
	if options.MaxIterations < 1 || options.MaxIterations > 64 {
		return nil, errors.New("max iterations must be between 1 and 64")
	}
	if options.Timeout == 0 {
		options.Timeout = 5 * time.Minute
	}
	if options.Timeout < 0 {
		return nil, errors.New("timeout must be positive")
	}
	if options.Start == nil {
		options.Start = startProcess
	}
	h := &Host{
		gate:    make(chan struct{}, 1),
		engines: make([]engine, 0, len(commands)),
		options: options,
		roots:   map[string]bool{"flux": true},
	}
	for _, name := range options.RootPlugins {
		h.roots[name] = true
	}
	names := map[string]bool{}
	for _, command := range commands {
		if command.Name == "" || names[command.Name] {
			return nil, fmt.Errorf("empty or duplicate plugin name %q", command.Name)
		}
		if _, err := configObject(command.Config); err != nil {
			return nil, fmt.Errorf("plugin %q config: %w", command.Name, err)
		}
		names[command.Name] = true
		command.Args = slices.Clone(command.Args)
		command.Config = slices.Clone(command.Config)
		h.engines = append(h.engines, engine{command: command})
	}
	// The command list's ordering cannot influence evaluation semantics.
	slices.SortFunc(h.engines, func(a, b engine) int {
		if a.command.Name < b.command.Name {
			return -1
		}
		if a.command.Name > b.command.Name {
			return 1
		}
		return 0
	})
	return h, nil
}

func startProcess(ctx context.Context, command plugin.Command) (plugin.Service, func() error, error) {
	client, err := plugin.Start(ctx, command)
	if err != nil {
		return nil, nil, err
	}
	return client, client.Close, nil
}

// Render opens fresh sessions, computes a fixed point, and closes every opened
// session before returning. RPC, validation, timeout, or cleanup failures return
// an error and Complete=false with no resources. Stable engine errors likewise
// suppress resources, but are represented by diagnostics rather than a Go error.
// request.Config is a JSON object keyed by command name; each engine's object
// overrides matching top-level keys in that command's Config.
// An operation error retires every engine after session cleanup. A subsequent
// Render starts fresh processes; uncertain RPCs are never retried in this render.
func (h *Host) Render(ctx context.Context, request plugin.OpenRequest) (result *Result, err error) {
	result = &Result{Resources: []plugin.Resource{}, Diagnostics: []plugin.Diagnostic{}, Evidence: []json.RawMessage{}}
	ctx, cancel := context.WithTimeout(ctx, h.options.Timeout)
	defer cancel()
	select {
	case h.gate <- struct{}{}:
		defer func() { <-h.gate }()
	case <-ctx.Done():
		return result, ctx.Err()
	}
	if h.closed {
		return result, errors.New("plugin host is closed")
	}
	sessions := make([]string, len(h.engines))
	defer func() {
		// Cleanup must still run after the evaluation context has expired.
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cleanupCancel()
		for i := len(sessions) - 1; i >= 0; i-- {
			if sessions[i] == "" {
				continue
			}
			response, closeErr := h.engines[i].service.CloseRender(cleanupCtx, &plugin.CloseRequest{Session: sessions[i]})
			if closeErr == nil && response == nil {
				closeErr = errors.New("nil close response")
			}
			if closeErr != nil {
				err = errors.Join(err, fmt.Errorf("plugin %q close session: %w", h.engines[i].command.Name, closeErr))
			}
		}
		if err != nil {
			result.Complete = false
			result.Resources = []plugin.Resource{}
			err = errors.Join(err, h.retireEngines())
		}
	}()
	configs, err := configObject(request.Config)
	if err != nil {
		return result, fmt.Errorf("render config must be keyed by plugin name: %w", err)
	}
	for i := range h.engines {
		e := &h.engines[i]
		if e.service == nil {
			// The transport observes this context for its entire lifetime. During
			// startup propagate render cancellation; after startup only Close owns
			// process cancellation, so returning from Render does not kill clients.
			processCtx, cancelProcess := context.WithCancel(context.WithoutCancel(ctx))
			stopCancellation := context.AfterFunc(ctx, cancelProcess)
			service, closeProcess, startErr := h.options.Start(processCtx, e.command)
			stopCancellation()
			if ctx.Err() != nil {
				startErr = errors.Join(startErr, ctx.Err())
			}
			if startErr != nil {
				cancelProcess()
				if closeProcess != nil {
					_ = closeProcess()
				}
				return result, fmt.Errorf("plugin %q start: %w", e.command.Name, startErr)
			}
			if service == nil {
				cancelProcess()
				if closeProcess != nil {
					_ = closeProcess()
				}
				return result, fmt.Errorf("plugin %q returned nil service", e.command.Name)
			}
			e.service = service
			e.close = func() error {
				cancelProcess()
				if closeProcess != nil {
					return closeProcess()
				}
				return nil
			}
		}
		if !e.ready {
			description, describeErr := e.service.Describe(ctx, &plugin.DescribeRequest{ProtocolVersion: plugin.ProtocolVersion})
			if describeErr != nil {
				return result, fmt.Errorf("plugin %q describe: %w", e.command.Name, describeErr)
			}
			if description == nil {
				return result, fmt.Errorf("plugin %q returned nil description", e.command.Name)
			}
			if description.ProtocolVersion != plugin.ProtocolVersion || description.Name != e.command.Name {
				return result, fmt.Errorf("plugin %q incompatible description", e.command.Name)
			}
			e.ready = true
		}
		open := request
		open.Paths = slices.Clone(request.Paths)
		open.Config, err = mergeConfig(e.command.Config, configs[e.command.Name])
		if err != nil {
			return result, fmt.Errorf("plugin %q render config: %w", e.command.Name, err)
		}
		response, openErr := e.service.OpenRender(ctx, &open)
		if openErr != nil {
			return result, fmt.Errorf("plugin %q open session: %w", e.command.Name, openErr)
		}
		if response == nil || response.Session == "" {
			return result, fmt.Errorf("plugin %q returned empty session", e.command.Name)
		}
		sessions[i] = response.Session
	}
	return h.evaluate(ctx, sessions, result)
}

// Close releases all started processes. It is idempotent and waits for any active
// render, including its session cleanup, to finish.
func (h *Host) Close() error {
	h.gate <- struct{}{}
	defer func() { <-h.gate }()
	if h.closed {
		return nil
	}
	h.closed = true
	return h.retireEngines()
}

// retireEngines runs only while the host gate is held, after session cleanup.
// Clear cached readiness and ownership before disposing of the old instances.
func (h *Host) retireEngines() error {
	var err error
	for i := len(h.engines) - 1; i >= 0; i-- {
		e := &h.engines[i]
		closeProcess := e.close
		e.service, e.close, e.ready = nil, nil, false
		if closeProcess != nil {
			if closeErr := closeProcess(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("plugin %q retire process: %w", e.command.Name, closeErr))
			}
		}
	}
	return err
}

func configObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	object := map[string]json.RawMessage{}
	if len(raw) == 0 {
		return object, nil
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("config must be a JSON object")
	}
	return object, nil
}

func mergeConfig(base, override json.RawMessage) (json.RawMessage, error) {
	merged, err := configObject(base)
	if err != nil {
		return nil, err
	}
	changes, err := configObject(override)
	if err != nil {
		return nil, err
	}
	for key, value := range changes {
		merged[key] = value
	}
	return json.Marshal(merged)
}
