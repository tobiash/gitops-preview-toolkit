package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/tobiash/gitops-preview-toolkit/pkg/agent"
	"github.com/tobiash/gitops-preview-toolkit/pkg/agentmcp"
)

const batchLimit = 1 << 20

var errAgentHandled = errors.New("agent operation failed")

type agentBatchResponse struct {
	SchemaVersion string           `json:"schemaVersion"`
	Status        string           `json:"status"`
	Complete      bool             `json:"complete"`
	Results       []agent.Response `json:"results"`
	FailedIndex   *int             `json:"failedIndex,omitempty"`
	Error         *agent.Error     `json:"error,omitempty"`
}

func agentCmd() *cobra.Command {
	var root string
	var opts agent.Options
	cmd := &cobra.Command{Use: "agent [operation]", Short: "Execute a JSON request or sequential batch in one ephemeral session", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 && args[0] == "schema" {
				return writeAgentSchema(cmd.OutOrStdout())
			}
			op := ""
			if len(args) == 1 {
				op = args[0]
			}
			if root == "" {
				return writeAgent(cmd.OutOrStdout(), agentFailure(op, "--root is required"), true)
			}
			s, err := agent.New(root, opts)
			if err != nil {
				return writeAgent(cmd.OutOrStdout(), agentFailure(op, err.Error()), true)
			}
			defer func() {
				if err := s.Close(); err != nil {
					_, _ = fmt.Fprintln(cmd.ErrOrStderr(), err)
				}
			}()
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			input := cmd.InOrStdin()
			// Interrupt a pipe read as well as service work on shutdown.
			interrupt := context.AfterFunc(ctx, func() {
				if closer, ok := input.(io.ReadCloser); ok {
					_ = closer.Close()
				}
			})
			defer interrupt()
			return runAgent(ctx, s, input, cmd.OutOrStdout(), op)
		}}
	agentFlags(cmd, &root, &opts)
	return cmd
}

func agentFailure(op, message string) agent.Response {
	return agentmcp.Failure(op, "InvalidInput", message)
}

func writeAgent(out io.Writer, value any, failed bool) error {
	if err := json.NewEncoder(out).Encode(value); err != nil {
		return err
	}
	if failed {
		return errAgentHandled
	}
	return nil
}

func decodeAgent(data []byte, value any) error {
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return errors.New("expected a JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return errors.New("invalid JSON request or unknown field")
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("expected exactly one JSON document")
	}
	return nil
}

func runAgent(ctx context.Context, s *agent.Service, in io.Reader, out io.Writer, op string) error {
	// Explicit discovery needs no body when invoked interactively. Pipes and
	// regular files still supply JSON overrides, even with an explicit operation.
	if file, ok := in.(*os.File); ok && op == "discover" {
		if info, err := file.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
			in = strings.NewReader(`{}`)
		}
	}
	data, err := io.ReadAll(io.LimitReader(in, batchLimit+1))
	if err != nil || len(data) > batchLimit {
		return writeAgent(out, agentFailure(op, "input exceeds 1 MiB or cannot be read"), true)
	}
	if len(bytes.TrimSpace(data)) == 0 && op == "discover" {
		data = []byte(`{}`)
	}
	var envelope map[string]json.RawMessage
	if err := decodeAgent(data, &envelope); err != nil {
		return writeAgent(out, agentFailure(op, err.Error()), true)
	}
	if _, batch := envelope["requests"]; batch {
		var input struct {
			Requests []json.RawMessage `json:"requests"`
		}
		if err := decodeAgent(data, &input); err != nil || op != "" || len(input.Requests) == 0 || len(input.Requests) > 32 {
			return writeAgent(out, agentFailure(op, "batch requires 1..32 requests and no operation argument"), true)
		}
		result := agentBatchResponse{SchemaVersion: "1", Status: "success", Complete: true, Results: []agent.Response{}}
		for i, raw := range input.Requests {
			var response agent.Response
			req, err := agentmcp.DecodeRequest(raw, "")
			if err != nil {
				response = agentFailure(req.Operation, err.Error())
			} else if err := resolveAgentRefs(&req, result.Results); err != nil {
				response = agentFailure(req.Operation, err.Error())
			} else {
				response = s.Execute(ctx, req)
			}
			// Reserve room for failure metadata before committing another result.
			candidate, _ := json.Marshal(append(result.Results, response))
			if len(candidate) > batchLimit-1024 {
				result.Status, result.Complete, result.FailedIndex = "failure", false, &i
				result.Error = &agent.Error{Code: "ResponseTooLarge", Message: "batch response exceeds 1 MiB"}
				break
			}
			result.Results = append(result.Results, response)
			if response.Status == "failure" {
				result.Status, result.Complete, result.FailedIndex, result.Error = "failure", false, &i, response.Error
				break
			}
		}
		return writeAgent(out, result, result.Status == "failure")
	}
	if len(data) > 64<<10 {
		return writeAgent(out, agentFailure(op, "request exceeds 64 KiB"), true)
	}
	req, err := agentmcp.DecodeRequest(data, op)
	if err != nil {
		return writeAgent(out, agentFailure(req.Operation, err.Error()), true)
	}
	response := s.Execute(ctx, req)
	return writeAgent(out, response, response.Status == "failure")
}

func resolveAgentRefs(req *agent.Request, results []agent.Response) error {
	for _, field := range []*string{&req.ID, &req.BeforeID, &req.AfterID, &req.ResourceID} {
		if !strings.HasPrefix(*field, "@") {
			continue
		}
		parts := strings.Split((*field)[1:], ".")
		index, err := strconv.Atoi(parts[0])
		if err != nil || index < 0 || index >= len(results) || len(parts) < 2 {
			return errors.New("reference must name an earlier result data field")
		}
		data, _ := json.Marshal(results[index].Data)
		var value any
		if err := json.Unmarshal(data, &value); err != nil {
			return errors.New("invalid reference data")
		}
		for _, part := range parts[1:] {
			switch node := value.(type) {
			case map[string]any:
				value = node[part]
			case []any:
				i, err := strconv.Atoi(part)
				if err != nil || i < 0 || i >= len(node) {
					return errors.New("invalid reference array index")
				}
				value = node[i]
			default:
				return errors.New("invalid reference path")
			}
		}
		text, ok := value.(string)
		if !ok {
			return errors.New("reference must resolve to a string")
		}
		*field = text
	}
	return nil
}

func writeAgentSchema(out io.Writer) error {
	return writeAgent(out, agentmcp.Schema(), false)
}
