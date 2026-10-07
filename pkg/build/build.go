// Package build loads manifest paths and executes Kustomize for render plugins.
// Host inventory and comparison use the passive pkg/render collection instead.
package build

import (
	"fmt"
	"path/filepath"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/resource"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// Builder owns path building and the passive collection receiving its results.
type Builder struct {
	*render.Render
	kustomizer *krusty.Kustomizer
	localRoot  string
}

// New creates a builder with default Kustomize options and execution plugins disabled.
func New(log logr.Logger) *Builder {
	opts := krusty.MakeDefaultOptions()
	opts.PluginConfig = types.DisabledPluginConfig()
	return &Builder{
		Render: render.NewDefaultRender(log), kustomizer: krusty.MakeKustomizer(opts),
	}
}

// SetLocalOnly confines subsequent builds and reads to root.
func (b *Builder) SetLocalOnly(root string) {
	b.localRoot = root
}

// AddKustomization builds a Kustomize directory and records its path provenance.
func (b *Builder) AddKustomization(fs filesys.FileSystem, path string) error {
	return b.AddKustomizationWithProducer(fs, path, render.PathProvenance(path).String())
}

// AddKustomizationWithProducer builds a directory and records the producer.
func (b *Builder) AddKustomizationWithProducer(fs filesys.FileSystem, path, producer string) error {
	if b.localRoot != "" {
		if err := render.ValidateLocalKustomization(fs, b.localRoot, path); err != nil {
			return err
		}
	}
	resources, err := b.kustomizer.Run(fs, path)
	if err != nil {
		return err
	}
	return b.AbsorbWithProducer(path, producer, resources)
}

// AddPath builds a Kustomize directory, or loads its raw YAML manifests.
func (b *Builder) AddPath(fs filesys.FileSystem, path string) error {
	return b.AddPathWithProducer(fs, path, render.PathProvenance(path).String())
}

// AddPathWithProducer loads a path and records its rendering producer.
func (b *Builder) AddPathWithProducer(fs filesys.FileSystem, path, producer string) error {
	if b.localRoot != "" {
		if err := render.ValidateLocalPath(fs, b.localRoot, path); err != nil {
			return err
		}
	}
	if isKustomization(fs, path) {
		return b.AddKustomizationWithProducer(fs, path, producer)
	}
	return b.addRawYAMLFiles(fs, path, producer)
}

func (b *Builder) addRawYAMLFiles(fs filesys.FileSystem, dir, producer string) error {
	entries, err := fs.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading directory %s: %w", dir, err)
	}
	for _, name := range entries {
		toolConfig := name == ".fmp.yaml" || name == ".fmp.yml" || name == ".gitops-preview.yaml" || name == ".gitops-preview.yml"
		githubConfig := filepath.Base(dir) == ".github" && name == "fmp.yaml"
		if toolConfig || githubConfig {
			continue
		}
		if ext := filepath.Ext(name); ext != ".yaml" && ext != ".yml" {
			continue
		}
		full := filepath.Join(dir, name)
		if b.localRoot != "" {
			if err := render.ValidateLocalPath(fs, b.localRoot, full); err != nil {
				return err
			}
		}
		data, err := fs.ReadFile(full)
		if err != nil {
			return fmt.Errorf("reading %s: %w", full, err)
		}
		resources, err := resmap.NewFactory(resource.NewFactory(nil)).NewResMapFromBytes(data)
		if err != nil {
			return fmt.Errorf("parsing %s: %w", full, err)
		}
		if err := b.AbsorbWithProducer(full, producer, resources); err != nil {
			return fmt.Errorf("appending resources from %s: %w", full, err)
		}
	}
	return nil
}

// AddPaths recursively loads independent directories, stopping below Kustomize bases.
func (b *Builder) AddPaths(fs filesys.FileSystem, root string) error {
	return b.AddPathsWithProducer(fs, root, render.PathProvenance(root).String())
}

// AddPathsWithProducer recursively builds paths with the supplied producer.
func (b *Builder) AddPathsWithProducer(fs filesys.FileSystem, root, producer string) error {
	return WalkPaths(fs, root, func(path string) error {
		return b.AddPathWithProducer(fs, path, producer)
	})
}

// WalkPaths visits independent build directories, stopping below Kustomize bases.
func WalkPaths(fs filesys.FileSystem, root string, visit func(string) error) error {
	if err := visit(root); err != nil {
		return err
	}
	if isKustomization(fs, root) {
		return nil
	}
	entries, err := fs.ReadDir(root)
	if err != nil {
		return fmt.Errorf("reading directory %s: %w", root, err)
	}
	for _, name := range entries {
		sub := filepath.Join(root, name)
		if fs.IsDir(sub) {
			if err := WalkPaths(fs, sub, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

func isKustomization(fs filesys.FileSystem, path string) bool {
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		if fs.Exists(filepath.Join(path, name)) {
			return true
		}
	}
	return false
}
