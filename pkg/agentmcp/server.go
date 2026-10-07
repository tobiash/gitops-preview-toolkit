// Package agentmcp exposes the bounded agent service through MCP tools.
package agentmcp

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tobiash/gitops-preview-toolkit/pkg/agent"
)

type discoverInput struct {
	Paths     []string `json:"paths,omitempty"`
	Recursive *bool    `json:"recursive,omitempty"`
}
type renderInput struct {
	Source    string   `json:"source,omitempty"`
	Paths     []string `json:"paths,omitempty"`
	Recursive *bool    `json:"recursive,omitempty"`
}
type previewInput struct {
	Base      string   `json:"base,omitempty"`
	Target    string   `json:"target,omitempty"`
	Paths     []string `json:"paths,omitempty"`
	Recursive *bool    `json:"recursive,omitempty"`
}
type queryInput struct {
	ID     string      `json:"id"`
	Query  agent.Query `json:"query,omitempty"`
	Offset int         `json:"offset,omitempty"`
	Limit  int         `json:"limit,omitempty"`
}
type inspectInput struct {
	ID         string   `json:"id"`
	ResourceID string   `json:"resourceId"`
	Fields     []string `json:"fields,omitempty"`
}
type compareInput struct {
	BeforeID string `json:"beforeId"`
	AfterID  string `json:"afterId"`
}
type checkInput struct {
	ID              string      `json:"id"`
	Query           agent.Query `json:"query,omitempty"`
	MaxCPURequests  string      `json:"maxCPURequests,omitempty"`
	MaxMemRequests  string      `json:"maxMemRequests,omitempty"`
	MaxCPULimits    string      `json:"maxCPULimits,omitempty"`
	MaxMemLimits    string      `json:"maxMemLimits,omitempty"`
	RequireRequests bool        `json:"requireRequests,omitempty"`
	RequireLimits   bool        `json:"requireLimits,omitempty"`
}
type releaseInput struct {
	ID string `json:"id"`
}

// NewServer creates an MCP server. The caller owns the service and must close it
// after all sessions end. Trust is exclusively configured when creating service.
func NewServer(service *agent.Service, version string) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "fmp", Version: version}, nil)
	for _, spec := range specifications() {
		// Generic AddTool validates before invoking the handler and includes
		// argument values in errors. Own validation so those values never escape.
		s.AddTool(&mcp.Tool{Name: "manifest_" + spec.operation, Description: spec.description,
			InputSchema: spec.input, OutputSchema: spec.output,
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: spec.readOnly}},
			func(ctx context.Context, call *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				data := call.Params.Arguments
				if len(data) == 0 {
					data = json.RawMessage(`{}`)
				}
				response := Failure(spec.operation, "InvalidInput", "Invalid tool arguments.")
				if len(data) > maxArgumentBytes {
					response = Failure(spec.operation, "RequestTooLarge", "Tool arguments exceed 64 KiB.")
				} else if validateJSON(data, spec.inputResolved) == nil {
					var req agent.Request
					if json.Unmarshal(data, &req) == nil {
						req.Operation = spec.operation
						response = service.Execute(ctx, req)
					}
				}
				encoded, err := json.Marshal(response)
				if err != nil || validateJSON(encoded, spec.outputResolved) != nil {
					response = Failure(spec.operation, "InvalidOutput", "Operation returned an invalid response.")
					encoded, _ = json.Marshal(response)
				}
				return &mcp.CallToolResult{IsError: response.Status == "failure", StructuredContent: json.RawMessage(encoded),
					Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}}}, nil
			})
	}
	return s
}
