package fluxks

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	fluxksv1 "github.com/fluxcd/kustomize-controller/api/v1"
	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/expander"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/kustomize/api/resource"
	"sigs.k8s.io/kustomize/kyaml/resid"
)

var fluxKSGVK = resid.NewGvk("kustomize.toolkit.fluxcd.io", "v1", "Kustomization")

// SourceResolver resolves GitRepository source references to local paths.
type SourceResolver interface {
	ResolvePath(namespace, name string) (string, bool)
}

// Expander discovers Flux Kustomization CRs in the resource set and returns
// their spec.path values as DiscoveredPaths for the next iteration of the
// expansion loop.
type Expander struct {
	log          logr.Logger
	resolver     SourceResolver
	strictInputs bool
}

// NewExpander creates a Flux Kustomization expander.
func NewExpander(log logr.Logger) *Expander {
	return &Expander{log: log}
}

// NewExpanderWithResolver creates a Flux Kustomization expander with GitRepository resolution.
func NewExpanderWithResolver(log logr.Logger, resolver SourceResolver) *Expander {
	return &Expander{log: log, resolver: resolver}
}

// SetStrictInputs rejects known rendering inputs not implemented by the expander.
// It does not change source resolution or filesystem access.
func (e *Expander) SetStrictInputs() {
	e.strictInputs = true
}

func (e *Expander) Expand(ctx context.Context, r *render.Render) (*expander.ExpandResult, error) {
	var paths []expander.DiscoveredPath
	var errs []error
	var deferredErrors []error

	for _, res := range r.Resources() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		gvk := res.GetGvk()
		if !render.MatchGVK(gvk, fluxKSGVK) {
			continue
		}

		ks, err := decodeKustomization(res)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if e.strictInputs {
			if err := unsupportedInputs(ks); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		path := ks.Spec.Path
		path = strings.TrimPrefix(path, "./")
		path = filepath.Clean(path)

		dp := expander.DiscoveredPath{
			Path:     path,
			Producer: fmt.Sprintf("Kustomization %s/%s", ks.Namespace, ks.Name),
		}
		if ks.Spec.SourceRef.Kind != "" && ks.Spec.SourceRef.Kind != "GitRepository" {
			errs = append(errs, fmt.Errorf("%s: unsupported source kind %q", dp.Producer, ks.Spec.SourceRef.Kind))
			continue
		}

		if ks.Spec.TargetNamespace != "" {
			dp.Namespace = ks.Spec.TargetNamespace
		}
		if ks.Spec.PostBuild != nil && len(ks.Spec.PostBuild.Substitute) > 0 {
			dp.Substitutions = make(map[string]string, len(ks.Spec.PostBuild.Substitute))
			for key, value := range ks.Spec.PostBuild.Substitute {
				dp.Substitutions[key] = value
			}
		}

		if e.resolver != nil && ks.Spec.SourceRef.Kind == "GitRepository" {
			ns := ks.Spec.SourceRef.Namespace
			if ns == "" {
				ns = ks.Namespace
			}
			if baseDir, ok := e.resolver.ResolvePath(ns, ks.Spec.SourceRef.Name); ok {
				dp.BaseDir = baseDir
			} else {
				deferredErrors = append(deferredErrors, fmt.Errorf("%s: unresolved GitRepository %s/%s", dp.Producer, ns, ks.Spec.SourceRef.Name))
				continue
			}
		}

		e.log.V(1).Info("discovered Flux Kustomization path",
			"path", dp.Path, "baseDir", dp.BaseDir, "name", ks.Name, "namespace", ks.Namespace)
		paths = append(paths, dp)
	}

	return &expander.ExpandResult{DiscoveredPaths: paths, Errors: errs, DeferredErrors: deferredErrors}, nil
}

func unsupportedInputs(ks *fluxksv1.Kustomization) error {
	for _, input := range []struct {
		field string
		used  bool
	}{
		{"postBuild.substituteFrom", ks.Spec.PostBuild != nil && len(ks.Spec.PostBuild.SubstituteFrom) > 0},
		{"patches", len(ks.Spec.Patches) > 0},
		{"images", len(ks.Spec.Images) > 0},
		{"components", len(ks.Spec.Components) > 0},
		{"namePrefix", ks.Spec.NamePrefix != ""},
		{"nameSuffix", ks.Spec.NameSuffix != ""},
		{"commonMetadata", ks.Spec.CommonMetadata != nil},
		{"decryption", ks.Spec.Decryption != nil},
	} {
		if input.used {
			return fmt.Errorf("strict inputs: Kustomization %s/%s spec.%s is unsupported", ks.Namespace, ks.Name, input.field)
		}
	}
	return nil
}

func decodeKustomization(res *resource.Resource) (*fluxksv1.Kustomization, error) {
	var ks fluxksv1.Kustomization
	m, err := res.Map()
	if err != nil {
		return nil, fmt.Errorf("reading resource map: %w", err)
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &ks); err != nil {
		return nil, fmt.Errorf("decoding Kustomization %s/%s: %w", res.GetNamespace(), res.GetName(), err)
	}
	return &ks, nil
}
