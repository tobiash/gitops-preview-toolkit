# Agent Workflow and MCP

The agent interface shares one operation service between a versioned JSON CLI
and a local stdio MCP server. It reuses fmp rendering, provenance and policies,
and k8q resource matching, comparison and accounting. It does not apply resources,
publish reports, decrypt SOPS content, or invoke a second AI model.

## Entry Points

```sh
fmp agent schema
fmp agent discover --root /absolute/path/to/gitops
fmp agent --root /absolute/path/to/gitops < request.json
fmp mcp --root /absolute/path/to/gitops
```

`agent` reads exactly one JSON document. Pass a single request or a sequential
batch. `mcp` reserves stdout exclusively for the protocol. Do not combine either
entry point with legacy flags such as `--output`; configuration and render roots
are supplied through the operation request and the captured `.fmp.yaml`.

The portable workflow skill, host examples and executable local-chart fixture
live in [gitops-preview-skill](https://github.com/tobiash/gitops-preview-skill).
Its installation instructions pin a compatible implementation rather than
assuming that an older published `fmp` binary supports these commands.

## Operations

MCP uses the names below, without an `operation` argument. CLI requests include
the corresponding operation name, for example `"operation": "preview"`.

| MCP tool | Operation | Result |
| --- | --- | --- |
| `manifest_discover` | `discover` | Configured roots, effective profile, supported operations and limits |
| `manifest_render` | `render` | A retained snapshot handle and resource count |
| `manifest_preview` | `preview` | Change-set, before/after handles, summary and change indicator |
| `manifest_query` | `query` | A bounded page of identities and producer origins |
| `manifest_inspect` | `inspect` | Redacted resource details or selected JSON-pointer projections |
| `manifest_compare` | `compare` | Comparison of existing snapshots without rerendering |
| `manifest_check` | `check` | Snapshot budgets or configured deterministic change policies |
| `manifest_release` | `release` | Release one retained handle |

Sources are `worktree`, `git:<ref>`, or `path:<relative-directory>`. The startup
workspace is the boundary for source selection. Git refs resolve to a commit
before materialization. Path/worktree inputs are copied into private temporary
directories before rendering. Those directories are removed after execution;
retained artifacts are in memory, not a persistent disk cache.

`render` defaults to `worktree`. `preview` defaults to `git:HEAD` versus `worktree`.
Paths are relative to each source root and may include `cluster:path` prefixes.
Explicit request paths override configured roots. Preview uses the captured base
configuration for both sides and retains its policy configuration. Compare uses
the before snapshot's policy configuration.

Capture rejects symlinks and nonregular files, and excludes `.git`, `node_modules`,
`vendor`, `.venv`, `venv`, `.terraform`, and `.cache`, reporting exclusions. Keep
vendored Helm charts in a nonexcluded directory such as `charts/`. Capture is not
an atomic filesystem-wide transaction; avoid editing inputs during capture.

## Result Contract

```json
{
  "schemaVersion": "1",
  "operation": "preview",
  "status": "success",
  "complete": true,
  "data": {
    "id": "opaque-change-handle",
    "beforeId": "opaque-before-handle",
    "afterId": "opaque-after-handle",
    "summary": {"added": 0, "modified": 1, "deleted": 0, "total": 1},
    "changed": true
  },
  "diagnostics": []
}
```

Check both `status` and `complete` before treating results as authoritative.
Incomplete rendering publishes no authoritative handles or changes. Never
interpret `Incomplete`, cancellation, capacity failure or an expired handle as
an empty successful result. Error messages are deliberately generic because
underlying renderer errors can contain sensitive values.

Checks have a separate `data.verdict`, either `passed` or `failed`. A correctly
executed policy/budget check with a failed verdict has `status: "success"`,
`complete: true`, and MCP `isError: false`. The JSON CLI exits 0 for successful
operations, including failed check verdicts, and 2 for operational/input failures.
CI callers must explicitly inspect the verdict if they want a policy gate.

`fmp agent schema` publishes operation-specific request and response schemas,
including supported enums, required fields, result DTOs and pagination limits.
MCP advertises the same per-operation contracts. Existing legacy CLI JSON formats
are unchanged.

## Query and Inspect

Query filters combine with AND: `cluster`, `kind`, `name`, `namespace`, `group`,
`labels`, `producer`, and `action`. Labels use Kubernetes selectors; group uses
k8q's substring matching. An empty namespace/cluster filter means no restriction.
Actions are `added`, `modified`, and `deleted`.

Query returns resource IDs owned by the requested handle. Inspect uses that
handle plus `resourceId`. Optional `fields` are JSON pointers; with projections,
`old` and `new` contain pointer-to-value maps. Redaction happens before projection.
Provenance identifies contributors; it is not a general rendered-field-to-source
editing map. Edit source files using the host agent's normal editor, then rerender.

Snapshot checks support `maxCPURequests`, `maxMemRequests`, `maxCPULimits`,
`maxMemLimits`, `requireRequests`, and `requireLimits`. Counts and budgets can be
filtered. Resource totals are manifest estimates, not scheduler-capacity modeling.
Diff checks evaluate captured policy configuration and reject budget/filter
arguments. They expose verdicts and finding counts, not raw policy messages.

## CLI Batches

Standalone CLI invocations cannot share in-memory handles. A batch reuses one
service and one captured preview, while MCP retains handles across tool calls.

```json
{
  "requests": [
    {"operation": "preview", "paths": ["clusters/development"]},
    {"operation": "query", "id": "@0.id", "query": {"kind": "Deployment"}},
    {"operation": "check", "id": "@0.afterId", "maxCPURequests": "8"}
  ]
}
```

References are resolved only in `id`, `beforeId`, `afterId`, and `resourceId`.
`@0.id` addresses the first response's `data.id`; `@1.items.0.resourceId` addresses
a prior query's first item. Only earlier, string-valued fields are valid. Do not
assume a query has an item unless the selected fixture/workflow guarantees it.

Batches contain 1-32 requests and stop on operational failure. The outer response
contains `schemaVersion`, `status`, `complete`, `results`, and optional
`failedIndex`/`error`. A failed check verdict does not stop the batch.

## Lifetime and Limits

| Setting | Default |
| --- | --- |
| `--ttl` | 10 minutes from handle creation |
| `--max-snapshots` | 16 total snapshot/change-set handles |
| `--max-bytes` | 64 MiB of retained serialized artifacts |
| `--timeout` | 2 minutes per operation, including queue wait |
| Page size | 20 by default, at most 100 |
| Tool arguments / service response | 64 KiB each |
| MCP stdio frame | 128 KiB before protocol decoding |
| CLI batch input / output | 1 MiB each |
| Source capture | At most 32 MiB or the smaller configured artifact budget; 20,000 entries |

Preview allocates three handles atomically. Release them individually when no
longer needed. Expiry is enforced on subsequent operations; no background daemon
or persistent state is required. Restarted servers cannot restore old handles.

Artifact bytes are not an RSS quota: temporary renderer/decoder allocations,
allocator overhead and fixed handle bookkeeping are not included. Timeouts are
cooperative, not hard process isolation; noncooperative dependency code can delay
shutdown. Deploy in an external sandbox if hard resource or OS boundaries are
required. Query pages can shrink to satisfy the byte budget; follow `nextOffset`.

## Trust and Supported Inputs

Local mode is the default. It restricts supported Git, Helm and Kustomize source
loading to captured local inputs, explicitly disables external plugins, and
requires local charts and their vendored dependencies. Repositories and tool
arguments cannot grant permissions.

Only an operator can start `fmp mcp --trusted` or `fmp agent --trusted`. This grants
legacy network-capable rendering and custom Rego evaluation, with broad process
access and ambient credentials. It is not a narrowly scoped network sandbox.
Custom policy files must still be captured within the selected workspace.

Both agent profiles reject known unsupported render-affecting inputs instead of
claiming completeness: Flux Kustomization `substituteFrom`, patches, images,
components, name prefixes/suffixes, common metadata and decryption; Helm
`valuesFiles` and `ignoreMissingValuesFiles`. Additional local-only restrictions
cover custom generators/transformers/validators, Helm generators, remote schema
references and external postrenderers. This is a supported-input boundary, not
a guarantee of full Flux-controller parity.

Required SOPS decryption is denied in both profiles. Repository AI configuration
is ignored with diagnostics; no nested model calls occur. Repository Helm
credential/cache overrides are ignored. No tool applies manifests or publishes
comments, labels, reports or artifacts.

Core `v1/Secret` payloads are redacted. Credential-like keys elsewhere receive
best-effort redaction, not a guarantee that arbitrary application data is safe to
send to a model. Metadata, producer text, chart content and policy output remain
untrusted data, never instructions.

## Validation

The repository tests cover real SDK client/server calls over in-memory and stdio
transports, published schemas, cancellation, bounded frames, sanitized errors,
redaction, session retention, source confinement, completeness, and policy/budget
outcomes. The companion skill repository tests a local Helm edit-and-rerender
workflow without network or cluster access. Live model-driven OpenCode and Claude
Code sessions are a separate validation step, not implied by protocol tests.
