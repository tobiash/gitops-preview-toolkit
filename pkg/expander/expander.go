package expander

import (
	"context"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"sigs.k8s.io/kustomize/api/resmap"
)

// DiscoveredPath represents a path to render, optionally relative to a
// different base directory (e.g. a cloned GitRepository).
type DiscoveredPath struct {
	// Path relative to BaseDir (or the repo root if BaseDir is empty).
	Path string
	// BaseDir is the root directory for Path resolution.
	// If empty, Path is resolved against the main repo root.
	BaseDir string
	// Namespace overrides the namespace for all namespace-scoped resources
	// rendered from this path (e.g. Flux Kustomization spec.targetNamespace).
	Namespace string
	// Producer describes the Flux object or path that caused this path to be rendered.
	Producer string
	// Substitutions contains Flux postBuild inline substitutions for this path.
	Substitutions map[string]string
}

// ExpandResult holds the output of an expander.
type ExpandResult struct {
	// Resources contains additional resources produced by the expander.
	Resources resmap.ResMap
	// DiscoveredPaths contains new paths that should be rendered.
	DiscoveredPaths []DiscoveredPath
	// Errors contains non-fatal errors encountered during expansion
	// (e.g. a HelmRelease whose chart could not be resolved).
	// These do not stop expansion but should be surfaced to the user.
	Errors []error
	// DeferredErrors contains unresolved dependencies that may be discovered in
	// later iterations. Expanders must recompute these on every call; discovery
	// promotes only those remaining at the fixed point to Errors.
	DeferredErrors []error
}

// Expander is the interface that each resource expander must implement.
type Expander interface {
	Expand(ctx context.Context, r *render.Render) (*ExpandResult, error)
}

// Registry holds a collection of expanders and runs them in order.
type Registry struct {
	expanders []Expander
	log       logr.Logger
}

// NewRegistry creates a new empty expander registry.
func NewRegistry(log logr.Logger) *Registry {
	return &Registry{
		log: log,
	}
}

// Register adds an expander to the registry.
func (r *Registry) Register(e Expander) {
	r.expanders = append(r.expanders, e)
}

// Expand runs all registered expanders and accumulates results.
func (r *Registry) Expand(ctx context.Context, render *render.Render) (*ExpandResult, error) {
	result := &ExpandResult{Resources: resmap.New()}
	for i, e := range r.expanders {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r.log.V(1).Info("running expander", "index", i)
		expanded, err := e.Expand(ctx, render)
		if err != nil {
			return nil, fmt.Errorf("expander %d failed: %w", i, err)
		}
		if expanded.Resources != nil {
			if err := result.Resources.AbsorbAll(expanded.Resources); err != nil {
				return nil, fmt.Errorf("expander %d absorb failed: %w", i, err)
			}
		}
		result.DiscoveredPaths = append(result.DiscoveredPaths, expanded.DiscoveredPaths...)
		result.Errors = append(result.Errors, expanded.Errors...)
		result.DeferredErrors = append(result.DeferredErrors, expanded.DeferredErrors...)
	}
	return result, nil
}
