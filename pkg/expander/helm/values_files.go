package helm

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	chart "helm.sh/helm/v4/pkg/chart/v2"
	"sigs.k8s.io/yaml"
)

// applyChartValuesFiles mirrors source-controller v1.8.1's chart builders:
// custom files replace (not overlay) chart defaults, before Helm merges release
// values. A lone values.yaml preserves the chart defaults even for Git sources.
// Git files are source-root relative; repository/OCI files live inside the chart.
func applyChartValuesFiles(ch *chart.Chart, task *RenderTask) error {
	paths := task.valuesFiles
	if len(paths) == 0 || len(paths) == 1 && filepath.Clean(paths[0]) == "values.yaml" {
		return nil
	}
	var root *os.Root
	if task.localSourceRoot != "" {
		var err error
		root, err = os.OpenRoot(task.localSourceRoot)
		if err != nil {
			return fmt.Errorf("opening chart source root: %w", err)
		}
		defer func() { _ = root.Close() }() // Read-only source handle; no pending writes.
	}
	merged := make(map[string]any)
	for _, path := range paths {
		name := filepath.Clean(path)
		var values map[string]any
		if root != nil {
			// Do not reinterpret absolute paths or clamp traversal to the root.
			// os.Root additionally confines symlink resolution to the source.
			if !filepath.IsLocal(path) {
				return fmt.Errorf("values file %q must be relative to and within the chart source", path)
			}
			data, err := readSourceValuesFile(root, path)
			if err != nil {
				if task.ignoreMissingValuesFiles && errors.Is(err, os.ErrNotExist) {
					continue
				}
				return fmt.Errorf("reading values file %q: %w", path, err)
			}
			if err := yaml.Unmarshal(data, &values); err != nil {
				return fmt.Errorf("unmarshaling values from %q: %w", path, err)
			}
		} else if name == "values.yaml" {
			values = ch.Values
		} else {
			var found bool
			for _, file := range ch.Files {
				if file.Name == name {
					found = true
					if err := yaml.Unmarshal(file.Data, &values); err != nil {
						return fmt.Errorf("unmarshaling values from %q: %w", path, err)
					}
					break
				}
			}
			if !found {
				if task.ignoreMissingValuesFiles {
					continue
				}
				return fmt.Errorf("no values file found at path %q", path)
			}
		}
		merged = mergeChartValuesMaps(merged, values)
	}
	// Upstream rejects replacement when the chart has no original values.yaml.
	for _, file := range ch.Raw {
		if file.Name == "values.yaml" {
			ch.Values = merged
			return nil
		}
	}
	return fmt.Errorf("failed to locate values file: values.yaml")
}

func readSourceValuesFile(root *os.Root, path string) ([]byte, error) {
	info, err := root.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular values file")
	}
	file, err := root.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }() // Read errors are reported by ReadAll below.
	return io.ReadAll(file)
}

// Like Flux transform.MergeMaps, recurse only when both values are mappings;
// later scalars, lists and nulls replace earlier values without Helm coalescing.
func mergeChartValuesMaps(base, override map[string]any) map[string]any {
	result := make(map[string]any, len(base)+len(override))
	for key, value := range base {
		result[key] = value
	}
	for key, value := range override {
		if nested, ok := value.(map[string]any); ok {
			if previous, ok := result[key].(map[string]any); ok {
				value = mergeChartValuesMaps(previous, nested)
			}
		}
		result[key] = value
	}
	return result
}
