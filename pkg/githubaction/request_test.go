package githubaction

import (
	"testing"
)

func TestGetInputSupportsUnderscoreAndHyphenEnvKeys(t *testing.T) {
	t.Setenv("INPUT_REPO_A", "/tmp/repo-a")
	t.Setenv("INPUT_REPO-B", "/tmp/repo-b")

	if got := getInput("repo-a", ""); got != "/tmp/repo-a" {
		t.Fatalf("getInput(repo-a) with underscore env = %q, want /tmp/repo-a", got)
	}

	if got := getInput("repo-b", ""); got != "/tmp/repo-b" {
		t.Fatalf("getInput(repo-b) with hyphen env = %q, want /tmp/repo-b", got)
	}
}

func TestParseRequestCrossplaneRuntimeInputs(t *testing.T) {
	t.Setenv("INPUT_CROSSPLANE", "true")
	t.Setenv("INPUT_CROSSPLANE_ENGINE", "/trusted/crossplane-core")
	t.Setenv("INPUT_CROSSPLANE_FUNCTION", "fn-a=localhost:9443\nfn-b=[::1]:9444")
	t.Setenv("INPUT_FLUX_PLUGIN", "/trusted/flux")
	t.Setenv("INPUT_CROSSPLANE_PLUGIN", "/trusted/crossplane")
	req, err := ParseRequestFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !req.Crossplane || req.CrossplaneEngine != "/trusted/crossplane-core" || len(req.CrossplaneFunctions) != 2 {
		t.Fatalf("runtime inputs not preserved: %#v", req)
	}
	if req.FluxPlugin != "/trusted/flux" || req.CrossplanePlugin != "/trusted/crossplane" {
		t.Fatalf("trusted plugin selection missing: %#v", req)
	}
}

func TestParseRequestFromEnvSupportsMultiplePaths(t *testing.T) {
	t.Setenv("INPUT_PATHS", "clusters/kube\nclusters/prod\nclusters/edge")

	req, err := ParseRequestFromEnv()
	if err != nil {
		t.Fatalf("ParseRequestFromEnv() error = %v", err)
	}

	want := []string{"clusters/kube", "clusters/prod", "clusters/edge"}
	if len(req.Paths) != len(want) {
		t.Fatalf("len(req.Paths) = %d, want %d", len(req.Paths), len(want))
	}

	for i := range want {
		if req.Paths[i] != want[i] {
			t.Fatalf("req.Paths[%d] = %q, want %q", i, req.Paths[i], want[i])
		}
	}
}

func TestParseRequestFromEnvSupportsClusterPrefixes(t *testing.T) {
	t.Setenv("INPUT_PATHS", "kube:clusters/kube\nkube:clusters/shared\nprod:clusters/prod")

	req, err := ParseRequestFromEnv()
	if err != nil {
		t.Fatalf("ParseRequestFromEnv() error = %v", err)
	}

	if req.ClusterPaths == nil {
		t.Fatal("expected ClusterPaths, got nil")
	}
	if len(req.ClusterPaths["kube"]) != 2 {
		t.Fatalf("len(kube) = %d, want 2", len(req.ClusterPaths["kube"]))
	}
	if len(req.ClusterPaths["prod"]) != 1 {
		t.Fatalf("len(prod) = %d, want 1", len(req.ClusterPaths["prod"]))
	}
}

func TestParseRequestFromEnvSupportsClustersInput(t *testing.T) {
	t.Setenv("INPUT_PATHS", "clusters/kube")
	t.Setenv("INPUT_CLUSTERS", "prod:clusters/prod\nedge:clusters/edge")

	req, err := ParseRequestFromEnv()
	if err != nil {
		t.Fatalf("ParseRequestFromEnv() error = %v", err)
	}

	if req.ClusterPaths == nil {
		t.Fatal("expected ClusterPaths, got nil")
	}
	if len(req.ClusterPaths[""]) != 1 || req.ClusterPaths[""][0] != "clusters/kube" {
		t.Fatalf("default cluster = %v, want [clusters/kube]", req.ClusterPaths[""])
	}
	if len(req.ClusterPaths["prod"]) != 1 {
		t.Fatalf("len(prod) = %d, want 1", len(req.ClusterPaths["prod"]))
	}
	if len(req.ClusterPaths["edge"]) != 1 {
		t.Fatalf("len(edge) = %d, want 1", len(req.ClusterPaths["edge"]))
	}
}

func TestParseRequestFromEnvSupportsHTMLReportInputs(t *testing.T) {
	t.Setenv("INPUT_HTML_REPORT", "true")
	t.Setenv("INPUT_HTML_REPORT_NAME", "custom-report")
	t.Setenv("INPUT_HTML_REPORT_RETENTION_DAYS", "14")
	t.Setenv("INPUT_HTML_REPORT_MAX_RESOURCE_DIFF_BYTES", "1234")

	req, err := ParseRequestFromEnv()
	if err != nil {
		t.Fatalf("ParseRequestFromEnv() error = %v", err)
	}

	if !req.HTMLReport {
		t.Fatal("HTMLReport = false, want true")
	}
	if req.HTMLReportName != "custom-report" {
		t.Fatalf("HTMLReportName = %q, want custom-report", req.HTMLReportName)
	}
	if req.HTMLReportRetentionDays != 14 {
		t.Fatalf("HTMLReportRetentionDays = %d, want 14", req.HTMLReportRetentionDays)
	}
	if req.HTMLReportMaxResourceDiffBytes != 1234 {
		t.Fatalf("HTMLReportMaxResourceDiffBytes = %d, want 1234", req.HTMLReportMaxResourceDiffBytes)
	}
}
