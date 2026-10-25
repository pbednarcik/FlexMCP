package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// A task whose arguments fail validation runs no handler, so it takes no
// concurrency slot: with the limit reached, the caller still gets the
// validation error through the task, not "max concurrent tasks".
func TestAnInvalidTaskCallTakesNoSlot(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	s := NewMCPServer("test", "1.0.0", WithTaskCapabilities(true, true, true), WithMaxConcurrentTasks(1), WithInputSchemaValidation())
	s.AddTool(mcp.Tool{
		Name:           "picky",
		Execution:      &mcp.ToolExecution{TaskSupport: mcp.TaskSupportOptional},
		RawInputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"number"}},"required":["n"]}`),
	}, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		<-release
		return mcp.NewToolResultText("ran"), nil
	})
	call := func(n any) mcp.CallToolRequest {
		return mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "picky", Arguments: map[string]any{"n": n}, Task: &mcp.TaskParams{}}}
	}

	_, reqErr := s.handleTaskAugmentedToolCall(t.Context(), 1, call(1))
	require.Nil(t, reqErr)

	created, reqErr := s.handleTaskAugmentedToolCall(t.Context(), 2, call("no"))
	require.Nil(t, reqErr, "an invalid call was refused for the concurrency limit")
	result, reqErr := s.handleTaskResult(t.Context(), 3, mcp.TaskResultRequest{Params: mcp.TaskResultParams{TaskId: created.Task.TaskId}})
	require.Nil(t, reqErr)
	require.True(t, result.IsError, "the task did not fail validation")
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "input schema validation failed")
}
