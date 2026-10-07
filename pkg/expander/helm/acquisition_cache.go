package helm

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-logr/logr"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/registry"
	"helm.sh/helm/v4/pkg/repo/v1"
	"sigs.k8s.io/kustomize/api/resmap"
)

// AcquisitionCache pins chart selectors to immutable archives within one run,
// never rendered output or local trees. Create a new cache for each resolution
// epoch and Close it after rendering has stopped. Reusing a selector cache across
// independent runs would freeze mutable tags and version ranges indefinitely.
type AcquisitionCache struct {
	mu     sync.Mutex
	root   string
	paths  map[string]string
	closed bool
}

func NewAcquisitionCache() (*AcquisitionCache, error) {
	root, err := os.MkdirTemp("", "fmp-helm-acquisition-*")
	if err != nil {
		return nil, err
	}
	return &AcquisitionCache{root: root, paths: map[string]string{}}, nil
}

func (c *AcquisitionCache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return os.RemoveAll(c.root)
}

// NewExpanderWithAcquisitionCache uses a shared archive cache while keeping the
// release tracker private to this expander. Local-only runners bypass acquisition.
func NewExpanderWithAcquisitionCache(
	runner *Runner,
	resolver chartSourceResolver,
	log logr.Logger,
	cache *AcquisitionCache,
) *Expander {
	e := NewExpander(runner, resolver, log)
	e.runner = &acquisitionRunner{runner: runner, cache: cache}
	return e
}

type acquisitionRunner struct {
	runner *Runner
	cache  *AcquisitionCache
}

func (r *acquisitionRunner) RenderCharts(ctx context.Context, tasks []RenderTask) (resmap.ResMap, []error, error) {
	ready := make([]RenderTask, 0, len(tasks))
	errs := []error{}
	for _, task := range tasks {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		remote := task.repo.URL != "" || task.isOCI
		if remote && task.localChartPath == "" && r.runner.localRoot == "" {
			path, err := r.cache.acquire(ctx, r.runner, task)
			if err != nil {
				if ctx.Err() != nil {
					return nil, nil, ctx.Err()
				}
				errs = append(errs, fmt.Errorf("%s: %w", helmReleaseProducer(task), err))
				continue
			}
			task.localChartPath = path
		}
		ready = append(ready, task)
	}
	resources, renderErrors, err := r.runner.RenderCharts(ctx, ready)
	return resources, append(errs, renderErrors...), err
}

func (c *AcquisitionCache) acquire(ctx context.Context, runner *Runner, task RenderTask) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if c.closed {
		return "", fmt.Errorf("helm acquisition cache is closed")
	}
	// Release names, namespaces, values and postrenderers do not participate in
	// acquisition. Source credentials and trusted Helm settings do.
	source := task.repo
	source.Name = ""
	keyData, err := json.Marshal(struct {
		Chart            string
		Version          string
		Source           repo.Entry
		OCI              bool
		RegistryConfig   string
		RepositoryConfig string
		RepositoryCache  string
	}{
		Chart: task.chart, Version: task.version, Source: source, OCI: task.isOCI,
		RegistryConfig:   runner.settings.RegistryConfig,
		RepositoryConfig: runner.settings.RepositoryConfig,
		RepositoryCache:  runner.settings.RepositoryCache,
	})
	if err != nil {
		return "", err
	}
	key := fmt.Sprintf("%x", sha256.Sum256(keyData))
	if path, ok := c.paths[key]; ok {
		return path, nil
	}

	install := action.NewInstall(new(action.Configuration))
	install.Version = task.version
	ref := task.chart
	if task.isOCI {
		ref = strings.TrimSuffix(task.repo.URL, "/") + "/" + task.chart
		client, err := registry.NewClient(registry.ClientOptCredentialsFile(runner.settings.RegistryConfig))
		if err != nil {
			return "", err
		}
		install.SetRegistryClient(client)
	} else {
		install.RepoURL = task.repo.URL
		install.Username, install.Password = task.repo.Username, task.repo.Password
		install.CaFile, install.CertFile, install.KeyFile = task.repo.CAFile, task.repo.CertFile, task.repo.KeyFile
		install.InsecureSkipTLSVerify = task.repo.InsecureSkipTLSVerify
		install.PassCredentialsAll = task.repo.PassCredentialsAll
	}
	// LocateChart does not expose a context. Preserve the existing runner's
	// acquisition behavior, but never publish a cache entry after cancellation.
	path, err := install.LocateChart(ref, runner.settings)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	destination := filepath.Join(c.root, key+".tgz")
	if err := os.WriteFile(destination, data, 0o600); err != nil {
		return "", err
	}
	c.paths[key] = destination
	return destination, nil
}
