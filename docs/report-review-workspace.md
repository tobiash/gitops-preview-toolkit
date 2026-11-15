# Reviewing an HTML report

Open the generated HTML file in a modern browser. All report assets are inline;
reviewing does not require a server or network connection.

- **Overview** separates resource counts, base/target context, report status,
  deterministic policy findings, AI Assessment, and your local review progress.
  An absent policy failure is not evidence that policies were evaluated or passed.
- **Resources** opens a compact list beside the selected manifest diff. On narrow
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

Warnings and truncated diffs are evidence limitations, not successful renders.
Archived payloads with no explicit policy evaluation outcome show that the outcome
is unavailable. Structured semantic change summaries, full provenance trees, and
cross-cluster equivalence review are deferred until the backend supplies reliable
structured facts; this UI does not infer them from diff text.
