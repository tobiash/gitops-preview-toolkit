//go:build integration

package plugins_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tobiash/gitops-preview-toolkit/pkg/plugin"
	"github.com/tobiash/gitops-preview-toolkit/pkg/pluginhost"
)

// TestLivePluginChain exercises both persistent gRPC subprocesses, the released
// Crossplane reconciler, and the real upstream function. No engine is substituted.
func TestLivePluginChain(t *testing.T) {
	t.Run("named-chain", func(t *testing.T) { runLivePluginChain(t, false) })
	t.Run("logical-chain", func(t *testing.T) { runLivePluginChain(t, true) })
}

func runLivePluginChain(t *testing.T, includeLogical bool) {
	t.Helper()
	engine := os.Getenv("CROSSPLANE_TEST_ENGINE")
	function := os.Getenv("CROSSPLANE_TEST_TEMPLATING_BINARY")
	flux := os.Getenv("PLUGIN_TEST_FLUX_BINARY")
	crossplane := os.Getenv("PLUGIN_TEST_CROSSPLANE_BINARY")
	if engine == "" || function == "" || flux == "" || crossplane == "" {
		t.Skip("run tests/plugins/run-live.sh to install/build the real engines and plugins")
	}
	version, err := exec.CommandContext(t.Context(), engine, "--version").CombinedOutput()
	if err != nil || strings.TrimSpace(string(version)) != "v2.4.2" {
		t.Fatalf("released engine version: %s, %v", version, err)
	}
	root, err := os.MkdirTemp("/tmp/opencode", "plugin-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(filepath.Join(root, "function.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), function, "--insecure", "--address="+target)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = log.Close()
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		connection, err := net.DialTimeout("tcp", target, 100*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("function failed to listen: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cfg, err := json.Marshal(map[string]any{
		"engineBinary": engine, "runtime": "Development",
		"developmentTargets": map[string]string{"templating": target}, "timeout": "45s",
	})
	if err != nil {
		t.Fatal(err)
	}
	host, err := pluginhost.New([]plugin.Command{
		{Name: "flux", Command: flux, Config: json.RawMessage(`{"helm":true,"resolveGit":true}`)},
		{Name: "crossplane", Command: crossplane, Config: cfg},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := host.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture, err := os.ReadFile("fixtures/chain/manifests/inputs.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !includeLogical {
		fixture = []byte(strings.ReplaceAll(string(fixture), "{{ else }}", "{{ else if false }}"))
	}
	base, head := filepath.Join(root, "base"), filepath.Join(root, "head")
	for _, directory := range []string{base, head} {
		if err := os.CopyFS(directory, os.DirFS("fixtures/chain")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "manifests", "inputs.yaml"), fixture, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write := func(directory, marker string, remove bool) {
		t.Helper()
		data := strings.ReplaceAll(string(fixture), "marker: base", "marker: "+marker)
		if remove {
			data = strings.Split(data, "---\napiVersion: live.example.org/v1\nkind: App")[0]
		}
		if err := os.WriteFile(filepath.Join(directory, "manifests", "inputs.yaml"), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	render := func(directory, marker string, removed bool) *pluginhost.Result {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		result, err := host.Render(ctx, plugin.OpenRequest{
			Root: directory, Paths: []string{"manifests"}, Fresh: false,
		})
		if err != nil || !result.Complete || len(result.Diagnostics) != 0 {
			t.Fatalf("real subprocess render: complete=%v diagnostics=%+v err=%v", result.Complete, result.Diagnostics, err)
		}
		wantCount := 10
		wantLogical := 1
		if !includeLogical {
			wantCount, wantLogical = 9, 0
		}
		if removed {
			wantCount = 4
		}
		if len(result.Resources) != wantCount {
			t.Fatalf("inventory count = %d, want %d: %+v", len(result.Resources), wantCount, result.Resources)
		}
		logical, named, helm, child := 0, 0, 0, 0
		for _, resource := range result.Resources {
			object, err := plugin.Object(resource)
			if err != nil {
				t.Fatal(err)
			}
			metadata, _ := object["metadata"].(map[string]any)
			if resource.Logical {
				logical++
				if metadata["name"] != nil && metadata["name"] != "" {
					t.Fatalf("logical output received synthetic name: %+v", object)
				}
				if !strings.HasPrefix(resource.ID, "logical:") {
					t.Fatalf("logical ID: %s", resource.ID)
				}
			}
			switch object["kind"] {
			case "ConfigMap":
				data, _ := object["data"].(map[string]any)
				if data["marker"] != marker || data["observedCount"] != "0" {
					t.Fatalf("changed value or synthetic observed state: %+v", object)
				}
				if !resource.Logical {
					named++
					if data["defaulted"] != "real-default" {
						t.Fatalf("XRD default absent: %+v", object)
					}
				}
			case "HelmRelease":
				helm++
			case "App":
				if metadata["name"] == "leaf" {
					child++
				}
			}
		}
		if !removed && (logical != wantLogical || named != 2 || helm != 1 || child != 1) {
			t.Fatalf("chain incomplete: logical=%d named=%d helm=%d child=%d", logical, named, helm, child)
		}
		if removed && (logical != 0 || named != 0 || helm != 0 || child != 0) {
			t.Fatalf("stale transitive output: logical=%d named=%d helm=%d child=%d", logical, named, helm, child)
		}
		return result
	}
	first := render(base, "base", false)
	repeat := render(base, "base", false)
	if !reflect.DeepEqual(first.Resources, repeat.Resources) {
		t.Fatal("identical render changed inventory")
	}
	write(head, "head", false)
	headed := render(head, "head", false)
	if len(first.Resources) != len(headed.Resources) || reflect.DeepEqual(first.Resources, headed.Resources) {
		t.Fatal("same-size changed values were lost")
	}
	baseAgain := render(base, "base", false)
	if !reflect.DeepEqual(first.Resources, baseAgain.Resources) {
		t.Fatal("head contaminated base session")
	}
	write(base, "edit", false)
	render(base, "edit", false)
	write(base, "edit", true)
	render(base, "", true)
	render(head, "head", false)
	t.Logf("verified real v2.4.2 engine + v0.12.0 function via persistent gRPC plugins at %s", target)
}

// TestLiveCLI validates the shipped core and sibling-plugin discovery, including
// real desired-state rendering and both user-facing comparison formats.
func TestLiveCLI(t *testing.T) {
	core, engine, function := os.Getenv("PLUGIN_TEST_CORE_BINARY"), os.Getenv("CROSSPLANE_TEST_ENGINE"), os.Getenv("CROSSPLANE_TEST_TEMPLATING_BINARY")
	if core == "" || engine == "" || function == "" {
		t.Skip("set PLUGIN_TEST_CORE_BINARY and the real Crossplane/function binaries")
	}
	root, err := os.MkdirTemp("/tmp/opencode", "plugin-cli-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	target := startLiveFunction(t, root, function)
	fixture, err := os.ReadFile("fixtures/chain/manifests/inputs.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// Two identically typed unnamed resources must remain separate in JSON and
	// HTML. Neither their logical key nor a generated name is Kubernetes metadata.
	second := `          ---
          apiVersion: v1
          kind: ConfigMap
          metadata:
            generateName: leaf-logical-
            namespace: workload
            annotations:
              gotemplating.fn.crossplane.io/composition-resource-name: logical-two
          data:
            marker: {{ $xr.spec.marker | quote }}
            observedCount: {{ len (.observed.resources | default dict) | quote }}
`
	fixture = []byte(strings.Replace(string(fixture), "          {{ end }}", second+"          {{ end }}", 1))
	before, after := filepath.Join(root, "before"), filepath.Join(root, "after")
	for _, dir := range []string{before, after} {
		if err := os.CopyFS(dir, os.DirFS("fixtures/chain")); err != nil {
			t.Fatal(err)
		}
		marker := "base"
		if dir == after {
			marker = "head"
		}
		data := strings.ReplaceAll(string(fixture), "marker: base", "marker: "+marker)
		if err := os.WriteFile(filepath.Join(dir, "manifests", "inputs.yaml"), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".gitops-preview.yaml"), []byte("paths: [manifests]\nresolve-git: true\ncrossplane:\n  timeout: 3s\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	trusted := []string{"--crossplane", "--crossplane-engine", engine, "--crossplane-function", "templating=" + target}
	run := func(t *testing.T, args ...string) ([]byte, string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, core, args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "TMPDIR="+root)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		assertNoLiveChildren(t, core, engine)
		return stdout.Bytes(), stderr.String(), err
	}
	decode := func(t *testing.T, data []byte) map[string]any {
		t.Helper()
		var doc map[string]any
		dec := json.NewDecoder(bytes.NewReader(data))
		if err := dec.Decode(&doc); err != nil {
			t.Fatalf("JSON document: %v\n%s", err, data)
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			t.Fatalf("stdout must contain exactly one JSON document: %v\n%s", err, data)
		}
		return doc
	}
	t.Run("render", func(t *testing.T) {
		out, stderr, err := run(t, append([]string{"render", before, "--output", "json"}, trusted...)...)
		if err != nil {
			t.Fatalf("CLI render: %v stderr=%s stdout=%s", err, stderr, out)
		}
		doc := decode(t, out)
		items, ok := doc["items"].([]any)
		if !ok || len(items) != 11 || doc["kind"] != "List" || doc["apiVersion"] != "v1" {
			t.Fatalf("render schema/inventory: %s", out)
		}
		logical := 0
		for _, item := range items {
			object := item.(map[string]any)
			if object["kind"] != "ConfigMap" {
				continue
			}
			metadata := object["metadata"].(map[string]any)
			data := object["data"].(map[string]any)
			if data["marker"] != "base" || data["observedCount"] != "0" {
				t.Fatalf("actual function state: %+v", object)
			}
			if metadata["generateName"] == "leaf-logical-" {
				logical++
				if metadata["name"] != nil && metadata["name"] != "" {
					t.Fatalf("synthetic render name: %+v", metadata)
				}
			}
		}
		if logical != 2 {
			t.Fatalf("logical rendered count=%d", logical)
		}
	})
	var logicalKeys []string
	t.Run("diff-json", func(t *testing.T) {
		out, stderr, err := run(t, append([]string{"diff", "path:" + before, "path:" + after, "--output", "json"}, trusted...)...)
		if err != nil {
			t.Fatalf("CLI diff: %v stderr=%s stdout=%s", err, stderr, out)
		}
		doc := decode(t, out)
		if doc["complete"] != true {
			t.Fatalf("incomplete diff: %s", out)
		}
		changes, ok := doc["changes"].([]any)
		if !ok || len(changes) != 7 {
			t.Fatalf("diff changes: %s", out)
		}
		seen := map[string]bool{}
		for _, raw := range changes {
			change := raw.(map[string]any)
			if change["action"] != "modified" {
				t.Fatalf("same-size update misclassified: %+v", change)
			}
			key, _ := change["logicalId"].(string)
			if key == "" {
				continue
			}
			if !strings.HasPrefix(key, "logical:") || seen[key] {
				t.Fatalf("logical diff key collision: %s", key)
			}
			seen[key] = true
			logicalKeys = append(logicalKeys, key)
			ref := change["objectRef"].(map[string]any)
			if ref["name"] != "" {
				t.Fatalf("synthetic diff name: %+v", ref)
			}
			for _, side := range []string{"old", "new"} {
				object := change[side].(map[string]any)
				metadata := object["metadata"].(map[string]any)
				if metadata["name"] != nil && metadata["name"] != "" {
					t.Fatalf("synthetic %s metadata: %+v", side, metadata)
				}
				want := "base"
				if side == "new" {
					want = "head"
				}
				if object["data"].(map[string]any)["marker"] != want {
					t.Fatalf("%s value lost: %+v", side, object)
				}
			}
		}
		if len(logicalKeys) != 2 {
			t.Fatalf("logical diff keys: %v", logicalKeys)
		}
	})
	t.Run("diff-html", func(t *testing.T) {
		out, stderr, err := run(t, append([]string{"diff", "path:" + before, "path:" + after, "--html", "--html-open=false"}, trusted...)...)
		if err != nil {
			t.Fatalf("CLI HTML diff: %v stderr=%s stdout=%s", err, stderr, out)
		}
		files, err := filepath.Glob(filepath.Join(root, "fmp-html-report-*", "index.html"))
		if err != nil || len(files) != 1 {
			t.Fatalf("HTML files=%v err=%v stderr=%s", files, err, stderr)
		}
		html, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		opening := `<script type="application/json" id="fmp-report-data">`
		_, payload, ok := strings.Cut(string(html), opening)
		if !ok {
			t.Fatal("HTML lacks embedded report JSON")
		}
		payload, _, ok = strings.Cut(payload, "</script>")
		if !ok {
			t.Fatal("HTML report JSON not terminated")
		}
		doc := decode(t, []byte(payload))
		resources, ok := doc["resources"].([]any)
		if !ok || len(resources) != 7 {
			t.Fatalf("HTML resource schema: %s", payload)
		}
		seen := map[string]bool{}
		htmlLogical := map[string]bool{}
		for _, raw := range resources {
			resource := raw.(map[string]any)
			id, _ := resource["id"].(string)
			if id == "" || seen[id] {
				t.Fatalf("HTML resource key collision: %+v", resource)
			}
			seen[id] = true
			if key, _ := resource["logicalId"].(string); key != "" {
				if resource["name"] != "" || !strings.HasSuffix(id, "|logical|"+key) || htmlLogical[key] {
					t.Fatalf("HTML synthetic name/key: %+v", resource)
				}
				htmlLogical[key] = true
			}
		}
		if len(htmlLogical) != 2 {
			t.Fatalf("HTML logical key count=%d", len(htmlLogical))
		}
		for _, key := range logicalKeys {
			if !htmlLogical[key] {
				t.Fatalf("HTML lost JSON logical key %s", key)
			}
		}
	})
	t.Run("raw-root-config", func(t *testing.T) {
		raw := filepath.Join(root, "raw")
		if err := os.CopyFS(raw, os.DirFS("fixtures/chain")); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(filepath.Join(raw, "manifests")); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(raw, "inputs.yaml"), fixture, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(raw, ".gitops-preview.yaml"), []byte("paths: [.]\nresolve-git: true\ncrossplane:\n  timeout: 3s\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, stderr, err := run(t, append([]string{"render", raw, "--output", "json"}, trusted...)...)
		if err != nil {
			t.Fatalf("raw-root config: %v stderr=%s stdout=%s", err, stderr, out)
		}
		if items, _ := decode(t, out)["items"].([]any); len(items) != 11 {
			t.Fatalf("raw-root inventory: %s", out)
		}
	})
	t.Run("unavailable-function", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		unavailable := listener.Addr().String()
		_ = listener.Close()
		out, stderr, err := run(t, "render", before, "--crossplane", "--crossplane-engine", engine, "--crossplane-function", "templating="+unavailable, "--output", "json")
		if err == nil {
			t.Fatalf("unavailable function succeeded: %s", out)
		}
		doc := decode(t, out)
		if doc["complete"] != false || doc["error"] == nil {
			t.Fatalf("failure must be incomplete JSON: stderr=%s stdout=%s", stderr, out)
		}
		if items, _ := doc["items"].([]any); len(items) != 0 {
			t.Fatalf("partial inventory leaked: %s", out)
		}
	})
}

func startLiveFunction(t *testing.T, root, binary string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	target := listener.Addr().String()
	_ = listener.Close()
	log, err := os.Create(filepath.Join(root, "function.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), binary, "--insecure", "--address="+target)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = log.Close() })
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", target, 100*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return target
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("real function did not listen within 10s")
	return ""
}

func assertNoLiveChildren(t *testing.T, core, engine string) {
	t.Helper()
	// Linux /proc gives an independent check that command exit cleaned up the
	// sibling plugin and reconciler processes, even on function RPC failure.
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	owned := map[string]bool{engine: true, filepath.Join(filepath.Dir(core), "gitops-preview-flux"): true, filepath.Join(filepath.Dir(core), "gitops-preview-crossplane"): true}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil {
			continue
		}
		binary, _, _ := strings.Cut(string(data), "\x00")
		if owned[binary] {
			t.Errorf("CLI leaked owned process pid=%s argv=%q", entry.Name(), data)
		}
	}
}

// TestLiveDockerRuntime is opt-in because it downloads an actual function image.
// The runner supplies an owned Podman Docker-compatible socket. Omitting Runtime
// and DevelopmentTargets intentionally verifies the production Docker default.
func TestLiveDockerRuntime(t *testing.T) {
	if os.Getenv("PLUGIN_TEST_DOCKER") != "1" {
		t.Skip("set PLUGIN_TEST_DOCKER=1 when running tests/plugins/run-live.sh")
	}
	engine, flux, crossplane := os.Getenv("CROSSPLANE_TEST_ENGINE"), os.Getenv("PLUGIN_TEST_FLUX_BINARY"), os.Getenv("PLUGIN_TEST_CROSSPLANE_BINARY")
	if engine == "" || flux == "" || crossplane == "" {
		t.Fatal("Docker verification requires real engine and plugin binaries")
	}
	socket, ok := strings.CutPrefix(os.Getenv("DOCKER_HOST"), "unix://")
	if !ok {
		t.Fatal("Docker verification requires an owned Unix API endpoint")
	}
	root, err := os.MkdirTemp("/tmp/opencode", "plugin-docker-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.CopyFS(filepath.Join(root, "fixture"), os.DirFS("fixtures/chain")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "fixture", "manifests", "inputs.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.ReplaceAll(data, []byte("xpkg.crossplane.io/crossplane-contrib/function-go-templating:v0.12.0"), []byte("ghcr.io/crossplane-contrib/function-go-templating:v0.12.0"))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	t.Cleanup(transport.CloseIdleConnections)
	api := func(method, path string, dest any) {
		t.Helper()
		req, err := http.NewRequestWithContext(context.WithoutCancel(t.Context()), method, "http://localhost"+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("Docker API %s %s: %d %s", method, path, response.StatusCode, body)
		}
		if dest != nil {
			if err := json.NewDecoder(response.Body).Decode(dest); err != nil {
				t.Fatal(err)
			}
		}
	}
	ownedContainers := func() []string {
		t.Helper()
		var containers []struct {
			ID    string   `json:"Id"`
			Names []string `json:"Names"`
		}
		api("GET", "/containers/json?all=true", &containers)
		var owned []string
		for _, container := range containers {
			candidate := false
			for _, name := range container.Names {
				if strings.HasPrefix(strings.TrimPrefix(name, "/"), "fmp-crossplane-") {
					candidate = true
				}
			}
			if !candidate {
				continue
			}
			var inspect struct {
				Config struct {
					Env []string `json:"Env"`
				} `json:"Config"`
			}
			api("GET", "/containers/"+container.ID+"/json", &inspect)
			for _, env := range inspect.Config.Env {
				if env == "PLUGIN_LIVE_TEST="+root {
					owned = append(owned, container.ID)
				}
			}
		}
		return owned
	}
	// Emergency removal is scoped to our unique environment marker, never user
	// containers or images, and still reports a cleanup regression as a failure.
	t.Cleanup(func() {
		for _, id := range ownedContainers() {
			t.Errorf("runtime leaked owned container %s", id)
			api("DELETE", "/containers/"+id+"?force=true", nil)
		}
	})
	config, err := json.Marshal(map[string]any{"engineBinary": engine, "timeout": "45s", "dockerEnv": map[string]string{"PLUGIN_LIVE_TEST": root}})
	if err != nil {
		t.Fatal(err)
	}
	host, err := pluginhost.New([]plugin.Command{
		{Name: "flux", Command: flux, Config: json.RawMessage(`{"helm":true,"resolveGit":true}`)},
		{Name: "crossplane", Command: crossplane, Config: config},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := host.Close(); err != nil {
			t.Error(err)
		}
	})
	result, err := host.Render(t.Context(), plugin.OpenRequest{Root: filepath.Join(root, "fixture"), Paths: []string{"manifests"}})
	if err != nil || !result.Complete || len(result.Diagnostics) != 0 {
		t.Fatalf("default Docker render: complete=%v diagnostics=%+v err=%v", result.Complete, result.Diagnostics, err)
	}
	if len(result.Resources) != 10 {
		t.Fatalf("default Docker inventory count=%d", len(result.Resources))
	}
	logical, named := 0, 0
	for _, resource := range result.Resources {
		object, err := plugin.Object(resource)
		if err != nil {
			t.Fatal(err)
		}
		if object["kind"] != "ConfigMap" {
			continue
		}
		data := object["data"].(map[string]any)
		if data["marker"] != "base" || data["observedCount"] != "0" {
			t.Fatalf("container function state: %+v", object)
		}
		if resource.Logical {
			logical++
			metadata := object["metadata"].(map[string]any)
			if metadata["name"] != nil && metadata["name"] != "" {
				t.Fatalf("Docker synthetic name: %+v", object)
			}
		} else {
			named++
		}
	}
	if logical != 1 || named != 2 {
		t.Fatalf("Docker chain outputs logical=%d named=%d", logical, named)
	}
	if ids := ownedContainers(); len(ids) != 0 {
		t.Fatalf("CloseRender left owned containers: %v", ids)
	}
	if err := host.Close(); err != nil {
		t.Fatal(err)
	}
	t.Run("cli-default", func(t *testing.T) {
		core := os.Getenv("PLUGIN_TEST_CORE_BINARY")
		if core == "" {
			t.Skip("set PLUGIN_TEST_CORE_BINARY to verify the CLI Docker default")
		}
		var before []struct {
			ID string `json:"Id"`
		}
		api("GET", "/containers/json?all=true", &before)
		known := map[string]bool{}
		for _, container := range before {
			known[container.ID] = true
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		command := exec.CommandContext(ctx, core, "render", filepath.Join(root, "fixture"), "--path", "manifests", "--resolve-git", "--crossplane", "--crossplane-engine", engine, "--output", "json")
		command.Env = append(os.Environ(), "TMPDIR="+root)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		out, err := command.Output()
		assertNoLiveChildren(t, core, engine)
		if err != nil {
			t.Fatalf("CLI default Docker runtime: %v stderr=%s stdout=%s", err, &stderr, out)
		}
		var doc struct {
			Kind  string           `json:"kind"`
			Items []map[string]any `json:"items"`
		}
		decoder := json.NewDecoder(bytes.NewReader(out))
		if err := decoder.Decode(&doc); err != nil {
			t.Fatalf("Docker CLI JSON: %v stdout=%s", err, out)
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			t.Fatalf("Docker CLI emitted trailing output: %v", err)
		}
		if doc.Kind != "List" || len(doc.Items) != 10 {
			t.Fatalf("Docker CLI inventory: %s", out)
		}
		logical := 0
		for _, object := range doc.Items {
			if object["kind"] != "ConfigMap" {
				continue
			}
			metadata := object["metadata"].(map[string]any)
			if metadata["generateName"] == "leaf-logical-" {
				logical++
				if metadata["name"] != nil && metadata["name"] != "" {
					t.Fatalf("Docker CLI invented a name: %+v", metadata)
				}
			}
		}
		if logical != 1 {
			t.Fatalf("Docker CLI logical count=%d", logical)
		}
		var after []struct {
			ID    string   `json:"Id"`
			Names []string `json:"Names"`
		}
		api("GET", "/containers/json?all=true", &after)
		for _, container := range after {
			if known[container.ID] {
				continue
			}
			for _, name := range container.Names {
				if strings.HasPrefix(strings.TrimPrefix(name, "/"), "fmp-crossplane-") {
					t.Errorf("Docker CLI left a new function container: %s %s", container.ID, name)
				}
			}
		}
	})
	t.Log("real v0.12.0 container function rendered through the default Docker runtime and cleaned up")
}
