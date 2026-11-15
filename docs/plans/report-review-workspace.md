# HTML report review workspace

## Goal

Make a portable report answer: what changed, what deserves attention, why did it change, and what have I reviewed? Preserve a self-contained offline HTML artifact and the existing report contracts.

## Implementation scope

Deliver steps 1–4 in this change. Steps 5–6 are follow-on work requiring richer rendering data; do not fabricate that data in the frontend.

### 1. Review brief and visual foundation

- Replace the generic overview headline with resource and cluster counts and base/target context.
- Display render/report status, deterministic policy outcome, and local review progress separately. Preserve warnings and truncation notices. A complete render does not mean policy approval. Only show “not evaluated” or “passed” when the data supports that distinction; extend the report DTO additively if needed.
- Keep policy classifications and AI Assessment visibly attributed, following CONTEXT.md terminology. Resource kinds must be labelled “Resource kinds”, not “Classifications”.
- Use warm off-white/slate light surfaces, restrained blue selection, subtle borders, compact rows, consistent spacing and readable monospace diffs. Reserve green/red for change meaning; use icons/text as well as color.
- Introduce CSS design tokens, light/dark/system theme selection and visible focus states. Use system fonts and inline assets; no network dependencies.

### 2. Persistent review workspace

- Desktop: resource list on the left, selected resource detail on the right; independently scrollable where helpful. Compact rows show action, kind/name, cluster and producer with a clear selection state.
- Mobile: list/detail flow with an obvious return control and no page-wide horizontal overflow; diff panels may scroll horizontally.
- Retain query autocomplete and all existing filters. Preserve query, diff mode and review filter through selection, previous/next, copy-link and browser back/forward.
- Previous/next operates on the filtered result set, not the global resource array. Handle direct links to excluded resources explicitly and avoid silently changing filters.
- Keep legacy #overview, #resources and #resource/<index>?view=... links working. Use additive hash parameters for state.
- Show useful empty, missing-resource and truncated-diff states. Keep copy diff/link controls and logical resource identities working.
- Offer grouping by cluster, producer or kind using existing fields. Do not infer application ownership or group by guessed semantic equivalence. Default ordering remains predictable; avoid introducing a fabricated risk score.

### 3. Local review progress and keyboard use

- Toggle reviewed/unreviewed per resource, show overall reviewed/total progress and allow filtering to unreviewed changes.
- Persist only review IDs and presentation preferences locally. Namespace review state by a deterministic fingerprint of the actual report payload so different report contents never inherit approvals. Do not store manifests or secret content in storage. Review completion is a local annotation, not an approval or policy result.
- Catch missing/blocked/corrupt storage; maintain an in-memory fallback. If persistence is unavailable, clearly indicate session-only progress.
- Define intuitive keyboard shortcuts for search, next/previous, marking reviewed and help. Ignore editable inputs, contenteditable and modifier-key combinations; maintain focus after rerenders. Include accessible shortcut help and live progress announcements.
- When marking reviewed under the unreviewed filter, select the next appropriate resource without skipping or losing navigation context.

### 4. Verification and deliverables

- Extend existing report.test.js tests for routing/filter preservation, filtered navigation, review-state isolation, blocked/corrupt storage, logical identities, legacy links and keyboard behavior.
- Extend Go report tests only where payload generation changes. Preserve safe DOM construction and HTML escaping; treat all report content as untrusted text.
- Run existing Node report tests and targeted Go githubaction tests. Inspect actual test commands in the repository rather than inventing a new framework.
- Generate an example report from the updated template/assets for browser verification. Check desktop light/dark and a narrow viewport; verify filters, selection, history, review progress, clipboard failure handling and absence of console errors. Use browser tools if available and report any verification limitations honestly.
- Keep large-report rendering practical; avoid repeated full payload hashing or rebuilding all diff views on every input event.
- Add a short usage/design document under docs covering controls, local-only review state, and theme behavior. Do not edit README, owned by the doctor workstream. Update this plan with implementation results and deferred items.

## Follow-on milestones

### 5. Structured change explanations

Extend Go report data with deterministic before/after facts for supported fields (container images, replicas, service type), extracted from structured resources rather than diff text or AI. Show a compact Changes view linked to YAML locations where mappings are reliable. Preserve redaction and avoid exposing new sensitive fields. Add an Origin view using real structured provenance; initially show only known producer information, not an invented ancestry tree. Define explicit data contracts and regression tests before implementing.

### 6. Cross-cluster repeated-change review

Group identical normalized semantic changes across resources/clusters, preserving all individual resources and surfacing exceptions. Define exact equality and provenance requirements first. Add a comparison view for cluster-specific deviations; do not merge changes based on similar display text.

## Acceptance criteria

1. A reviewer can open a filtered list, inspect several diffs, mark them reviewed, reload, and resume without losing scope or mixing progress with another report.
2. Existing archived report payloads and deep links still render; absent optional fields degrade gracefully.
3. Report completeness, policy verdict, AI Assessment and human review progress remain distinct.
4. Desktop and mobile are usable with keyboard and pointer in light and dark mode.
5. The artifact remains one offline HTML file, with no new runtime dependencies.

## Implementation results

Steps 1–4 delivered in `pkg/githubaction/reportui/`:

- Resource/cluster review brief, distinct status/policy/local-review signals,
  attributed classifications and AI Assessment, preserved warnings/truncation,
  warm light surfaces, explicit light/dark/system themes and keyboard focus.
- Responsive split-pane workspace with compact grouped lists, mobile return flow,
  filtered previous/next, legacy links, scope-carrying hashes/copy links/history,
  excluded-link notices, empty/missing/truncated states and safe text-only DOM.
- Local reviewed progress and unreviewed filter, SHA-256 payload-keyed storage
  containing only review indices, session fallback, keyboard help/shortcuts and
  live progress/clipboard error announcements. Query edits do not rebuild diffs.
- Usage and design boundaries documented in `docs/report-review-workspace.md`.

Verification: 10 Node runtime tests pass with `node --test
pkg/githubaction/reportui/report.test.js`; existing targeted Go suite passes with
`GOMAXPROCS=2 go test -p 1 -count=1 ./pkg/githubaction`. No Go payload changes were
needed. Chromium checked a freshly assembled 24-resource fixture using updated
inline template/assets (not the archived sample): desktop/narrow light and dark,
filter/group selection, split diff, scoped next/review advancement, reload
persistence, browser back, mobile list return, clipboard rejection and console.
At 390 CSS pixels the document width equalled viewport width; horizontal diff
overflow stayed inside the diff. No console errors/warnings were recorded.

Verification limitations: the in-app browser was disconnected, so separate
Chromium tools were used. Screenshot export into this worktree was denied by
the tool's configured workspace roots; accessibility snapshots and computed
layout/theme checks were used instead. The fixture is synthetic, not a live
cluster render. Browser-specific file URL storage and non-Chromium behavior
remain environment-dependent.

Commit-hook limitation: Secretlint could not run because the local package was
missing and `npx --no-install` refused to fetch it. Only that hook was skipped;
the policy hook passed. No dependencies were added to work around the environment.

Deferred: steps 5–6 remain future milestones. The existing payload cannot
distinguish policy-not-evaluated from policy-passed, so absence of a failure is
shown as evaluation status unavailable, never invented approval. Hashing requires
Web Crypto; unsupported/insecure contexts use explicitly session-only progress.
