package pluginhost_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	"github.com/tobiash/gitops-preview-toolkit/pkg/pluginhost"
)

func TestHostRetiresFailedOperationAndRestartsNextRender(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"crash", "timeout", "close", "validation", "iteration-limit"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			root := resource(t, "root", "ok")
			instances := map[string][]*fakeEngine{}
			processContexts := []context.Context{}
			start := func(ctx context.Context, command plugin.Command) (plugin.Service, func() error, error) {
				response := &plugin.ExpandResponse{}
				if command.Name == "flux" {
					response = expansion("root", "root", root)
				}
				engine := fixed(command.Name, response)
				engine.starts++
				instances[command.Name] = append(instances[command.Name], engine)
				processContexts = append(processContexts, ctx)
				return engine, func() error {
					if len(engine.closed) != len(engine.opens) {
						t.Errorf("%s retired before session cleanup: opened %d, closed %d",
							engine.name, len(engine.opens), len(engine.closed))
					}
					engine.stops++
					return nil
				}, nil
			}
			h, err := pluginhost.NewWithOptions([]plugin.Command{{Name: "flux"}, {Name: "z-other"}}, pluginhost.Options{
				Start: start, MaxIterations: 4,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = h.Close() })

			// Warm the host so both instances are already described and ready.
			result, err := h.Render(t.Context(), plugin.OpenRequest{})
			complete(t, result, err)
			oldFlux, failed := instances["flux"][0], instances["z-other"][0]
			beforeCalls := len(failed.inputs)
			ctx := t.Context()
			switch phase {
			case "crash":
				failed.expand = func(context.Context, *plugin.ExpandRequest, int) (*plugin.ExpandResponse, error) {
					return nil, errors.New("plugin process crashed")
				}
			case "timeout":
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Second)
				defer cancel()
				failed.expand = func(ctx context.Context, _ *plugin.ExpandRequest, _ int) (*plugin.ExpandResponse, error) {
					<-ctx.Done()
					return nil, ctx.Err()
				}
			case "close":
				failed.closeError = errors.New("session close failed")
			case "validation":
				failed.expand = func(context.Context, *plugin.ExpandRequest, int) (*plugin.ExpandResponse, error) {
					return expansion("collision", root.ID, root), nil
				}
			case "iteration-limit":
				failed.expand = func(_ context.Context, _ *plugin.ExpandRequest, call int) (*plugin.ExpandResponse, error) {
					return expansion("child", root.ID, resource(t, "child", fmt.Sprint(call))), nil
				}
			}
			result, err = h.Render(ctx, plugin.OpenRequest{})
			if err == nil {
				t.Fatal("operation failure accepted")
			}
			if phase == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline lost: %v", err)
			}
			incomplete(t, result)
			if oldFlux.stops != 1 || failed.stops != 1 {
				t.Fatal("failed operation did not retire all instances")
			}
			for _, processCtx := range processContexts {
				if processCtx.Err() == nil {
					t.Fatal("retired process context still alive")
				}
			}
			if len(oldFlux.closed) != 2 || len(failed.closed) != 2 {
				t.Fatal("failed session cleanup skipped")
			}
			if len(instances["flux"]) != 1 || len(instances["z-other"]) != 1 {
				t.Fatal("retried within failed operation")
			}
			if phase == "crash" || phase == "timeout" {
				if len(failed.inputs) != beforeCalls+1 {
					t.Fatal("retried uncertain expand RPC")
				}
			}

			// A new invocation has a fresh deadline and new engine instances.
			result, err = h.Render(t.Context(), plugin.OpenRequest{})
			complete(t, result, err)
			if len(result.Resources) != 1 || result.Resources[0].ID != root.ID {
				t.Fatal("restarted render lost root")
			}
			for _, name := range []string{"flux", "z-other"} {
				if len(instances[name]) != 2 {
					t.Fatalf("%s was not restarted", name)
				}
				fresh := instances[name][1]
				if fresh.describes != 1 || len(fresh.opens) != 1 || len(fresh.closed) != 1 {
					t.Fatalf("%s fresh instance lifecycle: %+v", name, fresh)
				}
			}
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
			if oldFlux.stops != 1 || failed.stops != 1 {
				t.Fatal("retired instances closed twice")
			}
		})
	}
}

func TestHostStableDiagnosticErrorKeepsHealthyProcess(t *testing.T) {
	t.Parallel()
	response := expansion("root", "root", resource(t, "root", "ok"))
	response.Diagnostics = []plugin.Diagnostic{{Code: "pending", Severity: "error", Message: "unresolved input"}}
	engine := fixed("flux", response)
	h := host(t, engine)
	for range 2 {
		result, err := h.Render(t.Context(), plugin.OpenRequest{})
		if err != nil {
			t.Fatal(err)
		}
		incomplete(t, result)
	}
	if engine.starts != 1 || engine.stops != 0 || len(engine.closed) != 2 {
		t.Fatalf("stable diagnostics retired a healthy process: %+v", engine)
	}
}

func TestHostVersionIndependentConcreteCollisions(t *testing.T) {
	t.Parallel()
	parse := func(apiVersion, kind, namespace, name string) plugin.Resource {
		r, err := plugin.ParseResource([]byte(fmt.Sprintf(
			"apiVersion: %s\nkind: %s\nmetadata:\n  namespace: %s\n  name: %s\n",
			apiVersion, kind, namespace, name)), plugin.Provenance{})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	one := parse("apps/v1", "Deployment", "default", "workload")
	two := parse("apps/v1beta1", "Deployment", "default", "workload")
	if one.ID == two.ID {
		t.Fatal("fixture does not have distinct versioned wire identities")
	}
	for _, placement := range []string{"same expansion", "same engine", "different engines"} {
		t.Run(placement, func(t *testing.T) {
			t.Parallel()
			flux := fixed("flux", expansion("root", "root", one))
			engines := []*fakeEngine{flux}
			switch placement {
			case "same expansion":
				flux = fixed("flux", expansion("root", "root", one, two))
				engines[0] = flux
			case "same engine":
				flux = fixed("flux", &plugin.ExpandResponse{Expansions: []plugin.Expansion{
					{ID: "root", Trigger: "root", Resources: []plugin.Resource{one}},
					{ID: "child", Trigger: one.ID, Resources: []plugin.Resource{two}},
				}})
				engines[0] = flux
			case "different engines":
				engines = append(engines, fixed("crossplane", expansion("child", one.ID, two)))
			}
			result, err := host(t, engines...).Render(t.Context(), plugin.OpenRequest{})
			if err == nil || !strings.Contains(err.Error(), "duplicate concrete resource identity") {
				t.Fatalf("cross-version collision accepted: %v", err)
			}
			incomplete(t, result)
		})
	}
	for _, test := range []struct {
		name  string
		other plugin.Resource
	}{
		{name: "different group", other: parse("other/v1", "Deployment", "default", "workload")},
		{name: "different kind", other: parse("apps/v1", "StatefulSet", "default", "workload")},
		{name: "different namespace", other: parse("apps/v1", "Deployment", "other", "workload")},
		{name: "different name", other: parse("apps/v1", "Deployment", "default", "other")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result, err := host(t, fixed("flux", expansion("root", "root", one, test.other))).Render(t.Context(), plugin.OpenRequest{})
			complete(t, result, err)
			if len(result.Resources) != 2 {
				t.Fatal("distinct concrete identities collapsed")
			}
		})
	}
}
