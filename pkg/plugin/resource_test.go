package plugin_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
)

func TestObjectPreservesKubernetesScalarTypesAndIntegerPrecision(t *testing.T) {
	t.Parallel()
	const manifest = `apiVersion: example.org/v1
kind: Counter
metadata:
  name: precise
spec:
  large: 9007199254740993
  maximum: 9223372036854775807
  minimum: -9223372036854775808
  nested:
    count: 7
  items:
    - 9007199254740993
    - count: -7
    - [0, 9223372036854775807]
  fraction: 1.25
  enabled: true
  text: "9007199254740993"
  absent: null
`
	provenance := plugin.Provenance{Kind: "source", Path: "counter.yaml", Text: "origin"}
	resource, err := plugin.ParseResource([]byte(manifest), provenance)
	if err != nil {
		t.Fatal(err)
	}
	if resource.ID != "example.org/v1/Counter//precise" || resource.YAML != manifest || resource.Provenance != provenance {
		t.Fatalf("resource identity, YAML or provenance changed: %+v", resource)
	}
	object, err := plugin.Object(resource)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"large": int64(9007199254740993), "maximum": int64(9223372036854775807),
		"minimum": int64(-9223372036854775808), "nested": map[string]any{"count": int64(7)},
		"items":    []any{int64(9007199254740993), map[string]any{"count": int64(-7)}, []any{int64(0), int64(9223372036854775807)}},
		"fraction": float64(1.25), "enabled": true, "text": "9007199254740993", "absent": nil,
	}
	if !reflect.DeepEqual(object["spec"], want) {
		t.Fatalf("scalar types or precision changed: got %#v, want %#v", object["spec"], want)
	}
	data, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	for _, literal := range []string{`"large":9007199254740993`, `"maximum":9223372036854775807`, `"minimum":-9223372036854775808`} {
		if !strings.Contains(string(data), literal) {
			t.Errorf("JSON lost exact integer %s: %s", literal, data)
		}
	}
}

func TestObjectRejectsMalformedOrMultipleDocuments(t *testing.T) {
	t.Parallel()
	const valid = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n"
	for _, test := range []struct {
		name string
		yaml string
	}{
		{name: "empty"},
		{name: "null", yaml: "null"},
		{name: "scalar", yaml: "42"},
		{name: "sequence", yaml: "[one, two]"},
		{name: "malformed first document", yaml: "metadata: ["},
		{name: "duplicate mapping key", yaml: valid + "kind: Secret\n"},
		{name: "second object", yaml: valid + "---\n" + valid},
		{name: "empty second document", yaml: valid + "---\n"},
		{name: "malformed second document", yaml: valid + "---\n["},
		{name: "integer overflow", yaml: valid + "spec:\n  count: 9223372036854775808\n"},
		{name: "nested integer overflow", yaml: valid + "spec:\n  items:\n    - count: 18446744073709551615\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := plugin.Object(plugin.Resource{YAML: test.yaml}); err == nil {
				t.Fatal("invalid resource document accepted")
			}
			if _, err := plugin.ParseResource([]byte(test.yaml), plugin.Provenance{}); err == nil {
				t.Fatal("invalid resource parsed successfully")
			}
		})
	}
}

func TestParseResourceRequiresNamedStringIdentity(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		yaml string
	}{
		{name: "numeric apiVersion", yaml: "apiVersion: 1\nkind: ConfigMap\nmetadata:\n  name: test\n"},
		{name: "numeric kind", yaml: "apiVersion: v1\nkind: 42\nmetadata:\n  name: test\n"},
		{name: "numeric name", yaml: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: 42\n"},
		{name: "numeric metadata", yaml: "apiVersion: v1\nkind: ConfigMap\nmetadata: 42\n"},
		{name: "generated name only", yaml: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  generateName: test-\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if _, err := plugin.ParseResource([]byte(test.yaml), plugin.Provenance{}); err == nil {
				t.Fatal("invalid named identity accepted")
			}
		})
	}
}
