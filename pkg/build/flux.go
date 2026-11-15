package build

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	fluxksv1 "github.com/fluxcd/kustomize-controller/api/v1"
	fluxkustomize "github.com/fluxcd/pkg/apis/kustomize"
	"github.com/tobiash/gitops-preview-toolkit/pkg/render"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/kustomize/kyaml/resid"
	"sigs.k8s.io/yaml"
)

// FluxOptions holds only rendering inputs, so discovery keys include all build
// overrides without depending on reconciliation state or object metadata.
type FluxOptions struct {
	Namespace      string
	NamePrefix     string
	NameSuffix     string
	Patches        []fluxkustomize.Patch
	Images         []fluxkustomize.Image
	Components     []string
	CommonMetadata *fluxksv1.CommonMetadata
}

// AddFluxPathWithProducer builds the generated Flux overlay without modifying
// source files. The merge rules follow fluxcd/pkg/kustomize v1.18.0 (the version
// used by kustomize-controller v1.6.1, matching our controller API). Actual
// transforms, selector matching, name references and errors belong to Kustomize,
// not post-render YAML rewrites. In particular, images replace matching entries
// and prefixes/suffixes override the root configuration rather than wrapping it.
func (b *Builder) AddFluxPathWithProducer(
	fs filesys.FileSystem,
	path string,
	producer string,
	opts FluxOptions,
) error {
	if b.localRoot != "" {
		if err := render.ValidateLocalPath(fs, b.localRoot, path); err != nil {
			return err
		}
	}
	dir, file, err := fs.CleanedAbs(path)
	if err != nil {
		return fmt.Errorf("resolving Flux build directory: %w", err)
	}
	if file != "" {
		return fmt.Errorf("flux build path %q is not a directory", path)
	}
	// Kustomize canonicalizes its loader root. Use the same root for the
	// virtual file, including when spec.path names an in-source symlink.
	path = filepath.Clean(string(dir))
	cfg, kfile, err := b.fluxConfiguration(fs, path)
	if err != nil {
		return err
	}
	if err := mergeFluxOptions(&cfg, opts); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("encoding Flux build configuration: %w", err)
	}
	overlay := &fluxFileSystem{FileSystem: fs, path: filepath.Clean(kfile), data: data}
	if b.localRoot != "" {
		if err := render.ValidateLocalKustomization(overlay, b.localRoot, path); err != nil {
			return err
		}
	}
	// Like Flux, permit sibling files inside the source rather than restricting
	// all reads to the overlay directory. Local-only validation above enforces
	// the source boundary and rejects remote inputs before invoking Kustomize.
	buildOptions := krusty.MakeDefaultOptions()
	buildOptions.LoadRestrictions = types.LoadRestrictionsNone
	buildOptions.PluginConfig = types.DisabledPluginConfig()
	resources, err := krusty.MakeKustomizer(buildOptions).Run(overlay, path)
	if err != nil {
		return fmt.Errorf("building Flux overlay: %w", err)
	}
	return b.AbsorbWithProducer(path, producer, resources)
}

func (b *Builder) fluxConfiguration(fs filesys.FileSystem, path string) (types.Kustomization, string, error) {
	cfg := types.Kustomization{TypeMeta: types.TypeMeta{
		APIVersion: types.KustomizationVersion, Kind: types.KustomizationKind,
	}}
	kfile := filepath.Join(path, "kustomization.yaml")
	if isKustomization(fs, path) {
		for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
			candidate := filepath.Join(path, name)
			if !fs.Exists(candidate) {
				continue
			}
			if b.localRoot != "" {
				if err := render.ValidateLocalPath(fs, b.localRoot, candidate); err != nil {
					return cfg, "", err
				}
			}
			data, err := fs.ReadFile(candidate)
			if err != nil {
				return cfg, "", err
			}
			if err := cfg.Unmarshal(data); err != nil {
				return cfg, "", fmt.Errorf("reading Flux build configuration: %w", err)
			}
			kfile = candidate
			break
		}
		if len(cfg.Resources) == 0 {
			cfg.BuildMetadata = []string{"originAnnotations"}
		}
	} else {
		if err := fs.Walk(path, func(file string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if file == path {
				return nil
			}
			if info.IsDir() {
				if !isKustomization(fs, file) {
					return nil
				}
			} else if ext := filepath.Ext(file); ext != ".yaml" && ext != ".yml" {
				return nil
			}
			if b.localRoot != "" {
				if err := render.ValidateLocalPath(fs, b.localRoot, file); err != nil {
					return err
				}
			}
			rel, err := filepath.Rel(path, file)
			if err != nil {
				return err
			}
			cfg.Resources = append(cfg.Resources, filepath.ToSlash(rel))
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}); err != nil {
			return cfg, "", fmt.Errorf("generating Flux build configuration: %w", err)
		}
		if len(cfg.Resources) == 0 {
			cfg.Namespace = "_placeholder"
		}
	}
	return cfg, kfile, nil
}

func mergeFluxOptions(cfg *types.Kustomization, opts FluxOptions) error {
	if opts.Namespace != "" {
		cfg.Namespace = opts.Namespace
	}
	if opts.NamePrefix != "" {
		cfg.NamePrefix = opts.NamePrefix
	}
	if opts.NameSuffix != "" {
		cfg.NameSuffix = opts.NameSuffix
	}
	for _, patch := range opts.Patches {
		p := types.Patch{Patch: patch.Patch}
		if target := patch.Target; target != nil {
			p.Target = &types.Selector{
				ResId: resid.ResId{
					Gvk:  resid.Gvk{Group: target.Group, Version: target.Version, Kind: target.Kind},
					Name: target.Name, Namespace: target.Namespace,
				},
				LabelSelector: target.LabelSelector, AnnotationSelector: target.AnnotationSelector,
			}
		}
		cfg.Patches = append(cfg.Patches, p)
	}
	for _, component := range opts.Components {
		// Flux components must be local and relative even outside local-only mode.
		if component == "" || filepath.IsAbs(component) || strings.ContainsAny(component, ":\\\x00@?#") ||
			strings.Contains(component, "//") || strings.HasPrefix(strings.ToLower(component), "github.com/") {
			return fmt.Errorf("component path %q must be local and relative", component)
		}
		cfg.Components = append(cfg.Components, component)
	}
	for _, image := range opts.Images {
		entry := types.Image{Name: image.Name, NewName: image.NewName, NewTag: image.NewTag, Digest: image.Digest}
		found := false
		for i := range cfg.Images {
			if cfg.Images[i].Name == entry.Name {
				cfg.Images[i] = entry
				found = true
				break
			}
		}
		if !found {
			cfg.Images = append(cfg.Images, entry)
		}
	}
	return nil
}

// ApplyCommonMetadata follows Flux's apply-stage behavior: only object metadata
// is changed, after postBuild substitution. Pod templates and selectors are not.
func ApplyCommonMetadata(r *render.Render, metadata *fluxksv1.CommonMetadata) error {
	if metadata == nil {
		return nil
	}
	for _, res := range r.Resources() {
		labels := res.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		maps.Copy(labels, metadata.Labels)
		if len(labels) > 0 {
			if err := res.SetLabels(labels); err != nil {
				return err
			}
		}
		annotations := res.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		maps.Copy(annotations, metadata.Annotations)
		if len(annotations) > 0 {
			if err := res.SetAnnotations(annotations); err != nil {
				return err
			}
		}
	}
	return nil
}

// fluxFileSystem overlays one generated file; all other reads (including local
// boundary and symlink validation) continue to use the original filesystem.
type fluxFileSystem struct {
	filesys.FileSystem
	path string
	data []byte
}

func (fs *fluxFileSystem) ReadFile(path string) ([]byte, error) {
	if filepath.Clean(path) == fs.path {
		return fs.data, nil
	}
	return fs.FileSystem.ReadFile(path)
}

func (fs *fluxFileSystem) Exists(path string) bool {
	return filepath.Clean(path) == fs.path || fs.FileSystem.Exists(path)
}

func (fs *fluxFileSystem) CleanedAbs(path string) (filesys.ConfirmedDir, string, error) {
	if filepath.Clean(path) == fs.path {
		dir, _, err := fs.FileSystem.CleanedAbs(filepath.Dir(path))
		return dir, filepath.Base(path), err
	}
	return fs.FileSystem.CleanedAbs(path)
}
