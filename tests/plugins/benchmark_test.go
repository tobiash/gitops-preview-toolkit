//go:build integration

package plugins_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/fluxrender"
	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	"github.com/tobiash/gitops-preview-toolkit/pkg/pluginhost"
	"gopkg.in/yaml.v3"
)

// Run with GOFLAGS= go test -tags=integration ./tests/plugins -run '^$'
// -bench 'Benchmark(PersistentRPC|HostSweeps|FluxLocal)' -benchmem
// -benchtime=300ms -count=10. These are current-architecture comparisons,
// not a baseline for the removed renderer. B/op measures only the parent process.
// Prebuilt binaries can be supplied through PLUGIN_BENCH_FLUX_BINARY and
// PLUGIN_BENCH_CLI_BINARY to reuse the same artifacts across repeated runs.

// TestBenchmarkPluginProcess turns the test executable into a real gRPC plugin.
// The marker is an argument, so ordinary test runs never start a server.
func TestBenchmarkPluginProcess(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "benchmark-plugin" {
			continue
		}
		if len(os.Args) != i+5 || os.Args[i+3] != "--socket" {
			os.Exit(90)
		}
		resources := benchmarkReadInventory(t, os.Args[i+2])
		service := &benchmarkService{resources: resources, echo: os.Args[i+1] == "echo"}
		if err := plugin.Serve(context.Background(), os.Args[i+4], service); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(91)
		}
		os.Exit(0)
	}
}

type benchmarkService struct {
	resources []plugin.Resource
	echo      bool
	sweeps    int
}

func (s *benchmarkService) Describe(context.Context, *plugin.DescribeRequest) (*plugin.DescribeResponse, error) {
	return &plugin.DescribeResponse{Name: "flux", ProtocolVersion: plugin.ProtocolVersion}, nil
}

func (s *benchmarkService) OpenRender(context.Context, *plugin.OpenRequest) (*plugin.OpenResponse, error) {
	s.sweeps = 0
	return &plugin.OpenResponse{Session: "benchmark"}, nil
}

func (s *benchmarkService) Expand(_ context.Context, req *plugin.ExpandRequest) (*plugin.ExpandResponse, error) {
	s.sweeps++
	resources := s.resources
	if s.echo {
		resources = req.Resources
	} else if s.sweeps == 1 {
		resources = resources[:len(resources)/2]
	}
	// A half inventory, then a full inventory, then its confirmation: three
	// sweeps. Cached output still crosses the real transport on every sweep.
	return &plugin.ExpandResponse{
		Expansions: []plugin.Expansion{{ID: "fixture", Trigger: "root", Resources: resources}},
		Evidence:   []json.RawMessage{json.RawMessage(fmt.Sprint(s.sweeps))},
	}, nil
}

func (*benchmarkService) CloseRender(context.Context, *plugin.CloseRequest) (*plugin.CloseResponse, error) {
	return &plugin.CloseResponse{}, nil
}

func benchmarkFixture(tb testing.TB, count int) string {
	tb.Helper()
	root := tb.TempDir()
	for i := range count {
		data := fmt.Sprintf("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: fixture-%04d\n  namespace: default\ndata:\n  payload: %s\n", i, strings.Repeat("x", 1024))
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("%04d.yaml", i)), []byte(data), 0o600); err != nil {
			tb.Fatal(err)
		}
	}
	return root
}

func benchmarkReadInventory(tb testing.TB, root string) []plugin.Resource {
	tb.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		tb.Fatal(err)
	}
	resources := make([]plugin.Resource, 0, len(entries))
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			tb.Fatal(err)
		}
		resource, err := plugin.ParseResource(data, plugin.Provenance{Path: entry.Name()})
		if err != nil {
			tb.Fatal(err)
		}
		resources = append(resources, resource)
	}
	return resources
}

func benchmarkCommand(tb testing.TB, root, mode string) plugin.Command {
	tb.Helper()
	executable, err := os.Executable()
	if err != nil {
		tb.Fatal(err)
	}
	return plugin.Command{Name: "flux", Command: executable, Args: []string{"-test.run=^TestBenchmarkPluginProcess$", "--", "benchmark-plugin", mode, root}}
}

func BenchmarkPersistentRPC(b *testing.B) {
	for _, count := range []int{100, 1000} {
		b.Run(fmt.Sprintf("resources=%d", count), func(b *testing.B) {
			root := benchmarkFixture(b, count)
			resources := benchmarkReadInventory(b, root)
			client, err := plugin.Start(b.Context(), benchmarkCommand(b, root, "echo"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { benchmarkClose(b, client.Close) })
			opened, err := client.OpenRender(b.Context(), &plugin.OpenRequest{})
			if err != nil {
				b.Fatal(err)
			}
			request := &plugin.ExpandRequest{Session: opened.Session, Resources: resources}
			var response *plugin.ExpandResponse
			b.ReportAllocs()
			for b.Loop() {
				response, err = client.Expand(b.Context(), request)
				if err != nil {
					b.Fatal(err)
				}
			}
			if len(response.Expansions) != 1 || !reflect.DeepEqual(response.Expansions[0].Resources, resources) {
				b.Fatal("RPC did not round-trip the full inventory")
			}
			if _, err := client.CloseRender(b.Context(), &plugin.CloseRequest{Session: opened.Session}); err != nil {
				b.Fatal(err)
			}
			wire, err := json.Marshal(request)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportMetric(float64(len(wire)), "request-JSON-B")
		})
	}
}

func benchmarkClose(tb testing.TB, close func() error) {
	tb.Helper()
	if err := close(); err != nil {
		tb.Error(err)
	}
}

func benchmarkRender(tb testing.TB, host *pluginhost.Host, request plugin.OpenRequest, count int) *pluginhost.Result {
	tb.Helper()
	result, err := host.Render(tb.Context(), request)
	if err != nil {
		tb.Fatal(err)
	}
	if !result.Complete || len(result.Resources) != count || len(result.Diagnostics) != 0 {
		tb.Fatalf("incomplete inventory: complete=%v resources=%d diagnostics=%v", result.Complete, len(result.Resources), result.Diagnostics)
	}
	return result
}

func BenchmarkHostSweeps(b *testing.B) {
	for _, count := range []int{100, 1000} {
		b.Run(fmt.Sprintf("resources=%d", count), func(b *testing.B) {
			root := benchmarkFixture(b, count)
			resources := benchmarkReadInventory(b, root)
			for _, transport := range []string{"in-process", "persistent-grpc"} {
				b.Run(transport, func(b *testing.B) {
					options := pluginhost.Options{MaxIterations: 3}
					if transport == "in-process" {
						options.Start = func(context.Context, plugin.Command) (plugin.Service, func() error, error) {
							return &benchmarkService{resources: resources}, nil, nil
						}
					}
					host, err := pluginhost.NewWithOptions([]plugin.Command{benchmarkCommand(b, root, "sweep")}, options)
					if err != nil {
						b.Fatal(err)
					}
					b.Cleanup(func() { benchmarkClose(b, host.Close) })
					benchmarkRender(b, host, plugin.OpenRequest{}, count) // startup excluded
					var result *pluginhost.Result
					b.ReportAllocs()
					for b.Loop() {
						result = benchmarkRender(b, host, plugin.OpenRequest{}, count)
					}
					if !reflect.DeepEqual(result.Resources, resources) || len(result.Evidence) != 1 || string(result.Evidence[0]) != "3" {
						b.Fatal("expected identical inventory after exactly three sweeps")
					}
					b.ReportMetric(3, "sweeps/op")
				})
			}
		})
	}
}

func benchmarkBinary(b *testing.B, name, environment string) string {
	b.Helper()
	if path := os.Getenv(environment); path != "" {
		return path
	}
	path := filepath.Join(b.TempDir(), name)
	command := exec.CommandContext(b.Context(), "go", "build", "-buildvcs=false", "-o", path, "./cmd/"+name)
	command.Dir = "../.."
	command.Env = append(os.Environ(), "GOFLAGS=")
	if output, err := command.CombinedOutput(); err != nil {
		b.Fatalf("build %s: %v\n%s", name, err, output)
	}
	return path
}

func BenchmarkFluxLocal(b *testing.B) {
	binary := benchmarkBinary(b, "gitops-preview-flux", "PLUGIN_BENCH_FLUX_BINARY")
	cliBinary := benchmarkBinary(b, "gitops-preview", "PLUGIN_BENCH_CLI_BINARY")
	for _, count := range []int{100, 1000} {
		b.Run(fmt.Sprintf("resources=%d", count), func(b *testing.B) {
			root := benchmarkFixture(b, count)
			request := plugin.OpenRequest{Root: root, Paths: []string{"."}, LocalOnly: true, RunID: "benchmark"}
			var expected []plugin.Resource
			for _, mode := range []string{"in-process", "persistent-grpc", "cold-grpc"} {
				b.Run(mode, func(b *testing.B) {
					command := plugin.Command{Name: "flux", Command: binary}
					options := pluginhost.Options{}
					if mode == "in-process" {
						options.Start = func(context.Context, plugin.Command) (plugin.Service, func() error, error) {
							service, err := fluxrender.New(logr.Discard())
							if err != nil {
								return nil, nil, err
							}
							return service, service.Close, nil
						}
					}
					newHost := func() *pluginhost.Host {
						host, err := pluginhost.NewWithOptions([]plugin.Command{command}, options)
						if err != nil {
							b.Fatal(err)
						}
						return host
					}
					host := newHost()
					warm := benchmarkRender(b, host, request, count)
					if expected == nil {
						expected = warm.Resources
					} else if !reflect.DeepEqual(expected, warm.Resources) {
						b.Fatal("in-process and subprocess Flux outputs differ")
					}
					if mode == "cold-grpc" {
						benchmarkClose(b, host.Close)
					} else {
						b.Cleanup(func() { benchmarkClose(b, host.Close) })
					}
					var result *pluginhost.Result
					b.ReportAllocs()
					for b.Loop() {
						if mode == "cold-grpc" {
							host = newHost()
						}
						result = benchmarkRender(b, host, request, count)
						if mode == "cold-grpc" {
							benchmarkClose(b, host.Close)
						}
					}
					if !reflect.DeepEqual(expected, result.Resources) {
						b.Fatal("repeated Flux inventory changed")
					}
				})
			}
			b.Run("cold-cli", func(b *testing.B) {
				if expected == nil {
					// Keep this sub-benchmark independently runnable with -bench.
					expected = benchmarkReadInventory(b, root)
				}
				var output []byte
				b.ReportAllocs()
				for b.Loop() {
					command := exec.CommandContext(b.Context(), cliBinary, "render", root, "--path=.", "--flux-plugin="+binary, "--render-helm=false", "--quiet")
					var err error
					output, err = command.CombinedOutput()
					if err != nil {
						b.Fatalf("CLI render: %v\n%s", err, output)
					}
				}
				// CLI adds output formatting and process startup to the host path.
				// Decode outside timing and require the same Kubernetes objects.
				want := map[string]map[string]any{}
				for _, resource := range expected {
					object, err := plugin.Object(resource)
					if err != nil {
						b.Fatal(err)
					}
					want[resource.ID] = object
				}
				got := map[string]map[string]any{}
				decoder := yaml.NewDecoder(strings.NewReader(string(output)))
				for {
					var object map[string]any
					if err := decoder.Decode(&object); err == io.EOF {
						break
					} else if err != nil {
						b.Fatal(err)
					}
					id, err := plugin.ObjectID(object)
					if err != nil {
						b.Fatal(err)
					}
					if got[id] != nil {
						b.Fatalf("duplicate CLI object %s", id)
					}
					got[id] = object
				}
				if !reflect.DeepEqual(got, want) {
					b.Fatal("CLI and service inventories differ")
				}
			})
		})
	}
}
