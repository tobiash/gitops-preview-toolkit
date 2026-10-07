package agentmcp

import (
	"encoding/json"
	"errors"
	"slices"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/tobiash/gitops-preview-toolkit/pkg/agent"
)

const maxArgumentBytes = 64 << 10

type successResponse[T any] struct {
	SchemaVersion string             `json:"schemaVersion"`
	Operation     string             `json:"operation"`
	Status        string             `json:"status"`
	Complete      bool               `json:"complete"`
	Data          T                  `json:"data"`
	Diagnostics   []agent.Diagnostic `json:"diagnostics"`
}

type specification struct {
	operation, description                         string
	readOnly                                       bool
	input, request, output                         *jsonschema.Schema
	inputResolved, requestResolved, outputResolved *jsonschema.Resolved
}

// These schemas are immutable after construction; public discovery returns copies.
var specifications = sync.OnceValue(func() []*specification {
	return []*specification{
		makeSpec[discoverInput, agent.DiscoverData]("discover", "Discover workspace configuration and startup capabilities.", true),
		makeSpec[renderInput, agent.RenderData]("render", "Render and retain an immutable snapshot. Startup trust may permit external capabilities.", false),
		makeSpec[previewInput, agent.PreviewData]("preview", "Render a comparison and retain its snapshots. Startup trust may permit external capabilities.", false),
		makeSpec[queryInput, agent.QueryData]("query", "Read a bounded page of retained resource identities.", true),
		makeSpec[inspectInput, agent.InspectData]("inspect", "Read a redacted retained resource or selected JSON-pointer fields.", true),
		makeSpec[compareInput, agent.PreviewData]("compare", "Compare retained snapshots and allocate a comparison handle.", false),
		makeSpec[checkInput, agent.CheckData]("check", "Evaluate retained facts; failed policy verdicts are not operational errors. Trusted policies may use external capabilities.", false),
		makeSpec[releaseInput, agent.ReleaseData]("release", "Delete a retained session handle, not user files.", false),
	}
})

func makeSpec[In, Data any](op, description string, readOnly bool) *specification {
	input := infer[In]()
	for _, field := range []string{"id", "beforeId", "afterId", "resourceId"} {
		if s := input.Properties[field]; s != nil {
			s.MinLength = new(1)
		}
	}
	if s := input.Properties["paths"]; s != nil {
		s.MaxItems = new(1000)
	}
	if s := input.Properties["fields"]; s != nil {
		s.MaxItems = new(100)
	}
	if s := input.Properties["limit"]; s != nil {
		s.Maximum = new(float64(100))
	}
	if s := input.Properties["query"]; s != nil {
		s.Properties["action"].Enum = []any{"", "added", "modified", "deleted"}
	}
	request := input.CloneSchemas()
	request.Properties["operation"] = enum(op)
	request.Required = append(slices.Clone(request.Required), "operation")
	success := infer[successResponse[Data]]()
	success.Properties["schemaVersion"] = enum("1")
	success.Properties["operation"] = enum(op)
	success.Properties["status"] = enum("success")
	success.Properties["complete"] = &jsonschema.Schema{Type: "boolean", Enum: []any{true}}
	data := success.Properties["data"]
	switch op {
	case "discover":
		// With no configured roots the service deliberately returns paths: null.
		data.Properties["paths"].Type = ""
		data.Properties["paths"].Types = []string{"array", "null"}
		data.Properties["operations"].Items = operationEnum(false)
		data.Properties["schemaVersions"].Items = enum("1")
		data.Properties["profile"] = enum("local", "trusted")
		data.Properties["defaultPageSize"].Enum = []any{20}
		data.Properties["maxPageSize"].Enum = []any{100}
		data.Properties["maxResponseBytes"].Enum = []any{maxArgumentBytes}
	case "query":
		data.Properties["items"].MaxItems = new(100)
		data.Properties["items"].Items.Properties["action"].Enum = []any{"", "added", "modified", "deleted"}
		data.Properties["items"].Items.Properties["logicalId"].Description = "Stable preview identity for an unnamed composed output; never a Kubernetes name."
	case "inspect":
		data.Properties["action"].Enum = []any{"", "added", "modified", "deleted"}
		data.Properties["logicalId"].Description = "Stable preview identity for an unnamed composed output; never a Kubernetes name."
	case "check":
		data.Properties["verdict"] = enum("passed", "failed")
	case "release":
		data.Properties["released"].Enum = []any{true}
	}
	failure := failureSchema(enum(op))
	output := &jsonschema.Schema{Type: "object", OneOf: []*jsonschema.Schema{success, failure}}
	return &specification{operation: op, description: description, readOnly: readOnly,
		input: input, request: request, output: output,
		inputResolved: resolve(input), requestResolved: resolve(request), outputResolved: resolve(output)}
}

func infer[T any]() *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic(err)
	} // All types are static DTOs, checked in schema tests.
	constrainNumbers(s)
	return s
}

func constrainNumbers(s *jsonschema.Schema) {
	if s == nil {
		return
	}
	if s.Type == "integer" || slices.Contains(s.Types, "integer") {
		s.Minimum = new(float64(0))
	}
	for _, child := range s.Properties {
		constrainNumbers(child)
	}
	constrainNumbers(s.Items)
	constrainNumbers(s.AdditionalProperties)
}

func enum(values ...string) *jsonschema.Schema {
	s := &jsonschema.Schema{Type: "string", Enum: make([]any, len(values))}
	for i, value := range values {
		s.Enum[i] = value
	}
	return s
}

func operationEnum(unknown bool) *jsonschema.Schema {
	values := []string{"discover", "render", "preview", "query", "inspect", "compare", "check", "release"}
	if unknown {
		values = append(values, "")
	}
	return enum(values...)
}

func failureSchema(operation *jsonschema.Schema) *jsonschema.Schema {
	s := infer[agent.Response]()
	s.Properties["schemaVersion"] = enum("1")
	s.Properties["operation"] = operation
	s.Properties["status"] = enum("failure")
	s.Properties["complete"] = &jsonschema.Schema{Type: "boolean", Enum: []any{false}}
	s.Properties["error"] = infer[agent.Error]()
	s.Required = append(s.Required, "error")
	delete(s.Properties, "data")
	return s
}

func resolve(s *jsonschema.Schema) *jsonschema.Resolved {
	r, err := s.Resolve(nil)
	if err != nil {
		panic(err)
	} // Invalid static schemas are programming errors.
	return r
}

func validateJSON(data []byte, schema *jsonschema.Resolved) error {
	var value any
	if json.Unmarshal(data, &value) != nil || schema.Validate(value) != nil {
		// Never return schema errors: they can include both values and field names.
		return errors.New("invalid request or response")
	}
	return nil
}

// Failure creates a public failure without echoing unknown operation names.
// Callers must supply fixed, non-sensitive codes and messages.
func Failure(op, code, message string) agent.Response {
	known := false
	for _, spec := range specifications() {
		if spec.operation == op {
			known = true
			break
		}
	}
	if !known {
		op = ""
	}
	return agent.Response{SchemaVersion: "1", Operation: op, Status: "failure", Diagnostics: []agent.Diagnostic{}, Error: &agent.Error{Code: code, Message: message}}
}

// DecodeRequest validates a bounded CLI request with the same narrow schemas as
// MCP. An explicit operation may fill an absent operation, but cannot override it.
// Returned errors are sanitized; the request's operation is usable on failure.
func DecodeRequest(data []byte, operation string) (agent.Request, error) {
	req := agent.Request{Operation: operation}
	invalid := errors.New("invalid request, operation, or parameters")
	if len(data) > maxArgumentBytes {
		return req, errors.New("request exceeds 64 KiB")
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		return req, invalid
	}
	if raw, ok := object["operation"]; ok {
		var bodyOp string
		if json.Unmarshal(raw, &bodyOp) != nil || (operation != "" && operation != bodyOp) {
			return req, invalid
		}
		req.Operation = bodyOp
	} else if operation != "" {
		object["operation"], _ = json.Marshal(operation)
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return req, invalid
	}
	for _, spec := range specifications() {
		if spec.operation == req.Operation {
			if validateJSON(encoded, spec.requestResolved) != nil || json.Unmarshal(encoded, &req) != nil {
				return req, invalid
			}
			return req, nil
		}
	}
	return req, invalid
}

// Schema returns standalone request, response, batchRequest, and batchResponse
// schemas, plus input/output unions for a complete CLI invocation. Data schemas
// are inferred from each operation's concrete DTO, including nested results.
func Schema() map[string]any {
	request := &jsonschema.Schema{Type: "object", Description: "Canonical request after applying an optional CLI operation argument; input is limited to 64 KiB. Batch handle fields may reference earlier result data, for example @0.id or @1.items.0.resourceId."}
	response := &jsonschema.Schema{Type: "object"}
	for _, spec := range specifications() {
		request.OneOf = append(request.OneOf, spec.request.CloneSchemas())
		response.OneOf = append(response.OneOf, spec.output.OneOf[0].CloneSchemas())
	}
	response.OneOf = append(response.OneOf, failureSchema(operationEnum(true)))
	batchRequest := &jsonschema.Schema{Type: "object", Required: []string{"requests"}, AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
		Properties: map[string]*jsonschema.Schema{"requests": {Type: "array", MinItems: new(1), MaxItems: new(32), Items: request.CloneSchemas()}}}
	batchSuccess := &jsonschema.Schema{Type: "object", Required: []string{"schemaVersion", "status", "complete", "results"}, AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
		Properties: map[string]*jsonschema.Schema{"schemaVersion": enum("1"), "status": enum("success"), "complete": {Type: "boolean", Enum: []any{true}},
			"results": {Type: "array", MinItems: new(1), MaxItems: new(32), Items: response.CloneSchemas()}}}
	batchFailure := batchSuccess.CloneSchemas()
	batchSuccess.Properties["results"].Items.OneOf = batchSuccess.Properties["results"].Items.OneOf[:len(specifications())]
	batchFailure.Properties["status"] = enum("failure")
	batchFailure.Properties["complete"].Enum = []any{false}
	batchFailure.Properties["results"].MinItems = new(0)
	batchFailure.Properties["failedIndex"] = &jsonschema.Schema{Type: "integer", Minimum: new(float64(0)), Maximum: new(float64(31))}
	batchFailure.Properties["error"] = infer[agent.Error]()
	batchFailure.Required = append(slices.Clone(batchFailure.Required), "failedIndex", "error")
	batchResponse := &jsonschema.Schema{Type: "object", OneOf: []*jsonschema.Schema{batchSuccess, batchFailure}}
	return map[string]any{"schemaVersion": "1", "request": request, "response": response, "batchRequest": batchRequest, "batchResponse": batchResponse,
		"input":  &jsonschema.Schema{OneOf: []*jsonschema.Schema{request.CloneSchemas(), batchRequest.CloneSchemas()}},
		"output": &jsonschema.Schema{OneOf: []*jsonschema.Schema{response.CloneSchemas(), batchResponse.CloneSchemas()}}}
}
