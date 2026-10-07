// Package agent provides bounded, in-memory manifest analysis sessions.
// Core Secret payloads are always redacted. Credential-key redaction elsewhere
// is best effort, not a guarantee that arbitrary application data is nonsecret.
package agent

import (
	"encoding/json"
	"time"

	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	"github.com/tobiash/k8q/pkg/engine"
)

// Options are startup capabilities; repository configuration cannot grant trust.
type Options struct {
	Trusted bool
	// PluginCommands select executables only at startup. Local-only restrictions
	// still apply unless Trusted is set. Empty commands use the default Flux plugin.
	PluginCommands []plugin.Command
	// Crossplane enables the default Crossplane plugin; CrossplaneConfig contains
	// trusted runtime configuration, never repository-supplied execution grants.
	Crossplane       bool
	CrossplaneConfig json.RawMessage
	TTL              time.Duration
	MaxSnapshots     int
	// MaxBytes bounds the sum of serialized cached artifact lengths. Cache
	// payloads retain bytes only; temporary rendering/decoding allocations and
	// fixed per-handle metadata are not a process heap quota.
	MaxBytes int64
	Timeout  time.Duration
}

// Request is the version 1 operation envelope. Paths override configured roots.
type Request struct {
	Operation       string   `json:"operation"`
	Source          string   `json:"source,omitempty"`
	Base            string   `json:"base,omitempty"`
	Target          string   `json:"target,omitempty"`
	Paths           []string `json:"paths,omitempty"`
	Recursive       *bool    `json:"recursive,omitempty"`
	ID              string   `json:"id,omitempty"`
	BeforeID        string   `json:"beforeId,omitempty"`
	AfterID         string   `json:"afterId,omitempty"`
	ResourceID      string   `json:"resourceId,omitempty"`
	Query           Query    `json:"query,omitempty"`
	Offset          int      `json:"offset,omitempty"`
	Limit           int      `json:"limit,omitempty"`
	Fields          []string `json:"fields,omitempty"`
	MaxCPURequests  string   `json:"maxCPURequests,omitempty"`
	MaxMemRequests  string   `json:"maxMemRequests,omitempty"`
	MaxCPULimits    string   `json:"maxCPULimits,omitempty"`
	MaxMemLimits    string   `json:"maxMemLimits,omitempty"`
	RequireRequests bool     `json:"requireRequests,omitempty"`
	RequireLimits   bool     `json:"requireLimits,omitempty"`
}

// Query combines resource filters with AND semantics.
type Query struct {
	Cluster   string `json:"cluster,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Group     string `json:"group,omitempty"`
	Labels    string `json:"labels,omitempty"`
	Producer  string `json:"producer,omitempty"`
	Action    string `json:"action,omitempty"`
}

// Response separates operational failure from a successful check's verdict.
type Response struct {
	SchemaVersion string       `json:"schemaVersion"`
	Operation     string       `json:"operation"`
	Status        string       `json:"status"`
	Complete      bool         `json:"complete"`
	Data          any          `json:"data,omitempty"`
	Diagnostics   []Diagnostic `json:"diagnostics"`
	Error         *Error       `json:"error,omitempty"`
}

// Diagnostic is deliberately generic: underlying render errors may contain secrets.
type Diagnostic struct {
	Code     string `json:"code"`
	Phase    string `json:"phase"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// Error describes a public operational error, without chart or filesystem details.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Origin identifies a rendering producer without exposing its objects.
type Origin struct {
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Path      string `json:"path,omitempty"`
	Text      string `json:"text,omitempty"`
}

// ResourceSummary is an identity and provenance projection, never a full object.
type ResourceSummary struct {
	ResourceID   string  `json:"resourceId"`
	LogicalID    string  `json:"logicalId,omitempty"`
	Cluster      string  `json:"cluster"`
	APIVersion   string  `json:"apiVersion"`
	Kind         string  `json:"kind"`
	Name         string  `json:"name"`
	Namespace    string  `json:"namespace,omitempty"`
	Action       string  `json:"action,omitempty"`
	Producer     string  `json:"producer,omitempty"`
	BeforeOrigin *Origin `json:"beforeOrigin,omitempty"`
	AfterOrigin  *Origin `json:"afterOrigin,omitempty"`
}

// RenderData identifies one immutable retained snapshot.
type RenderData struct {
	ID            string `json:"id"`
	ResourceCount int    `json:"resourceCount"`
}

// Summary counts changes without rendering any object contents.
type Summary struct {
	Added    int `json:"added"`
	Modified int `json:"modified"`
	Deleted  int `json:"deleted"`
	Total    int `json:"total"`
}

// PreviewData identifies a diff and its retained snapshots.
type PreviewData struct {
	ID       string  `json:"id"`
	BeforeID string  `json:"beforeId"`
	AfterID  string  `json:"afterId"`
	Summary  Summary `json:"summary"`
	Changed  bool    `json:"changed"`
}

// QueryData is a bounded page; NextOffset is present exactly when more remain.
type QueryData struct {
	ID         string            `json:"id"`
	Items      []ResourceSummary `json:"items"`
	Total      int               `json:"total"`
	Offset     int               `json:"offset"`
	Truncated  bool              `json:"truncated"`
	NextOffset *int              `json:"nextOffset,omitempty"`
}

// InspectData contains redacted objects, or JSON-pointer/value maps with Fields.
type InspectData struct {
	ID string `json:"id"`
	ResourceSummary
	Old map[string]any `json:"old,omitempty"`
	New map[string]any `json:"new,omitempty"`
}

// CheckData returns a verdict, bounded counts and optional resource budgets.
type CheckData struct {
	ID                  string              `json:"id"`
	Verdict             string              `json:"verdict"`
	Count               *engine.CountResult `json:"count,omitempty"`
	Resources           *engine.SumResult   `json:"resources,omitempty"`
	ClassificationCount int                 `json:"classificationCount,omitempty"`
	ViolationCount      int                 `json:"violationCount,omitempty"`
}

// DiscoverData reports effective capabilities without rendering or fetching sources.
type DiscoverData struct {
	Operations       []string `json:"operations"`
	SchemaVersions   []string `json:"schemaVersions"`
	Profile          string   `json:"profile"`
	Paths            []string `json:"paths"`
	Recursive        bool     `json:"recursive"`
	Helm             bool     `json:"helm"`
	ConfigFound      bool     `json:"configFound"`
	DefaultPageSize  int      `json:"defaultPageSize"`
	MaxPageSize      int      `json:"maxPageSize"`
	MaxResponseBytes int      `json:"maxResponseBytes"`
}

// ReleaseData confirms removal of a session handle.
type ReleaseData struct {
	ID       string `json:"id"`
	Released bool   `json:"released"`
}
