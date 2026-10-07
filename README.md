# gitops-preview-toolkit

[![Build Status](https://github.com/tobiash/gitops-preview-toolkit/actions/workflows/ci.yml/badge.svg)](https://github.com/tobiash/gitops-preview-toolkit/actions/workflows/ci.yml)
[![GitHub release (including prereleases)](https://img.shields.io/github/v/release/tobiash/gitops-preview-toolkit?include_prereleases)](https://github.com/tobiash/gitops-preview-toolkit/releases)
[![Go Report Card](https://goreportcard.com/badge/github.com/tobiash/gitops-preview-toolkit)](https://goreportcard.com/report/github.com/tobiash/gitops-preview-toolkit)
[![License](https://img.shields.io/github/license/tobiash/gitops-preview-toolkit)](https://github.com/tobiash/gitops-preview-toolkit/blob/main/LICENSE)

**Preview rendered Kubernetes resource changes before a GitOps change reaches the cluster.**

`gitops-preview` renders supported GitOps inputs through persistent local plugins, compares two revisions, and turns the result into CLI diffs, structured JSON, PR comments, policy checks, and an interactive HTML report. `fmp` remains a compatibility executable with the same commands, flags, exit codes, `FMP_*` environment variables and Action report artifacts. Existing `fmp` examples below work with either executable.

The core host owns comparison and reports. `gitops-preview-flux` owns Flux, Git, Helm and path rendering; the optional `gitops-preview-crossplane` plugin expands compositions through an external real Crossplane engine. The core binary does not embed Helm or Flux rendering libraries. The canonical repository and Go module are `github.com/tobiash/gitops-preview-toolkit`.

It is built for Kubernetes platform teams reviewing GitOps pull requests where the source YAML is not the whole story: Flux `Kustomization` dependencies, `HelmRelease` rendering, external `GitRepository` sources, Crossplane composition outputs, generated metadata, and multi-cluster layouts all affect the final manifests.

> [!NOTE]
> `v0.2.0-rc.1` is a release candidate, not the latest stable release. Pin this version for reproducible evaluation; see the [release notes and Go API migration](docs/releases/v0.2.0-rc.1.md). Workflows and report details may continue to evolve.

---

## What it shows you

Instead of asking “what YAML changed?”, `gitops-preview` answers “what desired Kubernetes resources change after the supported GitOps inputs are rendered?”

```mermaid
flowchart TD
    A[Git branch, PR, or local worktree] --> B[Flux Kustomizations]
    B --> C[HelmReleases]
    B --> D[External GitRepository sources]
    C --> E[Rendered Kubernetes manifests]
    D --> E
    E --> F[CLI diffs and summaries]
    E --> G[Policy checks and PR labels]
    E --> I[Interactive HTML report]
```

Use it to catch risky changes before merge:

- image updates and replica changes hidden inside Helm values
- Ingress/Gateway/HTTPRoute or Service exposure changes
- Secret, CRD, PVC, StatefulSet, DaemonSet, and Namespace changes
- noisy generated fields that would otherwise create permanent diffs
- cluster-specific impact in multi-cluster Flux repositories

---

## HTML report preview

Enable `html-report` in the GitHub Action to generate a browsable report artifact, or deploy it to GitHub Pages for direct links from pull requests.

| Impact overview                                                                                     | Unified resource diff                                                                                    | Side-by-side diff                                                                                                       |
| --------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| [![HTML report overview](docs/assets/fmp-report-overview.png)](docs/assets/fmp-report-overview.png) | [![Unified diff view](docs/assets/fmp-report-unified-diff.png)](docs/assets/fmp-report-unified-diff.png) | [![Side-by-side diff view](docs/assets/fmp-report-side-by-side-diff.png)](docs/assets/fmp-report-side-by-side-diff.png) |

View the [sample HTML report in a browser](https://raw.githack.com/tobiash/gitops-preview-toolkit/main/docs/examples/fmp-report-example.html), or download the checked-in [single-file HTML report](docs/examples/fmp-report-example.html) to open it locally. The archived sample retains its original title and payload; the [nested Crossplane/Flux example](docs/examples/nested-crossplane-flux/README.md) demonstrates the combined workflow.

The report includes:

- long-form impact summary
- added / modified / deleted resource counts
- multi-cluster breakdowns
- resource browser with search and filters for kind, namespace, cluster, producer, and action
- unified and side-by-side per-resource diffs
- policy classifications and violations when configured

---

## Key features

- **Flux-aware rendering** — discovers `Kustomization.spec.path`, follows Flux dependency patterns, and can resolve external `GitRepository` sources.
- **HelmRelease support** — renders `HelmRelease` resources through the Helm SDK, including post-renderers and `commonMetadata`.
- **Crossplane composition preview** — opt-in expansion through a pinned external controller engine, with logical identities for unnamed outputs and separate evaluation evidence.
- **Git-aware diffs** — compare `HEAD`, branches, commits, local paths, or your dirty worktree.
- **Multi-cluster reports** — configure several cluster roots and review each cluster independently.
- **SOPS support** — decrypt encrypted resources locally when requested.
- **Noise reduction** — normalize generated fields such as timestamps, random hashes, or certificate data.
- **Policy checks** — classify changes, block risky PRs, and suggest/apply labels using built-in or custom Rego policies.
- **AI assessment** — optionally generate a concise review summary and provenance-aware classifications with OpenAI, Anthropic, Z.AI, MiniMax, or OpenRouter.
- **Agent-friendly output** — render, diff, test, and discovery commands support structured JSON, with failure diagnostics kept in a single document.

---

## Install

Install the host **and its sibling plugins**, or extract a complete release archive into one directory on `PATH`. Installing only `go install ./cmd/fmp` (or only `cmd/gitops-preview`) installs a host with no render plugin; render operations fail unless `gitops-preview-flux` is already installed beside it or on `PATH`. The Crossplane plugin is additionally needed for `--crossplane`.

### From source

```bash
go install ./cmd/gitops-preview ./cmd/gitops-preview-flux ./cmd/gitops-preview-crossplane
# Optional compatibility command:
go install ./cmd/fmp
```

To install the complete toolkit including the compatibility executable in one command:

```bash
go install ./cmd/fmp ./cmd/gitops-preview ./cmd/gitops-preview-flux ./cmd/gitops-preview-crossplane
```

The Go module and GitHub source repository are `github.com/tobiash/gitops-preview-toolkit`. Test fixture builds disable VCS stamping. Release builds retain normal Go VCS behavior; the Docker action defaults to `BUILDVCS=false` for source-only build contexts and accepts `--build-arg BUILDVCS=true` when complete Git metadata is available.

### Release candidate via Go

After the RC tag is published, install all commands at the same explicit version:

```bash
go install github.com/tobiash/gitops-preview-toolkit/cmd/gitops-preview@v0.2.0-rc.1
go install github.com/tobiash/gitops-preview-toolkit/cmd/gitops-preview-flux@v0.2.0-rc.1
go install github.com/tobiash/gitops-preview-toolkit/cmd/gitops-preview-crossplane@v0.2.0-rc.1
# Optional compatibility command:
go install github.com/tobiash/gitops-preview-toolkit/cmd/fmp@v0.2.0-rc.1
```

`@latest` prefers a stable Go module version when one exists; it does not select this RC over an existing stable release. GitHub's `/releases/latest` likewise excludes prereleases. Use the explicit RC tag until stable promotion.

### Requirements

- `git` for git-aware diffing and external repository resolution
- Install the three executables together. Defaults resolve plugins beside the host executable, then on `PATH`. `--flux-plugin` and `--crossplane-plugin` select trusted executable overrides; repository configuration cannot select commands.
- Helm rendering uses the SDK in the Flux plugin; a standalone `helm` executable is not required. Unset Helm settings use the plugin's environment defaults.
- Release archives `gitops-preview-toolkit_<tag>_<os>_<arch>.tar.gz` contain the core, both plugins, and `fmp`. Compatibility `fmp_<tag>_<os>_<arch>.tar.gz` archives include `fmp` and both plugins. The GitHub Action downloads and caches the complete toolkit.

### Configuration and Crossplane

The host discovers `.gitops-preview.yaml` (or `.gitops-preview.yml`) first, then legacy `.fmp.yaml`, `.fmp.yml`, or `.github/fmp.yaml`. `--config` and the existing Action `config` input select an explicit file. A minimal configuration is:

```yaml
paths: [clusters/production]
helm: true
sort: true
crossplane:
  enabled: true
  timeout: 1m
  max-functions: 32
```

`crossplane.enabled` records repository intent; it never grants execution. Enable Crossplane explicitly from a trusted invocation with `--crossplane` (or Action input `crossplane: true`). Neither executable paths nor development endpoints are read from repository configuration or resource annotations.

```bash
gitops-preview render . -k clusters/production --crossplane \
  --crossplane-engine /opt/crossplane-v2.4.2/crossplane

# Repeatable trusted overrides for individual Function names:
gitops-preview diff main --crossplane \
  --crossplane-function function-go-templating=127.0.0.1:9443 \
  --crossplane-function function-auto-ready=127.0.0.1:9444
```

Crossplane rendering defaults to the **Docker** function runtime and requires access to Docker. Install the external **Crossplane controller binary v2.4.2**, not the user-facing Crossplane CLI; the default executable name is `crossplane-core` on `PATH`, or select its path with `--crossplane-engine`. The plugin checks its version before execution. For Linux/amd64, the controller artifact is [crossplane v2.4.2](https://releases.crossplane.io/stable/v2.4.2/bin/linux_amd64/crossplane). The engine is not included in toolkit archives or `Dockerfile.action`. That image installs the host and both sibling plugins; Crossplane use additionally requires the engine and Docker runtime access.

Install the pinned engine locally, checking its published SHA-256 before executing it (Linux/amd64):

```bash
(
set -euo pipefail
engine_dir=$(mktemp -d)
trap 'rm -rf "$engine_dir"' EXIT
url=https://releases.crossplane.io/stable/v2.4.2/bin/linux_amd64/crossplane
curl -fL "$url" -o "$engine_dir/crossplane"
curl -fL "$url.sha256" -o "$engine_dir/crossplane.sha256"
expected=$(tr -d '[:space:]' < "$engine_dir/crossplane.sha256")
printf '%s  %s\n' "$expected" "$engine_dir/crossplane" | sha256sum --check --status
chmod +x "$engine_dir/crossplane"
test "$("$engine_dir/crossplane" --version)" = v2.4.2
mkdir -p "$HOME/.local/bin"
install -m 0755 "$engine_dir/crossplane" "$HOME/.local/bin/crossplane-core"
)
```

Put `$HOME/.local/bin` on `PATH`, or pass `--crossplane-engine "$HOME/.local/bin/crossplane-core"`. The toolkit never downloads an engine implicitly. Engine and plugin executable selection and development endpoints are trusted invocation settings, not fields that a checked-out repository can grant.

Function definitions are automatically derived from rendered `Function` resources and composition references. Their names select trusted per-function development overrides. Unnamed composed resources use **Logical Resource Identity**: a stable parent/composition output key for matching and reports, not a generated live Kubernetes name. Composite status and readiness are **Evaluation Evidence**, separate from desired resources.

Preview performs bounded desired-state discovery, not arbitrary cyclic controller reconciliation. Offline rendering needs locally available sources/charts, cached function images or explicitly reachable development targets, and the external engine. Local-only agent mode restricts remote acquisition; it is not an OS sandbox and does not grant Crossplane execution.

Agent and MCP commands accept the same startup `--flux-plugin`, `--crossplane`, `--crossplane-plugin`, `--crossplane-engine` and repeatable `--crossplane-function` flags. Crossplane additionally requires `--trusted` because composition functions cannot run within local-only policy. Agent/MCP runtime configuration comes exclusively from startup flags: repository Crossplane settings and JSON operation requests cannot choose executables or grant development endpoints.

```bash
gitops-preview agent --root . --trusted --crossplane \
  --crossplane-engine "$HOME/.local/bin/crossplane-core" \
  --crossplane-function function-go-templating=127.0.0.1:9443 <<'JSON'
{"operation":"render","paths":["clusters/production"]}
JSON

gitops-preview mcp --root . --trusted --crossplane \
  --crossplane-engine "$HOME/.local/bin/crossplane-core"
```

CI runs transport/plugin-host integration tests with race detection, plus `tests/plugins/run-live.sh` on Linux with Go 1.26. The live chain downloads and checksum-checks the pinned controller, starts the real upstream function in Development mode, and exercises both plugins without Docker.

---

## Quick start

Run this inside a Flux repository:

```bash
fmp diff
```

By default, this compares rendered manifests from `HEAD` with your current worktree, so you can review the cluster impact of uncommitted local changes.

Configure `paths` or cluster roots in `.gitops-preview.yaml` (legacy `.fmp.yaml` also works), or supply `-k/--path` (for example, `gitops-preview diff -k clusters/production`). A repository argument selects the source directory; it does not implicitly select `.` as a render root. Missing render roots are an error.

Common examples:

```bash
fmp diff                      # HEAD vs current worktree
fmp diff HEAD~1               # one commit back vs current worktree
fmp diff main feature-branch  # two git refs
fmp diff ./before ./after     # two local directories
fmp diff git:HEAD path:/tmp   # mix git refs and local paths
```

Render manifests directly:

```bash
fmp render <path>
fmp render --output json <path>
```

Discover and validate Flux resources:

```bash
fmp test <path>       # validate renderability
fmp get ks <path>     # list discovered Kustomizations
fmp get hr <path>     # list discovered HelmReleases
```

Automation helpers:

```bash
fmp ci                        # CI-optimized diff mode
fmp detect-permadiffs <path>  # suggest filters for noisy fields
```

---

## Configuration

Both entry points auto-discover `.gitops-preview.yaml` or `.gitops-preview.yml`, then `.fmp.yaml`, `.fmp.yml`, or `.github/fmp.yaml` for compatibility.

Minimal config:

```yaml
paths:
  - clusters/kube
recursive: true
helm: true
resolve-git: true
sort: true
exclude-crds: true
```

Multi-cluster repositories can use `cluster:path` entries:

```yaml
paths:
  - staging:clusters/staging/flux-system
  - production:clusters/production/flux-system
```

Or the `clusters` map:

```yaml
clusters:
  staging:
    - clusters/staging/flux-system
  production:
    - clusters/production/flux-system
```

Add field normalizers and policy rules when you want deterministic diffs and automated review gates:

```yaml
paths:
  - staging:clusters/staging/flux-system
  - production:clusters/production/flux-system
recursive: true
helm: true
resolve-git: true
sort: true

filters:
  - kind: FieldNormalizer
    match:
      kind: Secret
    fieldPaths:
      - path: [data, tls.crt]
        action: replace
        placeholder: "<<auto-generated>>"

policies:
  builtin:
    - image_update
    - ingress_change
    - secret_change
  fail-on:
    - forbid_latest
  labels:
    image_update: image-update
    ingress_change:
      - needs-network-review
      - risky-change
  inline:
    - |
      package fmp
      import rego.v1

      violations contains {
        "id": "forbid_latest",
        "message": sprintf("%s/%s uses :latest", [change.namespace, change.name]),
        "severity": "error"
      } if {
        some change in input.changes
        spec := object.get(change.new, "spec", {})
        template := object.get(spec, "template", {})
        podspec := object.get(template, "spec", {})
        some c in object.get(podspec, "containers", [])
        endswith(object.get(c, "image", ""), ":latest")
      }
```

In `diff` mode, configuration is loaded from the current worktree so local config changes take effect immediately.

### Built-in policy IDs

Built-in policies currently include:

- `image_update`
- `secret_change`
- `ingress_change`
- `crd_change`
- `namespace_delete`
- `stateful_workload_change`
- `pvc_change`
- `service_type_change`
- `replicas_change`

`fail-on` matches policy IDs. If any listed rule matches, `fmp diff` exits non-zero and the GitHub Action fails. `labels` maps policy IDs to one or more pull request labels.

### AI assessment

AI assessment is an optional generated review aid for `fmp diff` and the GitHub Action. It adds a concise generated summary and may add classifications with `provenance: ai`; policy classifications use `provenance: policy`.

Enable it in config:

```yaml
ai:
  enabled: true
  provider: openai # openai, anthropic, zai, minimax, or openrouter; optional if exactly one token is set
  model: gpt-4o-mini # optional provider-specific override
  fail-on-error: false
  allowed-classifications:
    - network_exposure
    - data_risk
  max-input-bytes: 100000
  max-diff-lines-per-resource: 80
  timeout: 30s
  instructions: |
    Focus on production-impacting Kubernetes changes.
```

Or enable per invocation:

```bash
OPENAI_API_KEY=... fmp diff --ai-assessment --summary
```

Supported credential environment variables are `OPENAI_API_KEY`, `ANTHROPIC_API_KEY`, `ZAI_API_KEY`, `MINIMAX_API_KEY`, and `OPENROUTER_API_KEY`. If no provider is configured, `fmp` auto-selects only when exactly one supported token is present; missing or ambiguous credentials warn and skip AI assessment by default.

AI classifications participate in the same `policies.labels` and `policies.fail-on` mappings as policy classifications. Use `ai.allowed-classifications` when you want to restrict which generated classification IDs may enter the unified classification set.

---

## GitHub Action

Use the action to review GitOps pull requests automatically. The release-tagged Action downloads the matching binary bundle:

```yaml
- uses: actions/checkout@v6
  with:
    fetch-depth: 0 # required for git-aware diffing

- uses: tobiash/gitops-preview-toolkit@v0.2.0-rc.1
  with:
    repo: .
    base-ref: origin/main
    resolve-git: true
    comment: true
    html-report: true
    ai-assessment: true
  env:
    OPENAI_API_KEY: ${{ secrets.OPENAI_API_KEY }}
```

### Rich HTML report artifact

```yaml
- uses: tobiash/gitops-preview-toolkit@v0.2.0-rc.1
  with:
    repo: .
    base-ref: origin/main
    html-report: true
```

The action uploads the report as an artifact and exposes:

- `html-report-file` — generated `index.html`
- `html-report-artifact` — uploaded artifact name
- `html-report-url` — direct report URL when available

### Deploy report to GitHub Pages

```yaml
- uses: tobiash/gitops-preview-toolkit@v0.2.0-rc.1
  with:
    repo: .
    base-ref: origin/main
    html-report: true
    html-report-pages: true
```

This publishes reports to the `gh-pages` branch. When Pages is not enabled, the report is still available as a downloadable single-file HTML artifact that GitHub can preview directly.

### Export rendered manifests

The action's `export-dir` input exports the retained target manifests, including unnamed Crossplane outputs. Set `export-changed-only: true` to export only added or modified target resources. The destination must be absent or empty; filenames are deterministic and cluster-scoped. Use `gitops-preview render` for direct manifest output.

### Important inputs

| Input                 | Description                                        | Default         |
| :-------------------- | :------------------------------------------------- | :-------------- |
| `repo`                | Path to the repository checkout                    | `.`             |
| `base-ref`            | Git ref to diff against                            | `origin/main`   |
| `base-sha`            | Exact SHA to diff against; overrides `base-ref`    |                 |
| `paths`               | Directories to render, newline-separated           |                 |
| `recursive`           | Recursively discover paths                         | `false`         |
| `helm`                | Enable Helm rendering                              | `true`          |
| `resolve-git`         | Clone external `GitRepository` sources             | `false`         |
| `sort`                | Sort output for deterministic diffs                | `false`         |
| `exclude-crds`        | Strip CRDs from output                             | `false`         |
| `config`              | Explicit config path                               | auto-discovered |
| `comment`             | Post/update a sticky PR comment                    | `false`         |
| `comment-mode`        | When to comment: `changes`, `always`, or `failure` | `changes`       |
| `html-report`         | Generate and upload the HTML report                | `false`         |
| `html-report-pages`   | Deploy the HTML report to GitHub Pages             | `false`         |
| `ai-assessment`       | Enable AI assessment; empty inherits config        |                 |
| `ai-provider`         | AI provider override                               |                 |
| `ai-model`            | AI model override                                  |                 |
| `ai-fail-on-error`    | Fail if AI assessment cannot be generated          |                 |
| `export-dir`          | Reserved; manifest export is not implemented       |                 |
| `export-changed-only` | Reserved; manifest export is not implemented       | `false`         |
| `fail-on-warning`     | Fail the step on warnings                          | `false`         |
| `fail-on-error`       | Fail the step on errors                            | `true`          |

### Important outputs

| Output                                                         | Description                                               |
| :------------------------------------------------------------- | :-------------------------------------------------------- |
| `status`                                                       | Overall status: `clean`, `changed`, `warning`, or `error` |
| `changed`                                                      | Whether manifest changes were detected                    |
| `resources-added` / `resources-modified` / `resources-deleted` | Change counts                                             |
| `resources-total`                                              | Total changed resources                                   |
| `diff-file`                                                    | Unified diff preview file; may be truncated                |
| `summary-file`                                                 | Markdown summary file                                     |
| `report-file`                                                  | Structured JSON report                                    |
| `html-report-url`                                              | Direct URL to the interactive HTML report when available  |
| `export-dir`                                                   | Requested export directory; does not confirm an export    |
| `classifications-json`                                         | Matched classifications with provenance                    |
| `violations-json`                                              | Matched policy violations                                 |
| `labels-json`                                                  | Suggested or applied PR labels                            |
| `policy-failed`                                                | Whether a configured `fail-on` policy matched             |
| `ai-assessment-json`                                           | AI assessment object when generated                       |
| `ai-summary`                                                   | AI-generated summary when generated                       |

> [!WARNING]
> `sops-decrypt` is intentionally unsupported in the GitHub Action to avoid leaking decrypted content into logs, summaries, comments, or artifacts.

---

## Structured output and exit codes

### Agent CLI and MCP

The versioned agent interface adds local stdio MCP tools and a JSON CLI over the
same operation service:

```sh
fmp agent schema
fmp agent discover --root /absolute/path/to/gitops
fmp agent --root /absolute/path/to/gitops < request.json
fmp mcp --root /absolute/path/to/gitops
```

It supports discovery, rendering, preview, bounded queries, redacted inspection,
snapshot comparison, deterministic checks, and explicit handle release. MCP
retains results between calls; CLI batches reuse results within one invocation.
Existing CLI output formats are unchanged.

Local-only rendering is the default. Trusted access is a startup option, never a
tool argument. Both agent profiles reject known unsupported rendering inputs;
there are no apply, publish, decryption, or nested-AI tools. Artifact limits and
cooperative timeouts are not an OS sandbox or a process memory quota.

See the [agent workflow and contract](docs/agent-workflow.md) and the portable
[OpenCode/Claude Code skill](https://github.com/tobiash/gitops-preview-skill).

### Legacy command output

`render`, `diff`, `test`, and `get ks/hr` support `--output json`:

```bash
fmp diff --output json
fmp render --output json <path>
fmp test --output json <path>
fmp get ks --output json <path>
fmp get hr --output json <path>
```

JSON output preserves the existing command-specific shapes:

- rendered resources are wrapped in a `v1/List` envelope; discovery commands use an `items` collection
- resource references use `ObjectRef` fields: `apiVersion`, `kind`, `name`, `namespace`
- errors include `{"status":"failure","error":{"reason":"...","message":"..."}}` in the same document as any available result, never as a second JSON document
- diff results include `complete`, `warnings`, and a unified `changes` array carrying action, cluster, structured provenance, before/after origins, and resource snapshots
- legacy `added`, `deleted`, and `modified` fields remain available; use `changes` to retain cluster and provenance context for every action
- empty change collections are `[]`, not `null`

Ordinary complete diffs continue to exit 0 even when resources change. Use `fmp diff --exit-code` to exit 1 for differences. A failed policy or execution still returns nonzero without this flag. With JSON output, a failed check retains the available diff and its `complete: true` field alongside failure information.

| Code | Meaning                                                 |
| ---- | ------------------------------------------------------- |
| 0    | Success; may include differences unless `--exit-code` is set |
| 1    | Differences with `--exit-code`, or an otherwise unclassified error |
| 2    | Explicitly classified user input error                  |
| 3    | Expansion failure or explicitly classified dependency failure |
| 5    | Policy violation                                        |

Not every validation or dependency error is classified separately yet. Treat any nonzero status as a failed command/check and inspect the JSON error or stderr for details.

### Incomplete previews

If either side fails to render, fmp suppresses the entire comparison rather than reporting missing resources as deletions or an empty diff as clean. Policy and AI assessments are skipped, and the CLI/action fails. JSON failures include `complete: false`; action reports carry error status and diagnostics even when advisory `fail-on-error` behavior is disabled.

Malformed manifests, duplicate output identities, failed source acquisition, unresolved sources at the discovery fixed point, and missing discovered paths are failures. Known fmp configuration files are not treated as raw manifests. Distinct Flux rendering contexts remain separate even when they use the same directory, while equivalent bootstrap paths are deduplicated.

Completeness covers the configured render scope and detected failures. It does not guarantee full Flux feature parity, live-cluster validity, or runtime behavior. Rendering may access remote sources, credentials, or external programs according to your configuration; it is not sandboxed.

The hidden `describe` command emits command and flag metadata for agents and other automation:

```bash
fmp describe
```

---

## Local pre-commit policy check

Example [lefthook](https://github.com/evilmartians/lefthook) hook that checks only staged changes:

```yaml
pre-commit:
  commands:
    fmp-policy:
      run: |
        if [ ! -f .fmp.yaml ] || ! grep -q "policies:" .fmp.yaml; then
          exit 0
        fi
        if ! command -v fmp >/dev/null 2>&1; then
          echo "warning: fmp not found in PATH, skipping policy check" >&2
          exit 0
        fi
        head_tree=$(git rev-parse HEAD^{tree})
        staged_tree=$(git write-tree)
        fmp diff "git:${head_tree}" "git:${staged_tree}" --summary-only
```

---

## Development

```bash
go test ./...
go vet ./...
```

For local action development or branch testing, build `gitops-preview` (or `fmp`) and both sibling plugins into one directory, then pass the host path to the action with the `binary` input.
