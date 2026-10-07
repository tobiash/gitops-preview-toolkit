package preview

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"github.com/tobiash/gitops-preview-toolkit/pkg/sops"
	"sigs.k8s.io/kustomize/api/resource"
	"sigs.k8s.io/kustomize/kyaml/yaml"
)

// loadRepoResult keeps unnamed logical outputs outside the named ResMap.
type loadRepoResult struct {
	render   *render.Render
	logical  []plugin.Resource
	evidence []json.RawMessage
	errors   []error
	warnings []error
}

func (p *Preview) loadRepo(ctx context.Context, path string) (map[string]*loadRepoResult, error) {
	if !p.runActive {
		p.runID = rand.Text()
	}
	return p.loadRepoOptions(ctx, path, false)
}

func (p *Preview) loadRepoOptions(ctx context.Context, path string, freshLoads bool) (map[string]*loadRepoResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.isClustered() {
		results := make(map[string]*loadRepoResult, len(p.clusterPaths))
		clusters := make([]string, 0, len(p.clusterPaths))
		for cluster := range p.clusterPaths {
			clusters = append(clusters, cluster)
		}
		// Stable cluster order also stabilizes plugin acquisition and diagnostics.
		sort.Strings(clusters)
		for _, cluster := range clusters {
			result, err := p.loadRepoPaths(ctx, path, p.clusterPaths[cluster], cluster, freshLoads)
			if err != nil {
				return nil, err
			}
			results[cluster] = result
		}
		return results, nil
	}
	result, err := p.loadRepoPaths(ctx, path, p.paths, "", freshLoads)
	if err != nil {
		return nil, err
	}
	return map[string]*loadRepoResult{"": result}, nil
}

func (p *Preview) loadRepoPaths(
	ctx context.Context, path string, paths []string, cluster string, freshLoads bool,
) (*loadRepoResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no render roots configured: specify --path or configure paths in .fmp.yaml")
	}
	root, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if p.host == nil {
		return nil, fmt.Errorf("preview has no plugin host")
	}
	var sessionConfig json.RawMessage
	if p.borrowedHost {
		sessionConfig, err = p.sessionConfig()
		if err != nil {
			return nil, err
		}
	}
	result, err := p.host.Render(ctx, plugin.OpenRequest{
		RunID: p.runID,
		Root:  root, Paths: paths, Cluster: cluster, Recursive: p.recursive,
		LocalOnly: p.localOnly, StrictInputs: p.strictInputs, Fresh: freshLoads,
		Config: sessionConfig,
	})
	if err != nil {
		return nil, fmt.Errorf("cluster %q: plugin rendering failed: %w", cluster, err)
	}
	if result == nil {
		return nil, fmt.Errorf("cluster %q: plugin host returned no result", cluster)
	}
	loaded := &loadRepoResult{render: render.NewDefaultRender(p.log), evidence: result.Evidence}
	for _, diagnostic := range result.Diagnostics {
		issue := fmt.Errorf("%s", diagnostic.Message)
		if strings.EqualFold(diagnostic.Severity, "error") {
			loaded.errors = append(loaded.errors, issue)
			// Duplicate discovery was historically a warning promoted to an
			// incompleteness error. Preserve both diagnostic projections.
			if diagnostic.Code == "duplicate-resource" {
				loaded.warnings = append(loaded.warnings, issue)
			}
		} else {
			loaded.warnings = append(loaded.warnings, issue)
		}
	}
	if !result.Complete && len(loaded.errors) == 0 {
		loaded.errors = append(loaded.errors, fmt.Errorf("plugin render is incomplete"))
	}
	factory := resource.NewFactory(nil)
	for _, output := range result.Resources {
		if output.Logical {
			if _, err := plugin.Object(output); err != nil {
				loaded.errors = append(loaded.errors, fmt.Errorf("decoding logical resource %q: %w", output.ID, err))
				continue
			}
			loaded.logical = append(loaded.logical, output)
			continue
		}
		res, err := factory.FromBytes([]byte(output.YAML))
		if err != nil {
			loaded.errors = append(loaded.errors, fmt.Errorf("decoding plugin resource %q: %w", output.ID, err))
			continue
		}
		if res.GetName() == "" {
			loaded.errors = append(loaded.errors, fmt.Errorf("named plugin resource %q has no metadata.name", output.ID))
			continue
		}
		if err := loaded.render.Append(res); err != nil {
			loaded.errors = append(loaded.errors, fmt.Errorf("appending plugin resource %q: %w", output.ID, err))
			continue
		}
		origin := output.Provenance
		loaded.render.SetProvenance(res.CurId(), render.Provenance{
			Kind: origin.Kind, Name: origin.Name, Namespace: origin.Namespace, Path: origin.Path, Text: origin.Text,
		})
	}
	if p.filters != nil && loaded.render.Size() > 0 {
		for _, filter := range p.filters.Filters {
			if err := loaded.render.ApplyFilter(filter.Filter); err != nil {
				return nil, err
			}
		}
	}
	if p.sopsDecrypt {
		if err := sops.DecryptResources(loaded.render); err != nil {
			return nil, fmt.Errorf("cluster %q: sops decryption failed: %w", cluster, err)
		}
	}
	loaded.logical, err = p.filterLogical(loaded.logical)
	if err != nil {
		return nil, fmt.Errorf("cluster %q: %w", cluster, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return loaded, nil
}

func (r *loadRepoResult) asYAML() ([]byte, error) {
	named, err := r.render.AsYaml()
	if err != nil {
		return nil, err
	}
	var output strings.Builder
	output.Write(named)
	for _, logical := range r.logical {
		if output.Len() > 0 {
			output.WriteString("\n---\n")
		}
		output.WriteString(logical.YAML)
		if !strings.HasSuffix(logical.YAML, "\n") {
			output.WriteByte('\n')
		}
	}
	return []byte(output.String()), nil
}

func (r *loadRepoResult) objects() ([]map[string]any, error) {
	objects := make([]map[string]any, 0, r.render.Size()+len(r.logical))
	for i, resource := range r.render.Resources() {
		object, err := resource.Map()
		if err != nil {
			return nil, fmt.Errorf("resource %d: %w", i+1, err)
		}
		objects = append(objects, object)
	}
	for _, resource := range r.logical {
		object, err := plugin.Object(resource)
		if err != nil {
			return nil, fmt.Errorf("logical resource %q: %w", resource.ID, err)
		}
		objects = append(objects, object)
	}
	return objects, nil
}

// Logical IDs stay out of the object. A singleton node pipeline retains the
// sidecar identity even when a filter removes an output or changes its fields.
func (p *Preview) filterLogical(resources []plugin.Resource) ([]plugin.Resource, error) {
	filtered := make([]plugin.Resource, 0, len(resources))
	for _, output := range resources {
		node, err := yaml.Parse(output.YAML)
		if err != nil {
			return nil, fmt.Errorf("logical resource %q: %w", output.ID, err)
		}
		nodes := []*yaml.RNode{node}
		if p.filters != nil {
			for _, filter := range p.filters.Filters {
				nodes, err = filter.Filter.Filter(nodes)
				if err != nil {
					return nil, fmt.Errorf("filtering logical resource %q: %w", output.ID, err)
				}
			}
		}
		if len(nodes) == 0 {
			continue
		}
		if len(nodes) != 1 || nodes[0] == nil {
			return nil, fmt.Errorf("filter for logical resource %q must retain at most one object", output.ID)
		}
		node = nodes[0]
		if p.sopsDecrypt {
			object, err := node.Map()
			if err != nil {
				return nil, fmt.Errorf("logical resource %q: %w", output.ID, err)
			}
			if node.GetKind() == sops.SecretGVK && sops.IsSOPSContainer(object) {
				data, err := node.String()
				if err != nil {
					return nil, err
				}
				decrypted, err := sops.DecryptDocument([]byte(data))
				if err != nil {
					return nil, fmt.Errorf("decrypting logical resource %q: %w", output.ID, err)
				}
				// kyaml.Parse reads the first document. Validate the original bytes
				// so trailing documents cannot silently disappear from the preview.
				if _, err := plugin.Object(plugin.Resource{YAML: string(decrypted)}); err != nil {
					return nil, fmt.Errorf("decrypted logical resource %q: %w", output.ID, err)
				}
				node, err = yaml.Parse(string(decrypted))
				if err != nil {
					return nil, fmt.Errorf("parsing decrypted logical resource %q: %w", output.ID, err)
				}
			}
		}
		if node.GetName() != "" {
			return nil, fmt.Errorf("filter or decryption added metadata.name to logical resource %q", output.ID)
		}
		if p.excludeCRDs && node.GetKind() == "CustomResourceDefinition" {
			continue
		}
		if p.helmReleaseName != "" && node.GetLabels()["helm.toolkit.fluxcd.io/name"] != p.helmReleaseName {
			continue
		}
		output.YAML, err = node.String()
		if err != nil {
			return nil, fmt.Errorf("encoding logical resource %q: %w", output.ID, err)
		}
		if _, err := plugin.Object(output); err != nil {
			return nil, fmt.Errorf("logical resource %q: %w", output.ID, err)
		}
		filtered = append(filtered, output)
	}
	if p.sortOutput {
		sort.Slice(filtered, func(i, j int) bool { return filtered[i].ID < filtered[j].ID })
	}
	return filtered, nil
}
