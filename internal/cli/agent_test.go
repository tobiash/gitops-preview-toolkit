package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/tobiash/gitops-preview-toolkit/pkg/agent"
)

func TestAgentRequests(t *testing.T) {
	s, err := agent.New(t.TempDir(), agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, tt := range []struct {
		name, op, input string
		failed          bool
	}{
		{"discover", "discover", "", false},
		{"plain", "", `{"operation":"discover"}`, false},
		{"unknown", "", `{"operation":"oops"}`, true},
		{"mismatch", "query", `{"operation":"discover"}`, true},
		{"trust", "", `{"operation":"discover","trusted":true}`, true},
		{"trailing", "", `{"operation":"discover"} {}`, true},
		{"null", "", `null`, true},
		{"empty", "", ``, true},
		{"oversize", "", strings.Repeat(" ", batchLimit+1), true},
		{"plain limit", "", `{"operation":"discover"}` + strings.Repeat(" ", 64<<10), true},
		{"empty batch", "", `{"requests":[]}`, true},
		{"batch op", "discover", `{"requests":[{"operation":"discover"}]}`, true},
		{"batch", "", `{"requests":[{"operation":"discover"},{"operation":"discover"}]}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runAgent(t.Context(), s, strings.NewReader(tt.input), &out, tt.op)
			if errors.Is(err, errAgentHandled) != tt.failed {
				t.Fatalf("error=%v output=%s", err, &out)
			}
			if !json.Valid(out.Bytes()) {
				t.Fatalf("not one JSON document: %s", &out)
			}
			validateAgentDocument(t, out.Bytes(), "output")
		})
	}
}

func TestAgentDiscoverFileInput(t *testing.T) {
	for _, source := range []string{os.DevNull, "/dev/zero", "pipe"} {
		t.Run(source, func(t *testing.T) {
			var input *os.File
			var err error
			if source == "pipe" {
				var writer *os.File
				input, writer, err = os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = writer.Close() })
				if _, err := writer.WriteString(`{"paths":["piped-root"],"recursive":true}`); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				input, err = os.Open(source)
				if os.IsNotExist(err) {
					t.Skip("character device unavailable")
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				if err := input.Close(); err != nil {
					t.Error(err)
				}
			})
			cmd := agentCmd()
			cmd.SetArgs([]string{"discover", "--root", t.TempDir()})
			cmd.SetIn(input)
			var output bytes.Buffer
			cmd.SetOut(&output)
			if err := cmd.ExecuteContext(t.Context()); err != nil {
				t.Fatalf("discover with %s: %v", source, err)
			}
			var response struct {
				Status string             `json:"status"`
				Data   agent.DiscoverData `json:"data"`
			}
			if err := json.Unmarshal(output.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Status != "success" {
				t.Fatalf("discover response = %s", &output)
			}
			if source == "pipe" && (len(response.Data.Paths) != 1 || response.Data.Paths[0] != "piped-root" || !response.Data.Recursive) {
				t.Fatalf("piped overrides were ignored: %+v", response.Data)
			}
			validateAgentDocument(t, output.Bytes(), "response")
		})
	}
}

func TestAgentBatchWorkflow(t *testing.T) {
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
	input := `{"requests":[
	{"operation":"preview","base":"worktree","target":"worktree","paths":["."]},
	{"operation":"query","id":"@0.afterId"},
	{"operation":"inspect","id":"@0.afterId","resourceId":"@1.items.0.resourceId"},
	{"operation":"check","id":"@0.afterId","requireRequests":true},
	{"operation":"release","id":"@0.id"}]}`
	validateAgentDocument(t, []byte(input), "input")
	var out bytes.Buffer
	if err := runAgent(t.Context(), s, strings.NewReader(input), &out, ""); err != nil {
		t.Fatalf("batch: %v: %s", err, &out)
	}
	validateAgentDocument(t, out.Bytes(), "batchResponse")
	var result agentBatchResponse
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 5 || !result.Complete {
		t.Fatalf("batch: %+v", result)
	}
	if result.Results[3].Data.(map[string]any)["verdict"] != "failed" {
		t.Fatalf("check: %+v", result.Results[3])
	}
	out.Reset()
	if err := runAgent(t.Context(), s, strings.NewReader(`{"requests":[{"operation":"discover"},{"operation":"query","id":"@0.missing"},{"operation":"discover"}]}`), &out, ""); !errors.Is(err, errAgentHandled) {
		t.Fatalf("error=%v", err)
	}
	validateAgentDocument(t, out.Bytes(), "batchResponse")
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 2 || result.FailedIndex == nil || *result.FailedIndex != 1 {
		t.Fatalf("stop on failure: %+v", result)
	}
}

func TestAgentReferences(t *testing.T) {
	results := []agent.Response{{Data: map[string]any{"id": "snapshot", "items": []any{map[string]any{"resourceId": "resource"}}, "count": 1}}}
	for _, ref := range []string{"@1.id", "@-1.id", "@0", "@0.count", "@0.items.1.resourceId", "@0.items.-1.resourceId", "@0.nope", "@0.id.eval()"} {
		req := agent.Request{ID: ref}
		if err := resolveAgentRefs(&req, results); err == nil {
			t.Errorf("accepted %q", ref)
		}
	}
	req := agent.Request{ID: "@0.id", BeforeID: "@0.id", AfterID: "@0.id", ResourceID: "@0.items.0.resourceId"}
	if err := resolveAgentRefs(&req, results); err != nil || req.ID != "snapshot" || req.ResourceID != "resource" {
		t.Fatalf("resolve: %+v %v", req, err)
	}
}

func TestAgentBatchLimits(t *testing.T) {
	root := t.TempDir()
	config, err := json.Marshal(map[string]any{"paths": []string{strings.Repeat("a", 40000)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".fmp.yaml"), config, 0o600); err != nil {
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
	requests := make([]map[string]string, 33)
	for i := range requests {
		requests[i] = map[string]string{"operation": "discover"}
	}
	for _, count := range []int{32, 33} {
		input, err := json.Marshal(map[string]any{"requests": requests[:count]})
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := runAgent(t.Context(), s, bytes.NewReader(input), &out, ""); !errors.Is(err, errAgentHandled) {
			t.Fatalf("count %d: error=%v", count, err)
		}
		if out.Len() > batchLimit || !json.Valid(out.Bytes()) {
			t.Fatalf("count %d: invalid or oversized response (%d)", count, out.Len())
		}
		validateAgentDocument(t, out.Bytes(), "output")
		if count == 32 {
			var result agentBatchResponse
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Error == nil || result.Error.Code != "ResponseTooLarge" || result.FailedIndex == nil || *result.FailedIndex != len(result.Results) {
				t.Fatalf("response bound: %+v", result)
			}
		}
	}
}

func TestAgentSchema(t *testing.T) {
	var out bytes.Buffer
	if err := writeAgentSchema(&out); err != nil {
		t.Fatal(err)
	}
	var doc struct{ Request, Response, BatchRequest, BatchResponse *jsonschema.Schema }
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		schema         *jsonschema.Schema
		valid, invalid string
	}{
		{doc.Request, `{"operation":"discover"}`, `{"operation":"discover","trusted":true}`},
		{doc.Response, `{"schemaVersion":"1","operation":"render","status":"success","complete":true,"diagnostics":[],"data":{"id":"snapshot","resourceCount":0}}`, `{}`},
		{doc.BatchRequest, `{"requests":[{"operation":"discover"}]}`, `{"requests":[]}`},
		{doc.BatchResponse, `{"schemaVersion":"1","status":"failure","complete":false,"results":[],"failedIndex":0,"error":{"code":"InvalidInput","message":"Invalid request."}}`, `{}`},
	} {
		r, err := tt.schema.Resolve(nil)
		if err != nil {
			t.Fatal(err)
		}
		var value any
		if err := json.Unmarshal([]byte(tt.valid), &value); err != nil {
			t.Fatal(err)
		}
		if err := r.Validate(value); err != nil {
			t.Errorf("valid %s: %v", tt.valid, err)
		}
		if err := json.Unmarshal([]byte(tt.invalid), &value); err != nil {
			t.Fatal(err)
		}
		if err := r.Validate(value); err == nil {
			t.Errorf("accepted invalid %s", tt.invalid)
		}
	}
}

func validateAgentDocument(t *testing.T, data []byte, name string) {
	t.Helper()
	var out bytes.Buffer
	if err := writeAgentSchema(&out); err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(document[name], &schema); err != nil {
		t.Fatal(err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	if err := resolved.Validate(value); err != nil {
		t.Fatalf("actual document violates published %s schema: %v", name, err)
	}
}
