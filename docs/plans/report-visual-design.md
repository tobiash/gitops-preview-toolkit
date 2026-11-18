# Approved report visual design: production implementation

## Design source and scope

Implements the approved **concept A**, including the stronger overview color pass
and final cohesive header from `bc9bd56` in the `report-concepts` worktree:
`docs/design/code-review/{README.md,index.html,review.css,header.css}`.
The original brief was reviewed from the main worktree's same plan path, together
with its locally installed `frontend-design` skill. This is implementation of
that selection, not a fourth visual exploration.

The report is an offline resource-change review tool for platform engineers.
Overview answers “what is supplied across the report?”; Review answers “what
does this resource's evidence say?”. Neither screen invents semantic impact,
approval, successful evaluation, ancestry or resource matches.

## Tokens and composition

- Paper `#ffffff`, navigation fog `#f2f5f8`, ink `#243448`, interaction/modified
  blue `#245bc4`, addition green `#14684a`, deletion berry `#aa2f51`.
- Connected overview fills mint `#d5eedf`, blue `#dfeafe`, rose `#f7dce5`;
  proportional solid strips `#25845b`, `#3169d8`, `#c0476a`. Neutral zero counts.
  Dark mode uses the approved navy/slate and corresponding colored surfaces.
- Segoe UI / Tahoma / Arial for the interface; Cascadia Code / DejaVu Sans Mono /
  Consolas for evidence and rule identifiers. All are offline system stacks.
- Shared two-level header: product identity, labeled Base → Target, quiet theme
  and Help controls; then Overview / Review. Native disclosure exposes full,
  wrapping refs at all widths. The 320px screenshot prompted a narrower theme
  control so the product identity remains one line.
- Left-aligned factual introduction, connected action band, cluster count rows;
  wider actual inventory beside a separately located **Cluster-wide findings**
  column. Mobile stacks these with an explicit findings jump link.
- Tiny inventories (one/two resources, at most 80 available rows) expose every
  supplied diff row inline, with an immediate evidence index. A small inventory
  with larger evidence opens individual Review, never silently sliced rows.
  Larger inventories stop at eight entries and explicitly state
  shown, total and remaining counts, retaining source order rather than ranking.
- Review retains the established production engine: compact 292px navigation,
  dominant readable YAML, unified/split modes, filtering/grouping/history and
  local review annotation. New color surfaces communicate action and severity,
  never success or approval.

Pre-build critique rejected introducing a tile dashboard, a decorative logo,
additional modes, inferred YAML summaries, or prototype scenario controls. The
approved design wins over stylistic novelty. Review uses production data and
behavior, not copied prototype runtime code or its demo state.

## Production behavior

Counts derive from actual indexed resources, independently of workspace filters.
Explicit cluster-browse actions clear previous workspace scope and use exact
cluster names; ordinary Overview / Review navigation retains selected resource,
search, review filter, grouping and diff mode. Legacy hashes remain supported.

Findings stay report-level, with whole-cluster headings and provenance; browse
links never claim matching resources. Optional explicitly supplied identity and
message fields are rendered literally as source context in that global section,
not inferred or moved under selected-resource detail. Missing policy failures
never imply evaluation succeeded. AI Assessment remains separately attributed.

No backend data-model changes were needed. The sole Go test edit updates the
report title assertion; escaping/redaction paths remain unchanged. Local storage
still contains only report-fingerprinted review indices and theme preferences,
with session-only fallback. Clipboard errors and keyboard controls are retained.

## Verification and visual critique

- Node: all original 14 regressions plus seven meaningful additions pass (21 total):
  inline tiny evidence, truncation/literal hostile text, bounded/global counts,
  whole-cluster policy scope, exact-name browsing/clear scope, retained selection
  and refs, error/empty language, supplied-only context, separate AI provenance,
  and individual review of large evidence even with few resources.
- Go: `GOMAXPROCS=2 go test -p 1 ./pkg/githubaction ./internal/cli/...
  ./cmd/gitops-preview/... ./cmd/fmp/...` passes. The two command packages have no
  direct tests; the CLI implementation's tests are in `internal/cli`.
- Fresh standalone previews use the production inline template/CSS/JS and JSON
  extracted from `docs/examples/fmp-report-example.html`. The unchanged original
  fixture is the production preview; small (2), large (132), empty, error and
  truncated variants are local verification inputs only, not production options.
- Chromium checks cover desktop 1440px and actual emulated 390px/320px viewports,
  light/dark, all six inputs, Overview and selected split Review. Checked client
  width against scroll width (not just `innerWidth`); no page overflow. Code
  scrolls within its own evidence container. Initial window resizing bottomed
  out at 500px; those checks were discarded and repeated with device emulation.
- Inspected actual before/after desktop screenshots, mobile tiny inline evidence
  in both themes, large dark overview, 320px error state, expanded long refs and
  dark split Review. Screenshot critique fixed an invisible dark secondary action,
  wrapped 320px product title and cramped warning/inventory spacing.
- Actual click/keyboard checks: whole-staging browse gives all seven resources;
  select resource, `r` marks locally, `j` moves within cluster; split mode remains
  scoped. Native ref disclosure opens with Enter and visible focus. Navigation
  preservation and clipboard/storage failure paths are also covered in Node.
  Tiny evidence links scroll within Overview; system-theme changes follow the
  emulated OS preference. Expanded 604-character refs fit at all three widths
  in both themes. Final browser console has no warnings/errors.

Before: narrow centered stacked metrics/cards, policy pills mixed into cluster
navigation, resource-kind tiles delaying actual inventory, inconspicuous product
title. After: cohesive title/comparison/navigation, meaningful connected action
color, aligned cluster rows, earlier inventory beside unmistakably global policy
evidence, tiny diffs readable without drilldown.

Tradeoffs: mobile keeps source context visible and spends more vertical space on
it; tiny reports repeat identities in the evidence index and evidence heading;
the stacked findings section follows inline evidence, hence the jump link. Long
unwrapped YAML scrolls horizontally inside the evidence panel. The existing
example fixture repeats Deployment-shaped snippets for several kinds; evidence
is intentionally not corrected or interpreted by the UI.

No full screen-reader audit, physical handset test or semantic impact engine is
claimed. Host fonts and file-URL storage behavior vary by browser/platform.

Local preview (not committed): `dist/report-preview/original.html`, served at
`http://localhost:8766/dist/report-preview/original.html`. Sibling verification
inputs and a before-layout preview are under the same uncommitted verification
directory. These artifacts are not part of the implementation commit.
