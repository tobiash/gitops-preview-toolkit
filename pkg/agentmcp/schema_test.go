package agentmcp

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/tobiash/gitops-preview-toolkit/pkg/agent"
)

func TestSchemaSemantics(t *testing.T) {
	doc := Schema()
	for _, tt := range []struct {
		schema, value string
		valid         bool
	}{
		{"request", `{"operation":"discover"}`, true},
		{"request", `{"operation":"query","id":"@0.id","limit":0,"offset":0}`, true},
		{"request", `{"operation":"query","id":"id","limit":100}`, true},
		{"request", `{"operation":"query","id":"id","limit":101}`, false},
		{"request", `{"operation":"query","id":"id","limit":-1}`, false},
		{"request", `{"operation":"query","id":"id","offset":-1}`, false},
		{"request", `{"operation":"query","limit":-1}`, false},
		{"request", `{"operation":"query"}`, false},
		{"request", `{"operation":"inspect","id":"id"}`, false},
		{"request", `{"operation":"compare","beforeId":"id"}`, false},
		{"request", `{"operation":"not-an-operation"}`, false},
		{"request", `{"operation":"query","id":"id","query":{"action":"unexpected"}}`, false},
		{"request", `{"operation":"discover","trusted":true}`, false},
		{"request", `{"operation":"render","id":"id"}`, false},
		{"response", `{"schemaVersion":"999","operation":"query","status":"success","complete":true,"diagnostics":[],"data":42}`, false},
		{"response", `{"schemaVersion":"999","operation":"render","status":"success","complete":true,"diagnostics":[],"data":{"id":"id","resourceCount":0}}`, false},
		{"response", `{"schemaVersion":"1","operation":"render","status":"success","complete":true,"diagnostics":[],"data":42}`, false},
		{"response", `{"schemaVersion":"1","operation":"render","status":"success","complete":true,"diagnostics":[],"data":{"id":"id","resourceCount":0}}`, true},
		{"response", `{"schemaVersion":"1","operation":"render","status":"other","complete":true,"diagnostics":[],"data":{"id":"id","resourceCount":0}}`, false},
		{"response", `{"schemaVersion":"1","operation":"render","status":"failure","complete":false,"diagnostics":[],"error":{"code":"InvalidInput","message":"Invalid request."}}`, true},
		{"response", `{"schemaVersion":"1","operation":"render","status":"failure","complete":false,"diagnostics":[],"data":42,"error":{"code":"InvalidInput","message":"Invalid request."}}`, false},
		{"response", `{"schemaVersion":"1","operation":"check","status":"success","complete":true,"diagnostics":[],"data":{"id":"id","verdict":"failed"}}`, true},
		{"response", `{"schemaVersion":"1","operation":"check","status":"success","complete":true,"diagnostics":[],"data":{"id":"id","verdict":"other"}}`, false},
		{"response", `{"schemaVersion":"1","operation":"check","status":"success","complete":true,"diagnostics":[],"data":{"id":"id","verdict":"passed","count":{"count":-1}}}`, false},
		{"response", `{"schemaVersion":"1","operation":"check","status":"success","complete":true,"diagnostics":[],"data":{"id":"id","verdict":"passed","resources":42}}`, false},
		{"batchRequest", `{"requests":[{"operation":"query","id":"@0.id","limit":-1}]}`, false},
		{"batchRequest", `{"requests":[{"operation":"render"},{"operation":"query","id":"@0.id"}]}`, true},
		{"batchResponse", `{"schemaVersion":"999","status":"failure","complete":false,"results":[],"failedIndex":0,"error":{"code":"InvalidInput","message":"Invalid request."}}`, false},
		{"batchResponse", `{"schemaVersion":"1","status":"failure","complete":false,"results":[],"failedIndex":32,"error":{"code":"InvalidInput","message":"Invalid request."}}`, false},
	} {
		t.Run(tt.schema+"/"+tt.value, func(t *testing.T) {
			s := resolve(doc[tt.schema].(*jsonschema.Schema))
			err := validateJSON([]byte(tt.value), s)
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v want=%v", err == nil, tt.valid)
			}
		})
	}
	for _, spec := range specifications() {
		if len(spec.output.OneOf) != 2 || spec.output.OneOf[0].Properties["data"].Type != "object" {
			t.Errorf("%s lacks typed success/failure data schemas", spec.operation)
		}
		var output map[string]any
		data, err := json.Marshal(Failure(spec.operation, "InvalidInput", "Invalid arguments."))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &output); err != nil {
			t.Fatal(err)
		}
		if err := spec.outputResolved.Validate(output); err != nil {
			t.Errorf("%s failure: %v", spec.operation, err)
		}
	}
}

func TestLogicalSummarySchemas(t *testing.T) {
	for _, spec := range specifications() {
		if spec.operation != "query" && spec.operation != "inspect" {
			continue
		}
		t.Run(spec.operation, func(t *testing.T) {
			data := spec.output.OneOf[0].Properties["data"]
			summary := data
			if spec.operation == "query" {
				summary = data.Properties["items"].Items
			}
			if summary.Properties["logicalId"].Type != "string" || slices.Contains(summary.Required, "logicalId") {
				t.Fatal("logicalId must be an optional string in generated summaries")
			}
			for _, logicalID := range []string{"", "logical:parent/slot"} {
				resource := agent.ResourceSummary{ResourceID: "opaque", LogicalID: logicalID, Cluster: "east", APIVersion: "example.io/v1", Kind: "Bucket"}
				var result any = agent.InspectData{ID: "snapshot", ResourceSummary: resource, New: map[string]any{"kind": "Bucket"}}
				if spec.operation == "query" {
					result = agent.QueryData{ID: "snapshot", Items: []agent.ResourceSummary{resource}, Total: 1}
				}
				response := agent.Response{SchemaVersion: "1", Operation: spec.operation, Status: "success", Complete: true, Data: result, Diagnostics: []agent.Diagnostic{}}
				encoded, err := json.Marshal(response)
				if err != nil || validateJSON(encoded, spec.outputResolved) != nil {
					t.Fatalf("logical summary rejected by published SDK schema: %s, %v", encoded, err)
				}
			}
		})
	}
}

func TestDecodeRequestUsesPublishedSchema(t *testing.T) {
	for _, tt := range []struct {
		input, operation string
		valid            bool
	}{
		{`{}`, "discover", true},
		{`{"operation":"discover"}`, "discover", true},
		{`{"operation":"discover"}`, "query", false},
		{`{"operation":"query","id":"snapshot","limit":-1}`, "", false},
		{`{"operation":"query","id":"snapshot","limit":100}`, "", true},
		{`{"operation":"query","id":"snapshot","query":{"action":"CANARY"}}`, "", false},
		{`{"operation":"render","id":"CANARY"}`, "", false},
	} {
		_, err := DecodeRequest([]byte(tt.input), tt.operation)
		if (err == nil) != tt.valid {
			t.Errorf("DecodeRequest(%s, %s) error=%v", tt.input, tt.operation, err)
		}
	}
}
