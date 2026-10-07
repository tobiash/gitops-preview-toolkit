# GitOps Preview Toolkit

GitOps Preview Toolkit explains the rendered Kubernetes resource impact of supported GitOps changes before they reach a cluster. Its canonical repository and Go module are `github.com/tobiash/gitops-preview-toolkit`; `gitops-preview` is the primary executable and `fmp` is the compatibility executable.

The toolkit implements this workflow through persistent local render
plugins. A combined Flux/Git/Helm/path plugin and an optional Crossplane plugin
produce desired resources; the host retains comparison, policy, AI Assessment,
agent operations and report workflows.

**Logical Resource Identity** identifies an unnamed composed output by its parent
and stable composition output key. It is preview metadata, not a generated live
Kubernetes name. **Evaluation Evidence**, including composite status and readiness,
is separate from desired resource inventory. Rendering is bounded desired-state
discovery, not arbitrary cyclic controller reconciliation.

## Language

**Classification**:
A review finding assigned to a rendered manifest change, with `policy` or `ai` provenance indicating whether it came from a deterministic policy or an AI assessment.
_Avoid_: Risk, label, violation

**AI Assessment**:
An optional generated review aid that summarizes manifest impact and may add AI-provenance classifications.
_Avoid_: AI policy, automated gate

**Policy Classification**:
A deterministic classification produced by configured policy rules.
_Avoid_: AI classification

## Relationships

- An **AI Assessment** may add zero or more **Classifications**, constrained by an allowlist only when one is configured
- A **Policy Classification** is a **Classification** with policy provenance
- A **Classification** is distinct from a **Violation** and does not itself fail a review

## Example dialogue

> **Dev:** "Can this PR get an AI classification and still keep policy labels deterministic?"
> **Domain expert:** "Yes — both are Classifications, but provenance tells consumers whether a finding came from policy or an AI Assessment."

## Flagged ambiguities

- "classification" was used for both deterministic policy output and AI-generated review output — resolved: keep one **Classification** concept and add provenance.
