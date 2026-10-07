# ADR 0006: Shared agent operations with bounded retained artifacts

## Status

Proposed

## Context

Agent clients need structured evidence, completeness and producer context without
repeatedly rendering or parsing terminal reports. A CLI subprocess cannot retain
session handles for later subprocesses, while a local MCP process can. Existing
JSON output also has consumers whose contracts should not be replaced silently.

## Decision

`pkg/agent` owns version 1 operations, startup permissions, captured source inputs,
bounded serialized artifacts, redaction and result projections. `fmp agent` and
`pkg/agentmcp` are adapters over the same service. CLI batches supply ephemeral
handle reuse; MCP retains handles for its process lifetime. The schemas describe
each operation's inputs, successful result and operational failure.

The existing preview workflow still owns rendering and comparison. It can retain
the exact before/after snapshots used in a diff. Generic manifest matching,
accounting and comparison reuse k8q; no second render or diff engine is introduced.

Local-only acquisition is the default. A trusted startup profile can allow legacy
network acquisition and custom policies, but cannot bypass strict rejection of
known unsupported rendering inputs. SOPS and nested AI are not exposed. Secret
payload redaction precedes all detail projections. Neither profile is an OS
sandbox, and serialized artifact limits are not process heap limits.

## Consequences

Legacy CLI results remain compatible. New clients distinguish execution status,
render completeness, observed changes and deterministic check verdicts. Incomplete
operations do not publish authoritative handles. Changes and producer identity
survive cluster boundaries and serialization without rerendering.

Handles expire and are not persisted. CLI batches and MCP can use the same
workflow, but independent CLI processes cannot reuse handles. Hard execution
isolation, persistent caches, full controller parity and reverse source editing
remain separate features. A portable skill is distributed independently with an
explicit compatible fmp revision.
