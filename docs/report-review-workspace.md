# Reviewing an HTML report

Open the generated HTML file in a modern browser. All report assets are inline;
reviewing does not require a server or network connection.

- **Overview** separates resource counts, base/target context, report status,
  deterministic policy findings, AI Assessment, and your local review progress.
  An absent policy failure is not evidence that policies were evaluated or passed.
- **Review** opens a compact list beside the selected manifest diff. On narrow
  screens, **Back to resources** returns from detail to the list. Only diff panels
  scroll horizontally. Lists can be grouped by cluster, producer, or resource kind;
  grouping does not imply application ownership or equivalent changes.
- Search accepts free text and `action:`, `kind:`, `namespace:`, `cluster:`,
  `producer:`, `apiVersion:` (or `api:`), `name:`, and `logicalId:` filters.
  Suggestions support arrow keys and Enter. Logical identities and composition
  output slots remain searchable even when no Kubernetes name exists.
- Query, review filter, grouping and unified/split diff mode travel in the hash.
  Selection, previous/next, copy link, and browser history retain that scope.
  Previous/next uses only matching resources. A direct link outside the filters
  displays an explicit notice instead of silently clearing the filters.
- **Mark reviewed** is a local annotation, not approval or a policy result.
  **Unreviewed only** advances to the next remaining resource (or the preceding
  resource at the end) when marking reviewed. Reload resumes saved progress.

## Overview and scope

The shared header keeps **Base → Target** visible at every width. Open its
native disclosure to read complete, wrapping refs; shortened summaries never
replace the full values. Overview / Review switches reading scale without
discarding the current review selection, search, grouping or diff mode.

The connected green/blue/berry totals band and proportional strips encode
added/modified/deleted resource counts, not risk or approval. Counts come from
the indexed inventory and ignore Review filters. Cluster rows show their actual
counts and open their complete inventory. These explicit cluster actions clear
the previous search/review/group/diff scope and use an exact cluster name (the
`exactCluster` hash parameter); similarly named clusters are not included. Clear
filters removes that scope. Existing resource routes and query filters still work.

Reports with one or two resources and at most 80 available diff rows expose all
those rows directly in Overview, with an immediate evidence index. A small
inventory with larger evidence uses individual Review; rows are never sliced to
fit Overview. Larger reports show at most eight
inventory entries, the shown/total count, and a link to all changes. Source order
is retained; the preview is not a consequential-change or risk ranking.

**Cluster-wide findings** have their own report-level column/section, never a
place under the selected resource. Findings show the whole named cluster and
supplied provenance (or “unattributed”). Optional resource identity fields are
shown only as explicitly supplied source context in that global section; no
matches are inferred and no finding is moved into selected-resource detail.
Browse actions open that cluster's
inventory, not a claimed set of matching resources. Suggested labels remain
report-wide. Severity does not establish policy failure; no supplied failures
does not establish successful evaluation. AI Assessment remains a separately
attributed review aid, not a deterministic policy result.

Small/empty/error reports use the same production data path. An error report
with no resource evidence explicitly does not establish that there are no changes.
Truncation notices describe unavailable evidence; no semantic conclusions are
inferred from visible diff lines.

## Local storage and themes

Review state stores only resource indices, under a SHA-256 fingerprint of the
entire report payload. Changing any payload content, including generation time,
creates a separate review namespace. No manifests, diffs, queries, or secret
content are written to browser storage. Presentation storage contains only the
theme preference. Storage is browser/origin specific and is not shared with
teammates or synchronized into the report file. Copying the HTML does not copy
review notes. Clearing browser site data removes saved progress.

If storage is blocked, corrupt, or unavailable, or secure hashing is unavailable,
reviewing continues in memory and the UI says **session only**. Closing or
reloading then loses those session annotations. File URL storage behavior varies
between browsers; verify persistence before relying on it.

The theme selector supports **Light**, **Dark**, and **System theme**. System
follows the operating system; explicit choices override it. All modes use system
fonts, visible keyboard focus, text/action markers as well as change colors,
and respect reduced-motion preferences.

## Keyboard controls

`/` focuses search (opens the workspace from Overview); `j` / `k` navigate the
filtered resources; `r` toggles reviewed; `Escape` returns to the list. Arrow keys
and Enter focus/open list rows. `?` or **Help** opens accessible shortcut help.
Shortcuts do not intercept editable controls or modified key combinations.
Progress and clipboard failures are announced to assistive technology. Clipboard
permissions may be denied on local files; manual selection/copy remains available.

## Boundaries

Warnings are shown independently of the actual report status; their presence
does not imply incomplete rendering. Truncated diffs limit the available evidence.
Archived payloads with no explicit policy evaluation outcome show that the outcome
is unavailable. Structured semantic change summaries, full provenance trees, and
cross-cluster equivalence review are deferred until the backend supplies reliable
structured facts; this UI does not infer them from diff text.
