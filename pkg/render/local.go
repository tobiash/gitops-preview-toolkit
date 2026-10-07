package render

import (
	"fmt"
	"path/filepath"
	"strings"

	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// ValidateLocalPath requires an existing path inside root, resolving symlinks.
// It validates references, not concurrent filesystem mutations or OS isolation.
func ValidateLocalPath(fs filesys.FileSystem, root, path string) error {
	rootDir, rootFile, err := fs.CleanedAbs(root)
	if err != nil || rootFile != "" {
		return fmt.Errorf("local-only: invalid source root %q: %v", root, err)
	}
	dir, file, err := fs.CleanedAbs(path)
	if err != nil {
		return fmt.Errorf("local-only: resolving %q: %w", path, err)
	}
	rel, err := filepath.Rel(string(rootDir), filepath.Join(string(dir), file))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("local-only: path %q escapes source root %q", path, root)
	}
	if !fs.Exists(path) {
		return fmt.Errorf("local-only: path %q does not exist", path)
	}
	return nil
}

// ValidateLocalKustomization checks every supported read reference before krusty
// can interpret it as a URL. Custom generators/transformers/validators and Helm
// generators are deliberately unsupported; external plugins remain disabled.
func ValidateLocalKustomization(fs filesys.FileSystem, root, path string) error {
	return validateLocalKustomization(fs, root, path, make(map[string]bool))
}

func validateLocalKustomization(fs filesys.FileSystem, root, path string, seen map[string]bool) error {
	if err := ValidateLocalPath(fs, root, path); err != nil {
		return err
	}
	dir, file, err := fs.CleanedAbs(path)
	if err != nil {
		return err
	}
	key := filepath.Join(string(dir), file)
	if seen[key] {
		return nil
	}
	seen[key] = true
	var data []byte
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		configPath := filepath.Join(path, name)
		if !fs.Exists(configPath) {
			continue
		}
		if err := ValidateLocalPath(fs, root, configPath); err != nil {
			return err
		}
		data, err = fs.ReadFile(configPath)
		if err != nil {
			return err
		}
		break
	}
	if data == nil {
		return fmt.Errorf("local-only: no kustomization in %q", path)
	}
	var cfg types.Kustomization
	if err := cfg.Unmarshal(data); err != nil {
		return fmt.Errorf("local-only: invalid kustomization: %w", err)
	}
	if cfg.HelmGlobals != nil || len(cfg.HelmCharts)+len(cfg.HelmChartInflationGenerator) > 0 {
		return fmt.Errorf("local-only: Kustomize Helm generators are unsupported")
	}
	if len(cfg.Generators)+len(cfg.Transformers)+len(cfg.Validators) > 0 {
		return fmt.Errorf("local-only: custom generators, transformers and validators are unsupported (plugins disabled)")
	}
	check := func(ref string, recurse bool) error {
		if ref == "" || filepath.IsAbs(ref) || strings.ContainsAny(ref, ":\\\x00@?#") || strings.Contains(ref, "//") || strings.HasPrefix(strings.ToLower(ref), "github.com/") {
			return fmt.Errorf("local-only: unsupported reference %q", ref)
		}
		full := filepath.Join(path, ref)
		if err := ValidateLocalPath(fs, root, full); err != nil {
			return err
		}
		if fs.IsDir(full) {
			if !recurse {
				return fmt.Errorf("local-only: expected file reference %q", ref)
			}
			return validateLocalKustomization(fs, root, full, seen)
		}
		return nil
	}
	for _, refs := range [][]string{cfg.Resources, cfg.Bases, cfg.Components} { //nolint:staticcheck // Legacy fields still trigger reads and must be validated.
		for _, ref := range refs {
			if err := check(ref, true); err != nil {
				return err
			}
		}
	}
	refs := append(append([]string(nil), cfg.Configurations...), cfg.Crds...)
	if ref := cfg.OpenAPI["path"]; ref != "" {
		refs = append(refs, ref)
	}
	for _, patch := range append(cfg.Patches, cfg.PatchesJson6902...) { //nolint:staticcheck // Legacy fields still trigger reads and must be validated.
		if patch.Path != "" {
			refs = append(refs, patch.Path)
		}
	}
	for _, patch := range cfg.PatchesStrategicMerge { //nolint:staticcheck // Legacy fields still trigger reads and must be validated.
		// Inline legacy patches are unsupported rather than guessed to be paths.
		if strings.ContainsAny(string(patch), "\n\r") {
			return fmt.Errorf("local-only: inline patchesStrategicMerge is unsupported; use patches.patch")
		}
		refs = append(refs, string(patch))
	}
	for _, replacement := range cfg.Replacements {
		if replacement.Path != "" {
			refs = append(refs, replacement.Path)
		}
	}
	var generators []types.GeneratorArgs
	for _, generator := range cfg.ConfigMapGenerator {
		generators = append(generators, generator.GeneratorArgs)
	}
	for _, generator := range cfg.SecretGenerator {
		generators = append(generators, generator.GeneratorArgs)
	}
	for _, generator := range generators {
		refs = append(refs, generator.EnvSources...)
		if generator.EnvSource != "" {
			refs = append(refs, generator.EnvSource)
		}
		for _, ref := range generator.FileSources {
			if _, value, ok := strings.Cut(ref, "="); ok {
				ref = value
			}
			refs = append(refs, ref)
		}
	}
	for _, ref := range refs {
		if err := check(ref, false); err != nil {
			return err
		}
	}
	return nil
}
