package agentmcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tobiash/gitops-preview-toolkit/pkg/agent"
)

func TestTools(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "pod.yaml"), []byte("apiVersion: v1\nkind: Pod\nmetadata:\n  name: example\nspec:\n  containers:\n  - name: app\n    image: app:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := agent.New(root, agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	server := NewServer(s, "test")
	a, b := mcp.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), a, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ss.Close(); err != nil {
			t.Error(err)
		}
	})
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(t.Context(), b, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cs.Close(); err != nil {
			t.Error(err)
		}
	})
	list, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 8 {
		t.Fatalf("tools = %d, want 8", len(list.Tools))
	}
	want := map[string]bool{}
	for _, op := range []string{"discover", "render", "preview", "query", "inspect", "compare", "check", "release"} {
		want["manifest_"+op] = true
	}
	for _, tool := range list.Tools {
		if !want[tool.Name] || tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Fatalf("unexpected tool: %+v", tool)
		}
		delete(want, tool.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing tools: %v", want)
	}
	for _, args := range []map[string]any{{"trusted": true}, {"operation": "release"}, {"paths": 3}, {"recursive": "true"}} {
		if result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "manifest_discover", Arguments: args}); err != nil || !result.IsError {
			t.Errorf("invalid arguments: result=%+v err=%v", result, err)
		}
	}
	for _, args := range []map[string]any{{}, {"id": "missing", "query": map[string]any{"trusted": true}}, {"id": 123}} {
		if result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "manifest_query", Arguments: args}); err != nil || !result.IsError {
			t.Errorf("invalid query arguments: result=%+v err=%v", result, err)
		}
	}
	for _, tt := range []struct {
		op   string
		args any
	}{
		{"discover", map[string]any{"paths": "credential-CANARY"}},
		{"discover", map[string]any{"credential-CANARY": "secret"}},
		{"discover", map[string]any{"paths": strings.Repeat("credential-CANARY", 100000)}},
		{"discover", map[string]any{"paths": []string{strings.Repeat("credential-CANARY", 100000)}}},
		{"query", map[string]any{"id": "credential-CANARY", "limit": -1}},
		{"query", map[string]any{"id": "credential-CANARY", "query": map[string]any{"action": "credential-CANARY"}}},
	} {
		result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "manifest_" + tt.op, Arguments: tt.args})
		if err != nil {
			t.Fatalf("expected sanitized tool failure, got protocol error (%d bytes)", len(err.Error()))
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if !result.IsError || len(encoded) > 1024 || strings.Contains(string(encoded), "CANARY") {
			t.Fatalf("unsafe failure: isError=%v bytes=%d", result.IsError, len(encoded))
		}
		data, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range specifications() {
			if spec.operation == tt.op && validateJSON(data, spec.outputResolved) != nil {
				t.Fatalf("failure violates %s schema: %s", tt.op, data)
			}
		}
	}
	call := func(op string, args any) agent.Response {
		t.Helper()
		r, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "manifest_" + op, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(r.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		var response agent.Response
		if err := json.Unmarshal(data, &response); err != nil {
			t.Fatal(err)
		}
		if r.IsError != (response.Status == "failure") {
			t.Fatalf("IsError=%v response=%+v", r.IsError, response)
		}
		if len(r.Content) != 1 || !json.Valid([]byte(r.Content[0].(*mcp.TextContent).Text)) {
			t.Fatalf("missing JSON fallback: %+v", r)
		}
		for _, spec := range specifications() {
			if spec.operation == op && validateJSON(data, spec.outputResolved) != nil {
				t.Fatalf("%s response violates output schema", op)
			}
		}
		return response
	}
	if r := call("discover", map[string]any{}); r.Status != "success" {
		t.Fatalf("discover: %+v", r)
	}
	if r := call("query", map[string]any{"id": "missing"}); r.Status != "failure" {
		t.Fatalf("missing handle: %+v", r)
	}
	render := call("render", map[string]any{"paths": []string{"."}})
	if render.Status != "success" {
		t.Fatalf("render: %+v", render)
	}
	id := render.Data.(map[string]any)["id"].(string)
	page := call("query", map[string]any{"id": id})
	if page.Status != "success" {
		t.Fatalf("query: %+v", page)
	}
	items := page.Data.(map[string]any)["items"].([]any)
	resourceID := items[0].(map[string]any)["resourceId"]
	if r := call("inspect", map[string]any{"id": id, "resourceId": resourceID, "fields": []string{"/metadata/name"}}); r.Status != "success" {
		t.Fatalf("inspect: %+v", r)
	}
	if r := call("compare", map[string]any{"beforeId": id, "afterId": id}); r.Status != "success" {
		t.Fatalf("compare: %+v", r)
	}
	if r := call("preview", map[string]any{"base": "worktree", "target": "worktree", "paths": []string{"."}}); r.Status != "success" {
		t.Fatalf("preview: %+v", r)
	}
	check := call("check", map[string]any{"id": id, "requireRequests": true})
	if check.Status != "success" || check.Data.(map[string]any)["verdict"] != "failed" {
		t.Fatalf("policy check: %+v", check)
	}
	if r := call("release", map[string]any{"id": id}); r.Status != "success" {
		t.Fatalf("release: %+v", r)
	}
	if r := call("query", map[string]any{"id": id}); r.Status != "failure" {
		t.Fatalf("released handle: %+v", r)
	}
}
