# Crossplane render plugin

This package implements `plugin.Service`; `cmd/gitops-preview-crossplane` serves
it over the shared Unix-socket transport:

```sh
gitops-preview-crossplane --socket /absolute/path/crossplane.sock
```

## Dependencies

The root module pins `github.com/crossplane/cli/v2@v2.5.0`.
The plugin imports its exported render helpers and XRD defaulting,
the separately versioned Crossplane API/runtime packages, function SDK protobuf,
gRPC, and Kubernetes schema APIs. It does not import the controller module or
implement a replacement composition engine.

Install the **controller** binary `v2.4.2`, not the similarly named user-facing
CLI, as `crossplane-core` on PATH or configure its absolute path. The plugin
checks `--version` before first execution and does not download binaries.

Controller artifact for Linux/amd64:

<https://releases.crossplane.io/stable/v2.4.2/bin/linux_amd64/crossplane>

Sources:

- [Official CLI render helpers v2.5.0](https://github.com/crossplane/cli/tree/v2.5.0/cmd/crossplane/render)
- [Controller render protocol v2.4.2](https://github.com/crossplane/crossplane/blob/v2.4.2/proto/render/v1alpha1/render.proto)
- [CLI documentation](https://docs.crossplane.io/cli/v2.5/command-reference/)

## Trusted OpenRender configuration

`OpenRequest.Config` is a JSON object, supplied by the trusted plugin invocation
configuration. Unknown fields and invalid values are rejected.

```json
{
  "engineBinary": "/absolute/path/crossplane-core-v2.4.2",
  "runtime": "Docker",
  "developmentTargets": {
    "function-go-templating": "127.0.0.1:9443",
    "function-patch-and-transform": "127.0.0.1:9444"
  },
  "dockerEnv": {},
  "timeout": "1m",
  "maxFunctions": 32
}
```

- `engineBinary`: defaults to `crossplane-core`, resolved on PATH.
- `runtime`: `Docker` (default) or `Development`.
- `developmentTargets`: explicit trusted host:port grants, by Function name.
  A grant selects Development for that Function even when the default is Docker.
  Development has no implicit localhost fallback: an absent grant is an error.
- `dockerEnv`: trusted function-container environment. Commas/newlines are
  rejected because the upstream annotation uses comma-separated values.
- `timeout`: timeout of the entire sweep, positive and at most `10m`;
  defaults to `1m`. Cancellation also reaches engine subprocesses and gRPC.
- `maxFunctions`: persistent runtime bound, defaults to 32, range 1–256.

All manifest `render.crossplane.io/*` annotations are removed before applying
trusted settings. Docker connection/registry settings come from the process's
trusted environment; Function packages are resolved from inventory, not from
network discovery or a Crossplane project file.

`localOnly` is rejected, including with Development: a local controller and
loopback endpoint are not a sandbox for function filesystem or network access.
`StrictInputs` does not suppress diagnostics. Neither flag grants execution.

Each session has its own runtime handles and per-XR evaluation cache. `Fresh`
Docker sessions use unique plugin-owned container identities. `Fresh` is rejected
with `fresh-development-unsupported` whenever the trusted configuration selects
Development or grants development targets: externally owned fixture processes
cannot guarantee independent permadiff state. Ordinary Development sessions
(`Fresh: false`) are supported; their processes remain caller-owned.

An evaluation is reused while its direct inputs (XR, selected XRD, Composition,
effective Functions, credentials, and engine configuration) and requested
resource/schema witnesses remain identical. The inventory itself is not hashed:
unrelated generated Flux objects do not rerun random templates. Witnesses record
matching content, selector namespace/name/labels, and zero matches; requested
schemas include their referenced components but not unrelated schemas in the
same group-version document. Both successful and pipeline-FATAL evaluations can
be reused, with current evidence emitted once per sweep. Missing selection
dependencies are resolved anew, and transport/startup failures are not cached.
Independent sessions recompute. Absent producers' cache entries are pruned.

Obsolete runtimes are evicted when the bound would be exceeded. Session/service
close cancels active sweeps and stops Docker runtimes with a separate bounded
cleanup context. Failed cleanup retains its runtime handle and closed session
for an idempotent later `CloseRender` or `Close` retry. Pending sessions count
toward the session bound. Partially created Docker containers use known trusted
names and retain retry handles when removal fails.

## Discovery and desired outputs

Every sweep discovers XRs from XRD group/kind/served versions. XRD v1 defaults
to `LegacyCluster`; v2 defaults to `Namespaced`. Modern controls are read from
`spec.crossplane`; legacy controls from `spec`.

Selection precedence is enforced Composition, explicit XR reference, explicit
selector, XRD default, then a unique compatible Composition. Ambiguities are
errors. Pinned CompositionRevision payloads can be used without a current base
Composition, provided their type and parent Composition provenance are present
in inventory. Missing pinned revisions and revision selectors are diagnosed.
Legacy claim-to-XR reconciliation is explicitly unsupported.

XRD structural defaults and derived OpenAPI documents are supplied to the
engine. Function credentials are resolved from inventory Secrets; `stringData`
is converted to `data` because there is no Kubernetes admission server.
Current named inventory is supplied for required-resource lookup. The primary
XR is excluded from this list so an old copy cannot overwrite its defaults.

Observed composed state is **always empty**. Previous desired output is never
passed as observed cluster state. The external engine executes the real
Crossplane reconciler against its upstream in-memory client; provider/Flux
controllers and Kubernetes admission do not execute inside that engine.

There is one complete replacement Expansion per discovered XR, with both ID
and Trigger set to the inventory XR ID. Failed producers have empty desired
outputs and current diagnostics, so prior success cannot persist after failure.
Status, events, context, requirements, and deletion records are Evidence only.
Partial FATAL responses preserve Evidence but never publish successful output.

A transparent local gRPC proxy records the **actual function response** before
the controller allocates names. Explicit function names remain Kubernetes
identities (including Flux/Helm triggers). Unnamed outputs lose only their
engine-generated `metadata.name`, retain `generateName`, and have a stable
producer-relative logical ID based on `crossplane.io/composition-resource-name`.
The plugin never guesses anonymity from a suffix or invents a cluster identity.
Engine-generated ownership/version metadata and output status are not published
in desired inventory. Namespace behavior remains the controller's behavior.

## Tests

```sh
go test ./pkg/crossplanerender ./cmd/gitops-preview-crossplane
go test -race ./pkg/crossplanerender
```

The real live test accepts an already running upstream function and the actual
controller binary:

```sh
go install github.com/crossplane-contrib/function-go-templating@v0.12.0
function-go-templating --insecure --address=127.0.0.1:9443
```

In a separate terminal:

```sh
CROSSPLANE_TEST_ENGINE=/absolute/path/crossplane-core-v2.4.2 \
CROSSPLANE_TEST_TEMPLATING_TARGET=127.0.0.1:9443 \
go test -tags=integration ./pkg/crossplanerender -run TestLiveTemplating -v
```

The live test opens an ordinary (`Fresh: false`) Development session and verifies
real function execution, defaults, explicitly named versus unnamed outputs,
cached random templates across repeat sweeps, and desired outputs not being
observed. It also exercises real bootstrap requirements: an unchanged zero-match
FATAL remains stable, then reruns when the requested namespaced resource appears
or changes content.
Additional function-protocol tests can run
`function-patch-and-transform@v0.10.0 --insecure --address=127.0.0.1:9444`
with a trusted target for a patch/transform pipeline. These fixture versions
build with Go 1.26.4 without requiring the latest functions' Go 1.26.8 toolchain.
