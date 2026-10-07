//go:build integration

package crossplanerender

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
)

// TestLiveTemplating uses the real released controller and a separately running
// upstream function-go-templating. Nothing in this test substitutes an engine.
func TestLiveTemplating(t *testing.T) {
	binary := os.Getenv("CROSSPLANE_TEST_ENGINE")
	target := os.Getenv("CROSSPLANE_TEST_TEMPLATING_TARGET")
	if binary == "" || target == "" {
		t.Skip("set CROSSPLANE_TEST_ENGINE and CROSSPLANE_TEST_TEMPLATING_TARGET for real rendering")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	service := New(logr.Discard())
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	config, err := json.Marshal(Config{
		EngineBinary: binary, Runtime: "Development", DevelopmentTargets: map[string]string{"templating": target},
		Timeout: "45s", MaxFunctions: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := service.OpenRender(ctx, &plugin.OpenRequest{Fresh: false, Config: config})
	if err != nil {
		t.Fatal(err)
	}
	inv := fixtureInventory(t)
	inv[2] = testResource(t, `apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: live
spec:
  compositeTypeRef:
    apiVersion: example.org/v1
    kind: App
  mode: Pipeline
  pipeline:
  - step: template
    functionRef:
      name: templating
    input:
      apiVersion: gotemplating.fn.crossplane.io/v1beta1
      kind: GoTemplate
      source: Inline
      inline:
        template: |
          apiVersion: v1
          kind: ConfigMap
          metadata:
            name: explicit-live-output
            annotations:
              gotemplating.fn.crossplane.io/composition-resource-name: named
          data:
            replicas: {{ .observed.composite.resource.spec.replicas | quote }}
            observedCount: {{ .observed.resources | default (dict) | len | quote }}
            randomValue: {{ randAlphaNum 24 | quote }}
          ---
          apiVersion: v1
          kind: ConfigMap
          metadata:
            generateName: function-prefix-
            annotations:
              gotemplating.fn.crossplane.io/composition-resource-name: unnamed
          data:
            actualFunction: "true"
`)
	request := &plugin.ExpandRequest{Session: opened.Session, Resources: resourcesOf(inv)}
	var first *plugin.ExpandResponse
	for sweep := range 2 {
		response, err := service.Expand(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Diagnostics) != 0 {
			t.Fatalf("live render diagnostics: %+v", response.Diagnostics)
		}
		if len(response.Expansions) != 1 || len(response.Expansions[0].Resources) != 2 {
			t.Fatalf("live desired output: %+v", response.Expansions)
		}
		logical, named := 0, 0
		for _, resource := range response.Expansions[0].Resources {
			object, err := plugin.Object(resource)
			if err != nil {
				t.Fatal(err)
			}
			if resource.Logical {
				logical++
				metadata, _ := object["metadata"].(map[string]any)
				if name, _ := metadata["name"].(string); name != "" {
					t.Fatal("live synthetic name leaked into inventory")
				}
			} else {
				named++
				data, _ := object["data"].(map[string]any)
				if data["replicas"] != "2" {
					t.Fatalf("XRD default not seen by real function: %#v", data)
				}
				if data["observedCount"] != "0" {
					t.Fatal("prior desired output reached the actual function as observed state")
				}
			}
		}
		if logical != 1 || named != 1 {
			t.Fatal("live explicit versus generated naming distinction failed")
		}
		if len(service.sessions[opened.Session].runtimes) != 1 {
			t.Fatal("runtime cache grew between sweeps")
		}
		if sweep == 0 {
			first = response
			request.Resources = append(resourcesOf(inv), response.Expansions[0].Resources...)
		} else {
			before, _ := json.Marshal(first.Expansions)
			after, _ := json.Marshal(response.Expansions)
			if string(before) != string(after) {
				t.Fatal("real desired output changed between identical sweeps")
			}
		}
	}
	// A genuine input change must execute the real function again, still
	// without promoting the previous desired outputs into observed state.
	setField(t, inv[1].object, int64(3), "spec", "replicas")
	inv[1].resource = marshalResource(t, inv[1])
	request.Resources = append(resourcesOf(inv), first.Expansions[0].Resources...)
	changed, err := service.Expand(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed.Diagnostics) != 0 {
		t.Fatal(changed.Diagnostics)
	}
	for _, resource := range changed.Expansions[0].Resources {
		if resource.Logical {
			continue
		}
		object, err := plugin.Object(resource)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := object["data"].(map[string]any)
		if data["replicas"] != "3" || data["observedCount"] != "0" {
			t.Fatal("changed real input was cached, or desired output was observed")
		}
	}
	// Exercise the pinned engine's real bootstrap selector recording, including
	// a zero-match FATAL that must invalidate when its matching input appears.
	inv[2] = testResource(t, `apiVersion: apiextensions.crossplane.io/v1
kind: Composition
metadata:
  name: live-required
spec:
  compositeTypeRef:
    apiVersion: example.org/v1
    kind: App
  mode: Pipeline
  pipeline:
  - step: template
    functionRef:
      name: templating
    requirements:
      requiredResources:
      - requirementName: input
        apiVersion: v1
        kind: ConfigMap
        namespace: apps
        matchName: live-input
    input:
      apiVersion: gotemplating.fn.crossplane.io/v1beta1
      kind: GoTemplate
      source: Inline
      inline:
        template: |
          {{ $required := index (.requiredResources | default (dict)) "input" }}
          {{ $items := $required.items | default (list) }}
          {{ if eq (len $items) 0 }}{{ fail "required input absent" }}{{ end }}
          apiVersion: v1
          kind: ConfigMap
          metadata:
            name: live-dependent-output
            annotations:
              gotemplating.fn.crossplane.io/composition-resource-name: dependent
          data:
            value: {{ (index $items 0).resource.data.value | quote }}
`)
	request.Resources = resourcesOf(inv)
	firstFatal := mustExpand(t, service, request)
	secondFatal := mustExpand(t, service, request)
	if len(firstFatal.Diagnostics) != 1 || firstFatal.Diagnostics[0].Code != "pipeline-fatal" {
		t.Fatalf("real pending requirement diagnostics: %+v", firstFatal.Diagnostics)
	}
	a, _ := json.Marshal(firstFatal)
	b, _ := json.Marshal(secondFatal)
	if string(a) != string(b) {
		t.Fatal("unchanged real pending FATAL was not stable")
	}
	dependency := testResource(t, `apiVersion: v1
kind: ConfigMap
metadata: {name: live-input, namespace: apps}
data: {value: first}
`)
	for _, value := range []string{"first", "changed"} {
		setField(t, dependency.object, value, "data", "value")
		request.Resources = append(resourcesOf(inv), marshalResource(t, dependency))
		resolved := mustExpand(t, service, request)
		if len(resolved.Diagnostics) != 0 || len(resolved.Expansions[0].Resources) != 1 {
			t.Fatalf("real requirement did not resolve: %+v", resolved.Diagnostics)
		}
		object, err := plugin.Object(resolved.Expansions[0].Resources[0])
		if err != nil {
			t.Fatal(err)
		}
		data, _ := object["data"].(map[string]any)
		if data["value"] != value {
			t.Fatal("changed requested resource was served from a stale real evaluation")
		}
	}
}
