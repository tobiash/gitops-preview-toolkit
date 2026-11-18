# ResourceSet: two-region desired-state preview

This small, krops-inspired example fans regional inputs out into **namespaced Flux
Kustomizations**, then follows their local files to Namespace and ConfigMap
resources. It needs no cluster, cloud credentials, CAPI controllers or Helm chart.
Both regions belong to one preview inventory; a region input does not create a
cluster or imply a remote target.

## Run it

Build/install the host and `gitops-preview-flux` from the same checkout and put
both on `PATH` (see the repository [installation instructions](../../../README.md#install)).
From the repository root:

```sh
gitops-preview render docs/examples/resourcesets -k root
gitops-preview test docs/examples/resourcesets -k root
```

`root/source.yaml` uses `GitRepository.spec.url: .`, the renderer's local-source
convention pointing at the selected example directory. It is a preview fixture,
not a deployable remote Git URL. Only `root` is selected initially; the generated
Kustomizations discover `workloads/eu-west-1` and `workloads/us-east-1`.

Expected inventory: **eight resources** — one GitRepository, one ResourceSet, two
Kustomizations in `flux-system`, two Namespaces and two `regional-settings`
ConfigMaps in `apps-eu-west-1` and `apps-us-east-1`. Generated resources receive
ResourceSet ownership labels; Kustomization descendants retain their own producer
origins. Supported generated HelmReleases are also expanded when Helm rendering
is enabled.

To review an edit using two local copies:

```sh
cp -R docs/examples/resourcesets /tmp/resourcesets-before
cp -R docs/examples/resourcesets /tmp/resourcesets-after
# Edit /tmp/resourcesets-after/workloads/eu-west-1/resources.yaml, then:
gitops-preview diff /tmp/resourcesets-before /tmp/resourcesets-after -k root --output json
```

Changing the Europe message modifies that ConfigMap. Removing the `us-east-1`
input from the after copy's `root/regions.yaml` removes its generated
Kustomization and its Namespace/ConfigMap descendants from the desired inventory,
even if their files remain on disk. These are desired-state differences, not
predictions of controller pruning or cloud deletion.

For local-only agent rendering, start in the example directory:

```sh
gitops-preview agent --root docs/examples/resourcesets <<'JSON'
{"operation":"render","paths":["root"]}
JSON
```

Check `status` and `complete` before querying a returned handle; see the
[agent contract](../../agent-workflow.md).

## Supported ResourceSet inputs

The Flux plugin expands `fluxcd.controlplane.io/v1` ResourceSets discovered within
the selected render scope. There is no additional ResourceSet enablement flag.

| Input | Offline behavior |
| --- | --- |
| `spec.inputs` | Inline input objects, as in this example. |
| `ResourceSetInputProvider` with `spec.type: Static` | One input synthesized from `spec.defaultValues`. |
| Other provider types | Require an explicitly pre-baked `status.exportedInputs` snapshot; no live branch, pull-request, cloud or API discovery. |

`inputsFrom` selects providers by name **or** Kubernetes label selector in the
ResourceSet's namespace. References selecting the same provider are deduplicated.
The default `Flatten` strategy concatenates inputs. `Permute` creates a bounded
Cartesian product scoped by normalized provider names; its `includeEmptyProviders`
option makes an included empty group yield no outputs. `inputs.provider` exposes
the input's provider reference (for inline inputs, the ResourceSet itself).

For example, replace the inline `inputs` in `regions.yaml` with:

```yaml
inputsFrom:
  - kind: ResourceSetInputProvider
    name: europe
```

and add this manifest under `root`:

```yaml
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSetInputProvider
metadata:
  name: europe
  namespace: flux-system
spec:
  type: Static
  defaultValues:
    region: eu-west-1
    namespace: apps-eu-west-1
```

Static providers synthesize `inputs.id`, overriding a supplied default `id`.
With an explicit `metadata.uid`, the ID is the decimal Adler-32 checksum of that
UID. Ordinary local manifests have no API-server UID: the offline ID instead uses
the stable `namespace/name` string. **That offline ID is not the live UID-derived
ID.** Prefer explicit regional keys for output names when matching a live layout;
this example never uses the synthesized ID in a name.

A dynamic provider snapshot supplies its own input IDs and values, for example:

```yaml
apiVersion: fluxcd.controlplane.io/v1
kind: ResourceSetInputProvider
metadata:
  name: europe
  namespace: flux-system
spec:
  type: GitHubBranch
status:
  exportedInputs:
    - id: captured-main
      region: eu-west-1
      namespace: apps-eu-west-1
```

This consumes the supplied snapshot without contacting GitHub. An explicit empty
`exportedInputs: []` is a known empty input set and can retract outputs. A missing
named provider or dynamic provider without `status.exportedInputs` is a rendering
failure. It makes the preview incomplete and suppresses comparison on either
side: missing outputs must never be inferred as deletions. Likewise, changed
snapshot values are reevaluated even when output names stay unchanged.

## Templates and supported-input boundary

Both `spec.resources` object templates and multi-document
`spec.resourcesTemplate` use Go templates with `<<` / `>>` delimiters and the
`inputs` function. Missing keys fail rendering. `spec.commonMetadata` applies
labels and annotations to generated objects. Desired-state discovery is bounded;
it does not execute controller ordering, readiness or service-account semantics
from `dependsOn`, `wait` or `serviceAccountName`.

Only a deliberate template-function allowlist is available, alongside Go template
built-ins. `inputs`, `toYaml`, `mustToYaml`, bounded `indent` and `nindent` are
provided. The Sprig subset is:

```text
default empty coalesce all any ternary
quote squote toString toStrings split splitList splitn
trim trimAll trimSuffix trimPrefix upper lower title untitle
contains hasPrefix hasSuffix cat nospace trunc substr initials
snakecase camelcase kebabcase swapcase
b64enc b64dec b32enc b32dec toJson mustToJson fromJson mustFromJson
toRawJson mustToRawJson
list dict get hasKey keys values pluck pick omit dig
first mustFirst last mustLast rest mustRest initial mustInitial
reverse mustReverse uniq mustUniq compact mustCompact
without mustWithout has mustHas append mustAppend prepend mustPrepend
concat slice mustSlice sortAlpha len
add add1 sub div mod mul max min floor ceil round int int64 atoi float64
addf add1f subf divf mulf maxf minf
typeOf typeIs typeIsLike kindOf kindIs deepEqual fail
```

Formatting and escaping built-ins (`print`, `println`, `printf`, `html`, `js`,
`urlquery`) are bounded along with the helpers above. `printf` accepts bare format
verbs only, without widths, precision, flags or argument indexing.

Functions outside this allowlist, including `join`, `replace`, pretty-JSON helpers,
allocating generators such as `until`, `seq` and `repeat`, random/crypto,
environment and network helpers, are unsupported
and fail template parsing. ResourceSet `steps` and other unrecognized spec fields
are unsupported. Render-affecting transforms via
`fluxcd.controlplane.io/copyFrom`, `convertKubeConfigFrom` or `checksumFrom`
annotations are rejected, including in common metadata and nested output
metadata. A disabled ResourceSet or disabled Static provider cannot reproduce
retained live state offline and fails rendering.

Expansion is limited to 10,000 combined inputs/outputs, 128 provider references
and selected providers, 16 MiB byte budgets and a 65,536-operation template budget.
Function arguments and results are limited to 1 MiB of strings, 65,536 visited
values and depth 64 per call, counting shared sublists repeatedly. These guards
bound intermediate template materialization; they are not a hard process-memory
quota or an OS sandbox.
Exceeding a budget fails rendering rather than returning a complete partial
inventory.

## Relationship to krops and Cluster API

This example demonstrates regional fan-out only. It does **not** provision CAPI
clusters, expand `ClusterClass` topology, expand CAPI `ClusterResourceSet` objects
(a different API from Flux ResourceSet), or reconcile `HelmChartProxy` addons into
workload clusters. Explicit cloud-definition manifests are already previewable as
ordinary desired resources; their presence does not imply cloud provisioning.

Those controller expansions need target-aware inventories and runtime semantics
beyond ResourceSet templating. This is not full krops parity: Flux decryption,
`substituteFrom` and other unsupported Flux inputs still limit completeness,
particularly in strict/local-only agent profiles. A complete result describes
the supported configured desired-state scope, not a simulation of every
controller or remote cluster. See the [supported agent inputs](../../agent-workflow.md#trust-and-supported-inputs).
