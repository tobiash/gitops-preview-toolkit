# ADR 0007: Persistent plugins own engine rendering

## Status

Accepted

## Context

Preview currently constructs Flux and Helm expanders and builds paths in the
same package that runs comparison, policy, AI Assessment, and report workflows.
Crossplane introduces unnamed desired resources and function evaluation evidence
that cannot safely use a Kubernetes-name-indexed ResMap.

## Decision

`pkg/preview` retains its render, test, list, normalization, and `RunDiff` APIs.
It delegates engine rendering to `pkg/pluginhost` using the neutral `pkg/plugin`
contract. The default executable is `gitops-preview-flux`; trusted invocation
options may select commands or enable `gitops-preview-crossplane`.

Flux/Git/Helm/path building belongs to the combined Flux plugin. `WithFluxKS`,
`WithGitRepo`, and `WithHelm(*config.HelmSettings)` configure neutral flags, not
in-process SDK objects. Source alias capture belongs to `pkg/sourcealiases` so
diff source planning and agent source capture do not import engine packages.

`render.NewDefaultRender` constructs a passive resource collection. Filesystem
loading, recursive build boundaries and Kustomize execution belong to
`pkg/build.Builder`, which embeds that collection. The Flux graph constructs a
builder and returns its collection to the existing expansion workflow. Building
methods are not retained on `render.Render`: a forwarding compatibility method
would reintroduce the builder dependency into every host workflow.

`Render.SetProvenance` stores exact structured origin metadata separately from
YAML. Existing passive path/reference validators remain available to engine
callers; they do not construct a Kustomizer. A runtime dependency regression test
checks render, Preview and agent packages for Helm, Flux, Kustomizer and engine
implementation imports.

Plugin processes may persist across operations, but each source/cluster render
uses a fresh evaluation. `Preview.Close` releases an owned host; agent/MCP
services lend their persistent host to per-operation previews and release it on
service shutdown. Startup configuration
selects executables; manifest metadata cannot grant execution capabilities.
Local-only and strict-input policy are propagated independently of engine options.

Named desired resources project into the existing render collection. Unnamed
composed resources retain stable logical identities in a snapshot sidecar, with
no invented live names. Evaluation evidence, including composite status, stays
outside resource inventory. Logical comparison, agent artifacts, classifications,
AI Assessment inputs and report projections carry those identities end to end.
Unnamed outputs remain terminal; only concrete names can activate downstream
engine expansion or satisfy named dependency lookups.

The host owns bounded replacement/discovery across engines. This is a desired
preview, not a generic controller or a promise of arbitrary cyclic reconciliation.
Unsupported or non-convergent dependencies make the render incomplete; incomplete
snapshots cannot produce authoritative additions/deletions, policy or AI results.

## Consequences

This supersedes ADR 0004's placement of Flux orchestration inside Preview, while
preserving its intent that Flux orchestration is one cohesive module. ADRs 0001,
0002, 0003 and 0005 retain their workflow and provenance contracts.

The Helm-typed Go option changes to neutral configuration. Distribution includes
plugin siblings. Tests run real plugin executables rather than an in-process
fallback. Host runtime dependency graphs must exclude Helm, Flux SDKs and engine
expander packages, including transitive imports through source planning.
