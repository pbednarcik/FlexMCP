package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A legacy task-augmented call runs the registration the tool filters
// authorized, with the schemas kept with it, though the name is registered
// again while the filters run, as registration inside a filter may (filters
// run outside toolsMu): the replacement is one the filters deny. The same holds
// for a tool and for a task tool.
func TestTaskCallRunsTheAuthorizedRegistration(t *testing.T) {
	type registration struct {
		description string
		input       mcp.ToolOption
		output      string
	}
	authorized := registration{"authorized", mcp.WithString("a", mcp.Required()), `{"type":"object","properties":{"n":{"type":"number"}},"required":["n"]}`}
	replacement := registration{"replacement", mcp.WithNumber("a", mcp.Required()), `{"type":"object","properties":{"s":{"type":"string"}},"required":["s"]}`}
	for _, kind := range []string{"tool", "task tool"} {
		for _, schemas := range []bool{false, true} {
			name := kind + "/handler"
			if schemas {
				name = kind + "/schemas"
			}
			t.Run(name, func(t *testing.T) {
				ran := make(chan string, 2)
				// With schemas, both registrations share one handler, so only
				// the schemas the call is validated against tell them apart.
				register := func(srv *MCPServer, r registration) {
					label := r.description
					if schemas {
						label = "shared"
					}
					options := []mcp.ToolOption{mcp.WithDescription(r.description), mcp.WithTaskSupport(mcp.TaskSupportOptional)}
					if schemas {
						options = append(options, r.input, mcp.WithRawOutputSchema(json.RawMessage(r.output)))
					}
					tool := mcp.NewTool("slow", options...)
					content := []mcp.Content{mcp.NewTextContent(label)}
					structured := map[string]any{"n": 7}
					if kind == "tool" {
						srv.AddTool(tool, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
							ran <- label
							return &mcp.CallToolResult{Content: content, StructuredContent: structured}, nil
						})
						return
					}
					srv.AddTaskTool(tool, func(context.Context, mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
						ran <- label
						return &mcp.CreateTaskResult{Content: content, StructuredContent: structured}, nil
					})
				}
				var srv *MCPServer
				var swapped atomic.Bool
				options := []ServerOption{WithToolCapabilities(false), WithTaskCapabilities(true, true, true), WithToolFilter(func(_ context.Context, tools []mcp.Tool) []mcp.Tool {
					if len(tools) != 1 || tools[0].Description != authorized.description {
						return nil
					}
					if swapped.CompareAndSwap(false, true) {
						register(srv, replacement)
					}
					return tools
				})}
				if schemas {
					options = append(options, WithInputSchemaValidation(), WithOutputSchemaValidation())
				}
				srv = NewMCPServer("test", "1.0.0", options...)
				register(srv, authorized)

				body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
					"name": "slow", "arguments": map[string]any{"a": "x"}, "task": map[string]any{"ttl": 60000},
				}})
				require.NoError(t, err)
				answer, err := json.Marshal(srv.HandleMessage(t.Context(), body))
				require.NoError(t, err)
				var created struct {
					Result struct {
						Task struct {
							TaskID string `json:"taskId"`
						} `json:"task"`
					} `json:"result"`
				}
				require.NoError(t, json.Unmarshal(answer, &created), string(answer))
				require.NotEmpty(t, created.Result.Task.TaskID, string(answer))
				require.True(t, swapped.Load(), "the filters registered the replacement")

				result, reqErr := srv.handleTaskResult(t.Context(), 2, mcp.TaskResultRequest{Params: mcp.TaskResultParams{TaskId: created.Result.Task.TaskID}})
				require.Nil(t, reqErr)
				require.NotEmpty(t, result.Content)
				assert.False(t, result.IsError, "validated against the authorized schemas: %v", result.Content[0])
				select {
				case label := <-ran:
					if schemas {
						assert.Equal(t, "shared", label)
					} else {
						assert.Equal(t, authorized.description, label, "the handler of the registration the filters authorized")
					}
				default:
					t.Fatalf("no handler ran: %v", result.Content[0])
				}

				// The replacement is one the filters deny.
				denied, err := json.Marshal(srv.HandleMessage(t.Context(), body))
				require.NoError(t, err)
				assert.Contains(t, string(denied), `"code":-32602`)
			})
		}
	}
}

// handleTaskAugmentedToolCall looks the tool called up as handleToolCall
// does and runs the call as a task, without the tool filters and the checks
// handleToolCall makes first: the tests that drive the task path directly
// call it.
func (s *MCPServer) handleTaskAugmentedToolCall(ctx context.Context, id any, request mcp.CallToolRequest) (*mcp.CreateTaskResult, *requestError) {
	called, ok := s.toolToCall(request.Params.Name)
	if !ok {
		return nil, &requestError{id: id, code: mcp.INVALID_PARAMS, err: fmt.Errorf("tool '%s' not found", request.Params.Name)}
	}
	return s.handleToolCallAsTask(ctx, id, request, called)
}
