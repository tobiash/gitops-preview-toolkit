package helm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	fluxchartutil "github.com/fluxcd/pkg/chartutil"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/expander"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	chartcommon "helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/repo/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/resource"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/kustomize/kyaml/resid"
)

var (
	errSourcePending = errors.New("chart source not yet discovered")
	helmReleaseGVK   = resid.NewGvk("helm.toolkit.fluxcd.io", "v2", "HelmRelease")
	helmRepoGVK      = resid.NewGvk("source.toolkit.fluxcd.io", "v1", "HelmRepository")
	ociRepoGVK       = resid.NewGvk("source.toolkit.fluxcd.io", "v1", "OCIRepository")
	secretGVK        = resid.NewGvk("", "v1", "Secret")
	configMapGVK     = resid.NewGvk("", "v1", "ConfigMap")
)

// matchGVK reports true if the resource's GVK matches the target by group and kind.
// Version is ignored because Flux resources exist in multiple API versions,
// but the fields we read are compatible enough to decode into the current
// public API structs directly.
func matchGVK(resGvk resid.Gvk, target resid.Gvk) bool {
	return render.MatchGVK(resGvk, target)
}

type chartRunner interface {
	RenderCharts(ctx context.Context, releases []RenderTask) (resmap.ResMap, []error, error)
}

type chartSourceResolver interface {
	ResolvePath(namespace, name string) (string, bool)
}

// Expander implements the expander.Expander interface for Helm.
// It is safe for concurrent use -- each call to Expand operates on isolated state.
// The Expander tracks which releases have already been expanded to avoid
// duplicating resources across iterative expansion loops.
type Expander struct {
	runner       chartRunner
	resolver     chartSourceResolver
	logger       logr.Logger
	scheme       *runtime.Scheme
	seen         releaseTracker
	localRoot    string
	strictInputs bool
}

type releaseTracker struct {
	mu       sync.Mutex
	expanded map[string]bool
}

func (t *releaseTracker) claim(namespace, name string) bool {
	if t == nil {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.expanded == nil {
		t.expanded = make(map[string]bool)
	}
	key := namespace + "/" + name
	if t.expanded[key] {
		return false
	}
	t.expanded[key] = true
	return true
}

func (t *releaseTracker) contains(namespace, name string) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.expanded[namespace+"/"+name]
}

// expandState holds per-invocation state for a single Expand call.
type expandState struct {
	runner          chartRunner
	resolver        chartSourceResolver
	scheme          *runtime.Scheme
	render          *render.Render
	releases        []*helmv2.HelmRelease
	repositories    []*sourcev1.HelmRepository
	ociRepositories []*sourcev1.OCIRepository
	logger          logr.Logger
	localRoot       string
	strictInputs    bool
	seen            *releaseTracker
	deferredErrors  []error
}

// NewExpander creates a new Helm expander.
func NewExpander(runner *Runner, resolver chartSourceResolver, log logr.Logger) *Expander {
	sch := runtime.NewScheme()
	_ = scheme.AddToScheme(sch)
	return &Expander{
		runner:       runner,
		resolver:     resolver,
		logger:       log,
		scheme:       sch,
		localRoot:    runner.localRoot,
		strictInputs: runner.localRoot != "",
	}
}

// SetStrictInputs rejects known unsupported rendering inputs without restricting
// chart source acquisition. Local-only runners imply this validation as well.
func (e *Expander) SetStrictInputs() {
	e.strictInputs = true
}

// Expand implements expander.Expander. It parses HelmRelease and source resources
// from the render, then delegates chart rendering to the Runner.
func (e *Expander) Expand(ctx context.Context, r *render.Render) (*expander.ExpandResult, error) {

	s := &expandState{
		runner:       e.runner,
		resolver:     e.resolver,
		scheme:       e.scheme,
		render:       r,
		logger:       e.logger,
		localRoot:    e.localRoot,
		strictInputs: e.strictInputs,
		seen:         &e.seen,
	}

	// Parse resources from the render
	for _, res := range r.Resources() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		gvk := res.GetGvk()
		s.logger.V(1).Info("found manifest", "group", gvk.Group, "kind", gvk.Kind, "version", gvk.Version)

		if matchGVK(gvk, helmReleaseGVK) {
			release, err := s.parseHelmRelease(res)
			if err != nil {
				return nil, fmt.Errorf("error parsing HelmRelease: %w", err)
			}
			if e.seen.contains(release.Namespace, release.Name) {
				s.logger.V(1).Info("skipping already-expanded HelmRelease", "name", release.Name, "namespace", release.Namespace)
				continue
			}
			s.logger.V(1).Info("found helm release", "name", release.Name, "namespace", release.Namespace)
			s.releases = append(s.releases, release)
		} else if matchGVK(gvk, helmRepoGVK) {
			repo, err := s.parseRepository(res)
			if err != nil {
				return nil, fmt.Errorf("error parsing HelmRepository: %w", err)
			}
			s.logger.V(1).Info("found helm repository", "name", repo.Name, "namespace", repo.Namespace)
			s.repositories = append(s.repositories, repo)
		} else if matchGVK(gvk, ociRepoGVK) {
			repo, err := s.parseOCIRepository(res)
			if err != nil {
				return nil, fmt.Errorf("error parsing OCIRepository: %w", err)
			}
			s.logger.V(1).Info("found oci repository", "name", repo.Name, "namespace", repo.Namespace)
			s.ociRepositories = append(s.ociRepositories, repo)
		}
	}

	resources, errs, err := s.renderAllCharts(ctx)
	if err != nil {
		return nil, err
	}
	return &expander.ExpandResult{Resources: resources, Errors: errs, DeferredErrors: s.deferredErrors}, nil
}

func (s *expandState) parseHelmRelease(res *resource.Resource) (*helmv2.HelmRelease, error) {
	if s.strictInputs {
		m, err := res.Map()
		if err != nil {
			return nil, err
		}
		postRenderers, _, err := unstructured.NestedSlice(m, "spec", "postRenderers")
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(postRenderers)
		if err != nil {
			return nil, err
		}
		var parsed []helmv2.PostRenderer
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&parsed); err != nil {
			return nil, fmt.Errorf("strict inputs: unsupported Helm postrenderer: %w", err)
		}
	}
	hr, err := decodeResource[helmv2.HelmRelease](res)
	if err != nil {
		return nil, err
	}
	if hr.Spec.Chart == nil {
		return nil, fmt.Errorf("HelmRelease %s/%s has no spec.chart", hr.Namespace, hr.Name)
	}
	// Legacy previews retain the shipped behavior; strict snapshots reject
	// known unsupported chart inputs instead of claiming an authoritative render.
	if s.strictInputs {
		if len(hr.Spec.Chart.Spec.ValuesFiles) > 0 || hr.Spec.Chart.Spec.IgnoreMissingValuesFiles {
			return nil, fmt.Errorf("strict inputs: HelmRelease %s/%s chart.spec.valuesFiles and ignoreMissingValuesFiles are unsupported", hr.Namespace, hr.Name)
		}
		if hr.Spec.ChartRef != nil {
			return nil, fmt.Errorf("strict inputs: HelmRelease %s/%s spec.chartRef is unsupported", hr.Namespace, hr.Name)
		}
	}
	return hr, nil
}

func (s *expandState) parseRepository(res *resource.Resource) (*sourcev1.HelmRepository, error) {
	repo, err := decodeResource[sourcev1.HelmRepository](res)
	if err != nil {
		return nil, err
	}
	return repo, nil
}

func (s *expandState) parseOCIRepository(res *resource.Resource) (*sourcev1.OCIRepository, error) {
	repo, err := decodeResource[sourcev1.OCIRepository](res)
	if err != nil {
		return nil, err
	}
	return repo, nil
}
func (s *expandState) renderAllCharts(ctx context.Context) (resmap.ResMap, []error, error) {
	var tasks []RenderTask
	var skipErrs []error
	valuesClient, err := s.newValuesClient()
	if err != nil {
		return nil, nil, fmt.Errorf("building values client: %w", err)
	}
	for _, h := range s.releases {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if s.seen.contains(h.Namespace, h.Name) {
			continue
		}
		src, err := s.findChartURL(h)
		if err != nil {
			issue := fmt.Errorf("HelmRelease %s/%s: %w", h.Namespace, h.Name, err)
			if errors.Is(err, errSourcePending) {
				s.deferredErrors = append(s.deferredErrors, issue)
			} else if s.seen.claim(h.Namespace, h.Name) {
				skipErrs = append(skipErrs, issue)
			}
			continue
		}
		values, err := s.composeValues(ctx, valuesClient, h)
		if err != nil {
			issue := fmt.Errorf("error composing values for %s/%s: %w", h.Namespace, h.Name, err)
			if errors.Is(err, fluxchartutil.ErrResourceNotFound) {
				s.deferredErrors = append(s.deferredErrors, issue)
			} else if s.seen.claim(h.Namespace, h.Name) {
				skipErrs = append(skipErrs, issue)
			}
			continue
		}
		// Claim only ready tasks (or permanent errors above). Missing sources
		// are retried as discovery adds paths and Helm-generated resources.
		if !s.seen.claim(h.Namespace, h.Name) {
			continue
		}

		chartName := h.Spec.Chart.Spec.Chart
		chartVersion := h.Spec.Chart.Spec.Version
		releaseName := h.Spec.ReleaseName
		if releaseName == "" {
			releaseName = h.Name
		}
		namespace := h.Spec.TargetNamespace
		if namespace == "" {
			namespace = h.Namespace
		}

		install := h.GetInstall()
		postRenderer := buildPostRenderers(h)
		tasks = append(tasks, RenderTask{
			values:          values,
			chart:           chartName,
			version:         chartVersion,
			repo:            repo.Entry{URL: src.url, Name: fmt.Sprintf("%s-%s", h.Namespace, h.Name)},
			localChartPath:  src.localPath,
			localSourceRoot: src.localRoot,
			releaseName:     releaseName,
			namespace:       namespace,
			origin:          render.HelmReleaseProvenance(h.Namespace, h.Name),
			skipCRDs:        install.SkipCRDs,
			replace:         install.Replace,
			disableHooks:    install.DisableHooks,
			createNamespace: install.CreateNamespace,
			isOCI:           src.isOCI,
			postRenderer:    postRenderer,
		})
	}

	if len(tasks) == 0 {
		return resmap.New(), skipErrs, nil
	}
	resources, renderErrs, err := s.runner.RenderCharts(ctx, tasks)
	if err != nil {
		return nil, nil, err
	}
	return resources, append(skipErrs, renderErrs...), nil
}

func (s *expandState) composeValues(ctx context.Context, valuesClient ctrlclient.Client, hr *helmv2.HelmRelease) (chartcommon.Values, error) {
	inlineValues := hr.GetValues()
	if len(hr.Spec.ValuesFrom) == 0 {
		return inlineValues, nil
	}
	values, err := fluxchartutil.ChartValuesFromReferences(
		ctx,
		s.logger.WithValues("release", hr.Name, "namespace", hr.Namespace),
		valuesClient,
		hr.Namespace,
		inlineValues,
		hr.Spec.ValuesFrom...,
	)
	if err != nil {
		return nil, err
	}
	return values, nil
}

// chartSource holds the resolved chart location and whether it's an OCI reference.
type chartSource struct {
	url       string
	isOCI     bool
	localPath string
	localRoot string
}

func (s *expandState) findChartURL(source *helmv2.HelmRelease) (chartSource, error) {
	if source.Spec.Chart == nil {
		return chartSource{}, fmt.Errorf("HelmRelease %s/%s has no chart.spec.sourceRef", source.Namespace, source.Name)
	}
	sourceRef := source.Spec.Chart.Spec.SourceRef
	namespace := sourceRef.Namespace
	if namespace == "" {
		namespace = source.Namespace
	}
	name := sourceRef.Name
	kind := sourceRef.Kind

	switch kind {
	case "HelmRepository":
		for _, hr := range s.repositories {
			if hr.Namespace == namespace && hr.Name == name {
				return chartSource{url: hr.Spec.URL, isOCI: hr.Spec.Type == "oci"}, nil
			}
		}
	case "OCIRepository":
		for _, oci := range s.ociRepositories {
			if oci.Namespace == namespace && oci.Name == name {
				return chartSource{url: oci.Spec.URL, isOCI: true}, nil
			}
		}
	case "GitRepository":
		if s.resolver == nil {
			return chartSource{}, fmt.Errorf("GitRepository chart source requires --resolve-git")
		}
		baseDir, ok := s.resolver.ResolvePath(namespace, name)
		if !ok {
			return chartSource{}, fmt.Errorf("unresolved GitRepository %s/%s: %w", namespace, name, errSourcePending)
		}
		chartPath := strings.TrimPrefix(source.Spec.Chart.Spec.Chart, "./")
		if chartPath == "" {
			return chartSource{}, fmt.Errorf("HelmRelease %s/%s has no chart.spec.chart path", source.Namespace, source.Name)
		}
		full := filepath.Join(baseDir, chartPath)
		if s.localRoot != "" {
			if filepath.IsAbs(chartPath) {
				return chartSource{}, fmt.Errorf("local-only: absolute chart paths are unsupported")
			}
			if err := render.ValidateLocalPath(filesys.MakeFsOnDisk(), s.localRoot, baseDir); err != nil {
				return chartSource{}, err
			}
			if err := render.ValidateLocalPath(filesys.MakeFsOnDisk(), baseDir, full); err != nil {
				return chartSource{}, err
			}
		}
		return chartSource{localPath: full, localRoot: baseDir}, nil
	default:
		return chartSource{}, fmt.Errorf("unsupported source kind '%s'", kind)
	}
	return chartSource{}, fmt.Errorf("unable to find source '%s': %w", name, errSourcePending)
}

func decodeResource[T any](res *resource.Resource) (*T, error) {
	var out T
	m, err := res.Map()
	if err != nil {
		return nil, err
	}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (s *expandState) newValuesClient() (ctrlclient.Client, error) {
	objects := make([]ctrlclient.Object, 0)
	for _, res := range s.render.Resources() {
		switch res.GetGvk() {
		case configMapGVK:
			var cm corev1.ConfigMap
			if err := s.convertResource(res, &cm); err != nil {
				return nil, err
			}
			objects = append(objects, &cm)
		case secretGVK:
			var secret corev1.Secret
			if err := s.convertResource(res, &secret); err != nil {
				return nil, err
			}
			objects = append(objects, &secret)
		}
	}
	return fake.NewClientBuilder().WithScheme(s.scheme).WithObjects(objects...).Build(), nil
}

func (s *expandState) convertResource(res *resource.Resource, to any) error {
	m, err := res.Map()
	if err != nil {
		return err
	}
	var u unstructured.Unstructured
	u.SetUnstructuredContent(m)
	return s.scheme.Convert(&u, to, nil)
}
