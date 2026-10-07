package helm

import (
	"encoding/json"
	"fmt"
	"strings"

	"helm.sh/helm/v4/pkg/action"
	chartinterface "helm.sh/helm/v4/pkg/chart"
	chart "helm.sh/helm/v4/pkg/chart/v2"
)

func validateLocalChart(ch *chart.Chart) error {
	dependencies := make([]chartinterface.Dependency, len(ch.Metadata.Dependencies))
	for i, dependency := range ch.Metadata.Dependencies {
		dependencies[i] = dependency
	}
	if err := action.CheckDependencies(ch, dependencies); err != nil {
		return fmt.Errorf("local-only: chart dependencies must already be vendored: %w", err)
	}
	if len(ch.Schema) > 0 {
		var schema any
		if err := json.Unmarshal(ch.Schema, &schema); err != nil {
			return fmt.Errorf("local-only: invalid values schema: %w", err)
		}
		if err := validateLocalSchema(schema); err != nil {
			return err
		}
	}
	for _, child := range ch.Dependencies() {
		if err := validateLocalChart(child); err != nil {
			return err
		}
	}
	return nil
}

// Helm's schema compiler has both HTTP and unrestricted file loaders. Conservatively
// allow only in-document references, without IDs that could change their base URI.
func validateLocalSchema(value any) error {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			switch key {
			case "$ref", "$dynamicRef", "$recursiveRef", "$id", "id":
				if ref, ok := child.(string); ok && !strings.HasPrefix(ref, "#") {
					return fmt.Errorf("local-only: external values schema %s %q is unsupported", key, ref)
				}
			case "$schema":
				ref, _ := child.(string)
				if !strings.HasPrefix(ref, "https://") && !strings.HasPrefix(ref, "http://") {
					return fmt.Errorf("local-only: custom values metaschema is unsupported")
				}
				_, ref, _ = strings.Cut(strings.TrimSuffix(ref, "#"), "://")
				switch ref {
				case "json-schema.org/draft-04/schema", "json-schema.org/draft-06/schema", "json-schema.org/draft-07/schema", "json-schema.org/draft/2019-09/schema", "json-schema.org/draft/2020-12/schema":
					// These exact metaschemas are embedded in Helm's schema compiler.
				default:
					return fmt.Errorf("local-only: custom values metaschema is unsupported")
				}
			}
			if err := validateLocalSchema(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := validateLocalSchema(child); err != nil {
				return err
			}
		}
	}
	return nil
}
