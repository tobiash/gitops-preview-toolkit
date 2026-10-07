# Live plugin verification

From the repository root on Linux amd64, run:

```sh
bash tests/plugins/run-live.sh
```

Prerequisites: Go matching `go.mod`, curl, sha256sum, network access for the
official release and Go modules, and an existing writable `/tmp/opencode`.
Docker, Podman, and Kubernetes are not required.

The script downloads the official Crossplane **v2.4.2** core engine, verifies
its published SHA-256 and version, builds upstream
`function-go-templating@v0.12.0`, and builds the core CLI and both sibling plugin
executables. A temporary
module file isolates dependency resolution from concurrent repository work.
Build output, the Go build cache, and runtime temporary files are under
`/tmp/opencode`; bundle and runtime cleanup runs on exit. The reusable Go build
cache remains at `/tmp/opencode/plugins-go-build-cache`.

The integration-tagged test starts the upstream function on an ephemeral
loopback port with `--insecure`, then uses `pluginhost.New` to launch the actual
persistent Flux and Crossplane gRPC subprocesses. Trusted configuration selects
the engine binary and Development function target. Function and Composition
selection is derived from inventory, with only `manifests/` as a source root.
The vendored Git chart is resolved locally with `resolveGit: true`.

The fixture exercises XRD + Composition + Function + parent XR → named
HelmRelease → local Helm chart → leaf XR → named and logical ConfigMaps.
Assertions cover XRD defaults, empty observed composed-resource state, unchanged
repeat output, same-cardinality value changes, base/head isolation, fresh local
edits, transitive removal, and empty Kubernetes names on logical outputs.
Two independent subtests exercise a named-only chain and the additional logical
output, so a logical identity failure does not hide verification of the named
chain's value propagation and cleanup.

`TestLiveCLI` runs the actual installed `gitops-preview` binary, relying on
sibling plugin discovery. It verifies JSON render, `diff path:<before>
path:<after> --output json`, and `diff --html --html-open=false`. Two same-kind
unnamed resources have distinct logical keys in JSON and embedded HTML data,
with no invented `metadata.name`. It also tests `.gitops-preview.yaml` in a raw
manifest root and a refused function endpoint: a nonzero exit emits exactly one
incomplete JSON document with no partial inventory. `/proc` checks ensure each
CLI command releases its plugin and engine subprocesses.

The equivalent trusted CLI invocation is:

```sh
gitops-preview render <fixture-root> --crossplane \
  --crossplane-engine <released-core-binary> \
  --crossplane-function templating=127.0.0.1:<port> --output json
```

For prebuilt binaries, supply these variables:

```sh
CROSSPLANE_TEST_ENGINE=/tmp/opencode/bin/crossplane \
CROSSPLANE_TEST_TEMPLATING_BINARY=/tmp/opencode/bin/function-go-templating \
PLUGIN_TEST_FLUX_BINARY=/tmp/opencode/bin/gitops-preview-flux \
PLUGIN_TEST_CROSSPLANE_BINARY=/tmp/opencode/bin/gitops-preview-crossplane \
PLUGIN_TEST_CORE_BINARY=/tmp/opencode/bin/gitops-preview \
go test -tags=integration ./tests/plugins -run 'TestLive(PluginChain|CLI)$' -count=1 -v
```

Without those variables the test skips. CI can run the script to enforce actual
engine verification rather than a skipped or substituted-engine test.

## Real default Docker runtime through rootless Podman

```sh
PLUGIN_TEST_DOCKER=1 bash tests/plugins/run-live.sh
```

This additionally starts an owned `podman system service --time=0` on a Unix
socket inside the temporary bundle, confirms Docker API `_ping`, and pulls the
real `ghcr.io/crossplane-contrib/function-go-templating:v0.12.0` image anonymously.
`TestLiveDockerRuntime` omits Runtime and DevelopmentTargets configuration,
exercises the production Docker default, and independently checks container
cleanup through the Docker-compatible API. Its CLI subtest renders with
`--crossplane --crossplane-engine` and no development target. The service stops
on exit; user containers and images are not deleted. The downloaded upstream
image remains cached. Without the opt-in flag this Docker test explicitly skips.

## Packaging verification

```sh
bash tests/plugins/check-packaging.sh
bash tests/plugins/check-action-image.sh
```

The first script uses `go list -deps` to assert both core entrypoints exclude
Flux, Helm, Crossplane render engines and in-process expanders. It cross-builds
all four executables (`gitops-preview`, `fmp`, and both plugins) with
`CGO_ENABLED=0` for Linux/Darwin × amd64/arm64: 16 builds. It checksum-verifies
official GoReleaser v2.18.2 and runs `goreleaser check` against the repository
configuration. Per-target temporary caches bound `/tmp` usage. Prerequisites
add `file` and `tar`; several GB of temporary disk space are needed.

The second script builds the actual `Dockerfile.action` with rootless Podman,
checks that all four binaries are installed, and runs a real JSON render with
the shipped core and sibling Flux plugin inside the image. It uses a unique
image tag and container label and cleans up only those owned artifacts. Base
images pulled by Podman remain cached. Python 3 validates the render output.
Neither script creates tags, publishes releases, or changes packaging config.

## Performance measurements

Benchmarks separate persistent gRPC transport, host convergence, local Flux
rendering, process startup, and full CLI output. Each inventory contains 100 or
1,000 ConfigMaps with a 1 KiB payload; the host fixture verifies three sweeps.

```sh
go test -tags=integration ./tests/plugins -run '^$' \
  -bench 'Benchmark(PersistentRPC|HostSweeps|FluxLocal)' \
  -benchmem -benchtime=300ms -count=10
```

Development measurements on Linux/amd64, Go 1.26.4, Intel Core Ultra 7 270K Plus
(medians of ten repetitions):

| Operation | 100 resources | 1,000 resources |
| --- | ---: | ---: |
| Persistent gRPC inventory round-trip | 3.16 ms | 29.14 ms |
| Three cached host sweeps, in-process | 20.41 ms | 195.4 ms |
| Three cached host sweeps, gRPC | 28.62 ms | 252.6 ms |
| Local Flux render, in-process | 57.17 ms | 1.332 s |
| Local Flux render, persistent gRPC | 63.15 ms | 1.371 s |
| Local Flux render, new process per operation | 100.8 ms | 1.446 s |

IPC is measurable, particularly when rendering itself is cheap. These are not
comparisons against the old product. The large-inventory in-process and gRPC
render confidence intervals overlap. Allocation figures measure the benchmark
parent only, not total plugin-process memory. Re-run on the target environment
before making performance or capacity decisions.
