package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/preview"
)

func TestEscapedRequestTooLargePreservesOperation(t *testing.T) {
	s := newTestService(t, t.TempDir(), Options{})
	req := Request{Operation: "query", ID: strings.Repeat("&", 12000)}
	if !requestFits(req) {
		t.Fatal("fixture must pass the unescaped size check")
	}
	r := s.Execute(context.Background(), req)
	if r.Operation != "query" || r.Status != "failure" || r.Complete || r.Error == nil || r.Error.Code != "RequestTooLarge" {
		t.Fatalf("escaped oversized request = %+v, want query/RequestTooLarge", r)
	}
}

func TestCleanupFailureIsGeneric(t *testing.T) {
	for _, initial := range []Response{success("render", RenderData{ID: "unpublished"}), failure("render", "Incomplete", "render", "Render is incomplete.")} {
		result := initial
		cleanupResult(&result, "render", errors.New("sensitive-path-and-chart-content"))
		if result.Status != "failure" || result.Complete || result.Data != nil || result.Operation != "render" {
			t.Fatalf("cleanup result = %+v", result)
		}
		if initial.Status == "failure" && result.Error.Code != initial.Error.Code {
			t.Fatal("cleanup replaced primary failure")
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "sensitive-path") || !strings.Contains(string(encoded), "CleanupFailed") {
			t.Fatalf("cleanup diagnostic = %s", encoded)
		}
	}
}

func TestAgentStrictInputsInBothProfiles(t *testing.T) {
	for _, kind := range []string{"Kustomization", "HelmRelease"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			writeFixture(t, root, ".fmp.yaml", "paths: [manifests]\nresolve-git: true\n")
			writeFixture(t, root, "manifests/source.yaml", "apiVersion: source.toolkit.fluxcd.io/v1\nkind: GitRepository\nmetadata: {name: local, namespace: default}\nspec: {url: '.'}\n")
			if kind == "Kustomization" {
				writeFixture(t, root, "manifests/object.yaml", "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata: {name: child, namespace: default}\nspec:\n  path: child\n  sourceRef: {kind: GitRepository, name: local}\n  postBuild:\n    substituteFrom: [{kind: Secret, name: required, optional: false}]\n")
				writeFixture(t, root, "child/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: child}\ndata: {value: '${REQUIRED}'}\n")
			} else {
				writeFixture(t, root, "manifests/object.yaml", "apiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata: {name: app, namespace: default}\nspec:\n  chart:\n    spec:\n      chart: chart\n      sourceRef: {kind: GitRepository, name: local}\n      valuesFiles: [chart/override.yaml]\n")
				writeFixture(t, root, "chart/Chart.yaml", "apiVersion: v2\nname: local\nversion: 0.1.0\n")
				writeFixture(t, root, "chart/values.yaml", "value: original\n")
				writeFixture(t, root, "chart/override.yaml", "value: overridden\n")
				writeFixture(t, root, "chart/templates/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: app}\ndata:\n  value: {{ .Values.value | quote }}\n")
			}
			for _, trusted := range []bool{false, true} {
				s := newTestService(t, root, Options{Trusted: trusted})
				expectError(t, s, Request{Operation: "render"}, "Incomplete")
				expectError(t, s, Request{Operation: "preview", Base: "worktree"}, "Incomplete")
				if len(s.entries) != 0 {
					t.Fatalf("trusted=%t: unsupported transform published handles", trusted)
				}
			}
			// Strictness is an agent requirement, not a change to the legacy API.
			p, err := preview.New(preview.WithLogger(logr.Discard()), preview.WithPaths([]string{"manifests"}, false), preview.WithGitRepo(), preview.WithFluxKS(), preview.WithHelm(&config.HelmSettings{}))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := p.Close(); err != nil {
					t.Error(err)
				}
			})
			snapshot, err := p.RenderSnapshot(t.Context(), root)
			if err != nil || snapshot == nil || !snapshot.Complete {
				t.Fatalf("legacy render = %+v, %v; want unchanged complete behavior", snapshot, err)
			}
		})
	}
}
