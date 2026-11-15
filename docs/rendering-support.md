# Rendering support and validation boundaries

Preview computes desired manifests for supported inputs; it is not a live Flux or
Crossplane reconciliation simulator. A successful local build does not prove that
a controller can reconcile the result, that the cluster accepts it, or that it is
ready. Unknown fields are not comprehensively rejected.

## Choose the right check

| Command | What it checks | What it does not prove |
| --- | --- | --- |
| `gitops-preview doctor [repository]` | Config discovery/parsing, effective root directory existence, trusted plugin executable discovery and name/protocol handshake | Manifest validity, supported input usage, available charts/sources, Docker/registry access, function readiness |
| `gitops-preview doctor ... --crossplane` | Above, plus trusted Crossplane plugin handshake and controller engine `--version` against v2.4.2 | Engine render compatibility, Docker daemon access, function images or development endpoint reachability |
| `gitops-preview get ks/hr <repository>` | Discovered resources within selected roots | Complete rendering or live reconciliation |
| `gitops-preview test <repository>` | Actual build/render checks and reported input/render failures | Cluster API admission, health, readiness or arbitrary controller behavior |
| `gitops-preview render <repository>` | Actual desired manifest expansion using configured inputs | Live observed state or controller convergence |

Use the same `--config`, `-k/--path`, source and runtime flags throughout. Render
commands may acquire sources/charts; `--crossplane` may execute composition
functions. Doctor does neither. It launches only trusted plugin executables for
`Describe`, closes them without `OpenRender`/`Expand`, and (when opted in) invokes
the trusted engine with `--version`.

Doctor JSON has `scope: "preflight"`, `status`, `complete`, and ordered `checks`
with `id`, `status`, `message` and optional `remediation`. Check statuses are
`success`, `failure`, `warning`, `skipped`. `complete: true` means the preflight
report was collected, **not** that a render was completed. On command failure the
normal CLI `error` envelope is added to the same document. No timestamps or random
session IDs are included in successful reports. Exit 0 allows warnings/skips;
exit 2 denotes config/root/invocation failures; exit 3 denotes dependency failures.
Config failures take precedence over dependency failures.

## Configuration and roots

Discovery order: `.gitops-preview.yaml`, `.gitops-preview.yml`, `.fmp.yaml`,
`.fmp.yml`, `.github/fmp.yaml`, within the repository argument (default `.`).
`--config` selects an explicit file relative to the invoking working directory.
Missing auto-discovered config is a warning, not an error if roots are supplied.
Malformed config or a missing explicit file is an error.

Roots come from `-k/--path`, config `paths`, or named `clusters`. A repository
argument alone is **not** a render root: use `-k .` intentionally. Root paths are
relative to the repository, not the config file. Doctor checks directory existence,
not contents or recursively discovered child inputs. Configured cluster roots
currently take precedence over flat paths, including CLI `--path`; doctor mirrors
that existing behavior. Cluster checks are ordered by cluster name, then path order.

## Rendering support matrix

The boundaries below follow `pkg/expander`, `pkg/fluxrender`, `pkg/render`, and
`pkg/crossplanerender`, rather than assuming upstream controller feature parity.

| Input / feature | Support | Boundary |
| --- | --- | --- |
| Local YAML and Kustomize directories | Supported | Explicit roots required; external executable Kustomize plugins disabled |
| Flux `Kustomization.spec.path` | Supported | Bounded desired-state expansion, not a reconciliation loop |
| Flux `targetNamespace` | Supported | Applied to discovered output |
| Flux `postBuild.substitute` | Supported | Inline substitution; no cluster lookup |
| Flux `patches`, `images`, `components` | Supported in integrated release† | Applied as Flux build customization, not merely raw directory Kustomize fields |
| Flux `namePrefix`, `nameSuffix`, `commonMetadata` | Supported in integrated release† | Applied to discovered output |
| Flux `postBuild.substituteFrom` | Unsupported | ConfigMap/Secret lookup semantics not implemented |
| Flux `spec.decryption` | Unsupported | Distinct from toolkit's explicit SOPS decryption option; not controller decryption parity |
| Flux Kustomization source kinds | `GitRepository` supported | External resolution requires `--resolve-git` and available sources; other source kinds rejected |
| ResourceSet / input providers | Bounded support | Inline, Static or pre-baked snapshots; see [example boundaries](examples/resourcesets/README.md) |
| HelmRelease `spec.chart.spec` | Supported | Supported chart sources must be available; embedded Helm SDK, no standalone Helm CLI needed |
| HelmRelease `chart.spec.valuesFiles`, `ignoreMissingValuesFiles` | Supported in integrated release† | Chart-file values are distinct from `valuesFrom` references |
| HelmRelease `spec.chartRef` | Unsupported | Existing HelmChart/OCIRepository references are not implemented |
| HelmRelease inline `values` and `valuesFrom` | Supported | References resolved from rendered inventory, not live cluster lookups |
| HelmRelease Kustomize post-renderers, `commonMetadata` | Supported | Built-in post-render chain; no arbitrary post-render command execution |
| Crossplane composition expansion | Opt-in | Requires sibling plugin, pinned controller v2.4.2 and actual function runtime prerequisites |
| Live Flux/Crossplane health, drift and readiness | Not simulated | Crossplane evaluation evidence is separate from desired resources |

† Integration prerequisite: the Flux customization and Helm chart values-file
changes are being integrated alongside this documentation. These rows describe
the intended combined release, not the standalone doctor branch. The integrator
must include and test those renderer changes before publishing these support claims.

Known unsupported inputs may be rejected in strict-input validation paths. Do not
interpret a doctor pass, or permissive rendering that ignores a field, as support.
Use `test`/`render` to exercise actual sources and consult their diagnostics.

Local-only agent policy additionally rejects remote acquisition, symlinks/special
files, Kustomize Helm generators and custom generators/transformers/validators;
it is not an OS sandbox. Trusted Crossplane execution is a separate grant, never
derived from repository configuration or resource annotations.
