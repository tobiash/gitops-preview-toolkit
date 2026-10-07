package plugin

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"

	"gopkg.in/yaml.v3"
)

// Object decodes exactly one Kubernetes object, retaining YAML scalar types.
func Object(resource Resource) (map[string]any, error) {
	decoder := yaml.NewDecoder(bytes.NewBufferString(resource.YAML))
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("resource must be a Kubernetes object")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("resource must contain exactly one YAML document")
	}
	if err := normalizeNumbers(object); err != nil {
		return nil, err
	}
	return object, nil
}

// Kubernetes unstructured values accept int64, not the platform int produced by
// YAML decoding. Preserve integer precision instead of round-tripping float64.
func normalizeNumbers(object map[string]any) error {
	for key, value := range object {
		normalized, err := normalizeValue(value)
		if err != nil {
			return fmt.Errorf("field %q: %w", key, err)
		}
		object[key] = normalized
	}
	return nil
}

func normalizeValue(value any) (any, error) {
	switch value := value.(type) {
	case int:
		return int64(value), nil
	case uint64:
		if value > math.MaxInt64 {
			return nil, errors.New("integer exceeds Kubernetes int64 range")
		}
		return int64(value), nil
	case map[string]any:
		return value, normalizeNumbers(value)
	case []any:
		for i, item := range value {
			normalized, err := normalizeValue(item)
			if err != nil {
				return nil, err
			}
			value[i] = normalized
		}
		return value, nil
	default:
		return value, nil
	}
}

// ObjectID returns the concrete Kubernetes identity used across render engines.
func ObjectID(object map[string]any) (string, error) {
	apiVersion, _ := object["apiVersion"].(string)
	kind, _ := object["kind"].(string)
	metadata, _ := object["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	namespace, _ := metadata["namespace"].(string)
	if apiVersion == "" || kind == "" || name == "" {
		return "", errors.New("named resource requires apiVersion, kind and metadata.name")
	}
	return fmt.Sprintf("%s/%s/%s/%s", apiVersion, kind, namespace, name), nil
}

// ParseResource validates named YAML and attaches out-of-band provenance.
func ParseResource(data []byte, provenance Provenance) (Resource, error) {
	resource := Resource{YAML: string(data), Provenance: provenance}
	object, err := Object(resource)
	if err != nil {
		return Resource{}, err
	}
	resource.ID, err = ObjectID(object)
	return resource, err
}
