package helm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	fmprender "github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"helm.sh/helm/v4/pkg/action"
	chartcommon "helm.sh/helm/v4/pkg/chart/common"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	helmcli "helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/registry"
	ri "helm.sh/helm/v4/pkg/release"
	"helm.sh/helm/v4/pkg/repo/v1"
	"sigs.k8s.io/kustomize/api/hasher"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/resource"
	"sigs.k8s.io/kustomize/kyaml/filesys"

	"gopkg.in/yaml.v3"
)

// Runner handles Helm chart downloading and rendering.
type Runner struct {
	settings  *helmcli.EnvSettings
	logger    logr.Logger
	localRoot string
}

type RenderTask struct {
	values          chartcommon.Values
	chart           string
	version         string
	repo            repo.Entry
	localChartPath  string
	localSourceRoot string
	releaseName     string
	namespace       string
	origin          fmprender.Provenance
	createNamespace bool
	skipCRDs        bool
	replace         bool
	disableHooks    bool
	includeCRDs     bool
	isOCI           bool
	postRenderer    PostRenderer
}

// SetLocalOnly requires existing local charts confined to root, with no acquisition.
func (r *Runner) SetLocalOnly(root string) {
	r.localRoot = root
}

// NewRunner creates a new Helm runner.
func NewRunner(settings *helmcli.EnvSettings, log logr.Logger) *Runner {
	return &Runner{
		settings: settings,
		logger:   log,
	}
}

// RenderCharts renders multiple charts in parallel.
// Charts that fail to render are collected as errors and skipped; partial results are returned.
func (r *Runner) RenderCharts(ctx context.Context, releases []RenderTask) (resmap.ResMap, []error, error) {
	res := resmap.New()

	type result struct {
		resources resmap.ResMap
		err       error
		task      RenderTask
	}
	results := make([]result, len(releases))

	var wg sync.WaitGroup
	// Bound active Helm work as well as the number of worker goroutines.
	workers := make(chan struct{}, 4)
	for i, h := range releases {
		select {
		case workers <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return nil, nil, ctx.Err()
		}
		i := i
		h := h
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-workers }()
			chartRes, err := r.renderChart(ctx, &h)
			results[i] = result{resources: chartRes, err: err, task: h}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}

	var errs []error
	for _, chartResult := range results {
		if chartResult.err != nil {
			r.logger.V(1).Info("skipping chart render", "chart", chartResult.task.chart, "namespace", chartResult.task.namespace, "error", chartResult.err)
			errs = append(errs, fmt.Errorf("%s: %w", helmReleaseProducer(chartResult.task), chartResult.err))
			continue
		}
		for _, res := range chartResult.resources.Resources() {
			annotations := res.GetAnnotations()
			if annotations == nil {
				annotations = make(map[string]string)
			}
			labels := res.GetLabels()
			if labels == nil {
				labels = make(map[string]string)
			}
			if chartResult.task.origin.Name != "" {
				labels["helm.toolkit.fluxcd.io/name"] = chartResult.task.origin.Name
				labels["helm.toolkit.fluxcd.io/namespace"] = chartResult.task.origin.Namespace
			} else if labels["helm.toolkit.fluxcd.io/name"] == "" {
				labels["helm.toolkit.fluxcd.io/name"] = chartResult.task.releaseName
				labels["helm.toolkit.fluxcd.io/namespace"] = chartResult.task.namespace
			}
			_ = res.SetLabels(labels)
			annotations[fmprender.ProducerAnnotation] = fmprender.HelmReleaseProvenance(labels["helm.toolkit.fluxcd.io/namespace"], labels["helm.toolkit.fluxcd.io/name"]).String()
			_ = res.SetAnnotations(annotations)
		}

		if err := absorbResMap(res, chartResult.resources, r.logger); err != nil {
			errs = append(errs, fmt.Errorf("absorbing resources for %s: %w", helmReleaseProducer(chartResult.task), err))
		}
	}
	return res, errs, nil
}

func helmReleaseProducer(task RenderTask) string {
	if task.origin.Name != "" {
		return task.origin.String()
	}
	if task.namespace != "" {
		return fmt.Sprintf("HelmRelease %s/%s", task.namespace, task.releaseName)
	}
	return fmt.Sprintf("HelmRelease %s", task.releaseName)
}

func (r *Runner) renderChart(ctx context.Context, t *RenderTask) (resmap.ResMap, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.localRoot != "" {
		if t.localChartPath == "" || t.isOCI || t.repo.URL != "" {
			return nil, fmt.Errorf("local-only: Helm requires an existing local chart; repository and OCI acquisition are disabled")
		}
		fs := filesys.MakeFsOnDisk()
		root := t.localSourceRoot
		if root == "" {
			root = r.localRoot
		}
		if err := fmprender.ValidateLocalPath(fs, r.localRoot, root); err != nil {
			return nil, err
		}
		if err := fmprender.ValidateLocalPath(fs, root, t.localChartPath); err != nil {
			return nil, err
		}
		if err := filepath.Walk(t.localChartPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				return fmt.Errorf("local-only: chart symlinks and special files are unsupported: %s", path)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	cfg := new(action.Configuration)
	driver := os.Getenv("HELM_DRIVER")
	if r.localRoot != "" {
		// In particular, never initialize the SQL driver from process environment.
		driver = "memory"
	}
	if err := cfg.Init(r.settings.RESTClientGetter(), t.namespace, driver); err != nil {
		return nil, fmt.Errorf("initializing helm configuration: %w", err)
	}

	install := action.NewInstall(cfg)
	install.DryRunStrategy = action.DryRunClient
	install.Namespace = t.namespace
	install.CreateNamespace = t.createNamespace
	install.ReleaseName = t.releaseName
	install.SkipCRDs = t.skipCRDs
	install.Replace = t.replace
	install.DisableHooks = t.disableHooks
	install.IncludeCRDs = t.includeCRDs

	var (
		chartRef string
		ch       *chart.Chart
	)
	if t.localChartPath != "" {
		loaded, err := loader.Load(t.localChartPath)
		if err != nil {
			return nil, fmt.Errorf("loading local chart %s: %w", t.localChartPath, err)
		}
		ch = loaded
		if r.localRoot != "" {
			if err := validateLocalChart(ch); err != nil {
				return nil, err
			}
		}
		r.logger.V(1).Info("loaded local chart", "chart", t.chart, "path", t.localChartPath)
	} else if t.isOCI {
		// For OCI charts, construct the full reference, skip repo index, and set up a registry client.
		chartRef = strings.TrimSuffix(t.repo.URL, "/") + "/" + t.chart
		install.Version = t.version
		regClient, err := registry.NewClient()
		if err != nil {
			return nil, fmt.Errorf("creating registry client: %w", err)
		}
		install.SetRegistryClient(regClient)
	} else {
		install.Version = t.version
		install.RepoURL = t.repo.URL
		install.Username = t.repo.Username
		install.Password = t.repo.Password
		install.CaFile = t.repo.CAFile
		install.CertFile = t.repo.CertFile
		install.InsecureSkipTLSVerify = t.repo.InsecureSkipTLSVerify
		install.PassCredentialsAll = t.repo.PassCredentialsAll
		install.KeyFile = t.repo.KeyFile
		chartRef = t.chart
	}
	if ch == nil {
		cp, err := install.LocateChart(chartRef, r.settings)
		if err != nil {
			return nil, fmt.Errorf("error locating chart: %w", err)
		}
		r.logger.V(1).Info("loaded chart from repo", "chart", t.chart, "repo", t.repo.Name, "url", t.repo.URL, "path", cp)
		loaded, err := loader.Load(cp)
		if err != nil {
			return nil, err
		}
		ch = loaded
	}
	rel, err := install.RunWithContext(ctx, ch, t.values)
	if err != nil {
		return nil, err
	}

	var manifests bytes.Buffer
	if rel != nil {
		acc, err := ri.NewAccessor(rel)
		if err != nil {
			return nil, fmt.Errorf("unexpected release type: %w", err)
		}
		fmt.Fprintln(&manifests, strings.TrimSpace(acc.Manifest()))
		if !install.DisableHooks {
			for _, h := range acc.Hooks() {
				ha, err := ri.NewHookAccessor(h)
				if err != nil {
					return nil, fmt.Errorf("reading hook: %w", err)
				}
				fmt.Fprintf(&manifests, "---\n# Source: %s\n%s\n", ha.Path(), ha.Manifest())
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	postRenderer := t.postRenderer
	if r.localRoot != "" {
		postRenderer, err = localPostRenderer(postRenderer)
		if err != nil {
			return nil, err
		}
	}
	renderedManifests, err := runPostRenderer(postRenderer, &manifests)
	if err != nil {
		return nil, fmt.Errorf("running post renderer: %w", err)
	}
	manifests = *renderedManifests
	resources, err := parseManifests(manifests.Bytes(), r.logger)
	if err != nil {
		return nil, err
	}
	applyNamespaceToRenderedResources(resources, t.namespace)
	return resources, nil
}

func applyNamespaceToRenderedResources(resources resmap.ResMap, namespace string) {
	if namespace == "" {
		return
	}
	clusterScopedGVKs := clusterScopedGVKsFromCRDs(resources)
	for _, res := range resources.Resources() {
		if !isClusterScopedRenderedResource(res, clusterScopedGVKs) && res.GetNamespace() == "" {
			_ = res.SetNamespace(namespace)
		}
	}
}

func isClusterScopedRenderedResource(res *resource.Resource, clusterScopedGVKs map[string]bool) bool {
	gvk := res.GetGvk()
	return gvk.IsClusterScoped() ||
		isKnownClusterScopedRenderedGVK(gvk.Group, gvk.Kind) ||
		clusterScopedGVKs[gvkScopeKey(gvk.Group, gvk.Kind)]
}

func isKnownClusterScopedRenderedGVK(group, kind string) bool {
	switch gvkScopeKey(group, kind) {
	case gvkScopeKey("snapshot.storage.k8s.io", "VolumeSnapshotClass"):
		return true
	default:
		return false
	}
}

func clusterScopedGVKsFromCRDs(resources resmap.ResMap) map[string]bool {
	clusterScopedGVKs := make(map[string]bool)
	for _, res := range resources.Resources() {
		gvk := res.GetGvk()
		if gvk.Group != "apiextensions.k8s.io" || gvk.Kind != "CustomResourceDefinition" {
			continue
		}

		obj, err := res.Map()
		if err != nil {
			continue
		}
		spec, ok := obj["spec"].(map[string]any)
		if !ok || spec["scope"] != "Cluster" {
			continue
		}
		names, ok := spec["names"].(map[string]any)
		if !ok {
			continue
		}
		group, groupOK := spec["group"].(string)
		kind, kindOK := names["kind"].(string)
		if groupOK && kindOK {
			clusterScopedGVKs[gvkScopeKey(group, kind)] = true
		}
	}
	return clusterScopedGVKs
}

func gvkScopeKey(group, kind string) string {
	return group + "/" + kind
}

func runPostRenderer(renderer PostRenderer, manifests *bytes.Buffer) (*bytes.Buffer, error) {
	if renderer == nil {
		return manifests, nil
	}
	return renderer.Run(manifests)
}

// parseManifests splits a multi-document YAML byte slice into individual
// resources. Invalid or duplicate resources fail the chart rather than disappearing.
func parseManifests(data []byte, log logr.Logger) (resmap.ResMap, error) {
	result := resmap.New()
	factory := resmap.NewFactory(resource.NewFactory(&hasher.Hasher{}))

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		var doc any
		if err := decoder.Decode(&doc); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("decoding manifest: %w", err)
		}
		if doc == nil {
			continue
		}
		if err := enc.Encode(doc); err != nil {
			return nil, fmt.Errorf("encoding manifest: %w", err)
		}
		if err := enc.Close(); err != nil {
			return nil, fmt.Errorf("closing encoder: %w", err)
		}
		if buf.Len() == 0 {
			continue
		}

		rm, err := factory.NewResMapFromBytes(buf.Bytes())
		if err != nil {
			return nil, fmt.Errorf("parsing manifest: %w", err)
		}
		if err := absorbResMap(result, rm, log); err != nil {
			return nil, fmt.Errorf("absorbing resources: %w", err)
		}
	}
	return result, nil
}

// absorbResMap merges src into dst, rejecting duplicate identities.
func absorbResMap(dst, src resmap.ResMap, log logr.Logger) error {
	for _, r := range src.Resources() {
		id := r.CurId()
		if _, err := dst.GetById(id); err == nil {
			return fmt.Errorf("duplicate resource %s", id)
		}
		if err := dst.Append(r); err != nil {
			return err
		}
	}
	return nil
}
