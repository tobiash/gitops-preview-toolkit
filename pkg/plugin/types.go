// Package plugin defines the engine-independent contract for local render plugins.
package plugin

import (
	"context"
	"encoding/json"
)

const ProtocolVersion = 1

// Resource is desired YAML and its stable preview identity. Logical identities
// identify unnamed composition outputs without inventing Kubernetes names.
type Resource struct {
	ID         string     `json:"id"`
	YAML       string     `json:"yaml"`
	Logical    bool       `json:"logical,omitempty"`
	Provenance Provenance `json:"provenance"`
}

type Provenance struct {
	Kind      string `json:"kind,omitempty"`
	Name      string `json:"name,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Path      string `json:"path,omitempty"`
	Text      string `json:"text,omitempty"`
}

// Expansion is a complete replacement of one producer's current desired output.
// Trigger is an inventory identity, or "root" for the configured source roots.
type Expansion struct {
	ID        string     `json:"id"`
	Trigger   string     `json:"trigger"`
	Resources []Resource `json:"resources"`
}

type Diagnostic struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Severity   string `json:"severity"`
	ResourceID string `json:"resourceId,omitempty"`
}

type DescribeRequest struct {
	ProtocolVersion int `json:"protocolVersion"`
}
type DescribeResponse struct {
	ProtocolVersion int    `json:"protocolVersion"`
	Name            string `json:"name"`
	Version         string `json:"version"`
}

// OpenRequest is trusted invocation configuration, never an instruction derived
// from Kubernetes annotations. A session belongs to one cluster and evaluation.
type OpenRequest struct {
	RunID        string          `json:"runId,omitempty"`
	Root         string          `json:"root"`
	Paths        []string        `json:"paths"`
	Recursive    bool            `json:"recursive"`
	LocalOnly    bool            `json:"localOnly"`
	StrictInputs bool            `json:"strictInputs"`
	Cluster      string          `json:"cluster,omitempty"`
	Fresh        bool            `json:"fresh,omitempty"`
	Config       json.RawMessage `json:"config,omitempty"`
}
type OpenResponse struct {
	Session string `json:"session"`
}
type ExpandRequest struct {
	Session   string     `json:"session"`
	Resources []Resource `json:"resources"`
}

// ExpandResponse exhaustively describes all producers and current diagnostics.
// Evidence is informational and must never be fed back as observed cluster state.
type ExpandResponse struct {
	Expansions  []Expansion       `json:"expansions"`
	Diagnostics []Diagnostic      `json:"diagnostics"`
	Evidence    []json.RawMessage `json:"evidence,omitempty"`
}
type CloseRequest struct {
	Session string `json:"session"`
}
type CloseResponse struct{}

// Service is implemented by engines and by the persistent gRPC client Adapter.
type Service interface {
	Describe(context.Context, *DescribeRequest) (*DescribeResponse, error)
	OpenRender(context.Context, *OpenRequest) (*OpenResponse, error)
	Expand(context.Context, *ExpandRequest) (*ExpandResponse, error)
	CloseRender(context.Context, *CloseRequest) (*CloseResponse, error)
}

// Command selects an executable from trusted startup configuration.
type Command struct {
	Name    string          `json:"name" yaml:"name"`
	Command string          `json:"command" yaml:"command"`
	Args    []string        `json:"args,omitempty" yaml:"args,omitempty"`
	Config  json.RawMessage `json:"config,omitempty" yaml:"-"`
}
