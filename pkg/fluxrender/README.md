# Flux rendering cache boundaries

Each `OpenRender` creates a fresh session. Helm desired output is memoized within
that session by the release spec, its selected `valuesFrom` data keys, the chart
source spec, and the resolved Git chart path. This memo survives host sweeps, so
random-valued Helm templates converge within an evaluation. Independent sessions
render independently, preserving permadiff detection. Reconciliation status,
unrelated metadata, and unused values keys do not invalidate desired output.
Target histories are bounded and removed when the target disappears. Local
chart paths are treated as stable source snapshots during a session; changed
source trees should open a fresh session.

Acquisition has a separate boundary. An explicit `OpenRequest.RunID` pins mutable
Git branches/tags and Helm version ranges/tags for one preview run. Before/head
and normalization sessions can share that run's acquired artifacts without
sharing desired output. The service retains at most one named acquisition epoch.
A different run resets its repositories and chart-selector cache once the old
run has no active sessions; overlapping different named runs are rejected.
Without a `RunID`, a session receives private acquisition caches that are removed
on `CloseRender`.

This bounds retained selector state without a process-wide cache that freezes
mutable selectors forever. The tradeoff is that a new run may reacquire Git
repositories and re-resolve chart selectors; Helm's content-addressed disk cache
can still reuse immutable chart archives. The last named epoch remains available
between sequential sessions and is released on the next run or `Service.Close`.
