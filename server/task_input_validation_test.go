package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// Input validation applies to a task-augmented call as to a synchronous one:
// arguments that fail it never reach the handler, and the task's result is
// the tool error the synchronous call would have returned (Tools.md row 23).
func TestTaskAugmentedCallValidatesInput(t *testing.T) {
	handlerRan := make(chan struct{}, 1)
	s := NewMCPServer("test", "1.0.0", WithTaskCapabilities(true, true, true), WithInputSchemaValidation())
	s.AddTool(mcp.Tool{
		Name:           "picky",
		Execution:      &mcp.ToolExecution{TaskSupport: mcp.TaskSupportOptional},
		RawInputSchema: json.RawMessage(`{"type":"object","properties":{"n":{"type":"number"}},"required":["n"]}`),
	}, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		handlerRan <- struct{}{}
		return mcp.NewToolResultText("ran"), nil
	})

	created, reqErr := s.handleTaskAugmentedToolCall(t.Context(), 1, mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "picky", Arguments: map[string]any{"n": "no"}, Task: &mcp.TaskParams{},
	}})
	require.Nil(t, reqErr)
	result, reqErr := s.handleTaskResult(t.Context(), 2, mcp.TaskResultRequest{Params: mcp.TaskResultParams{TaskId: created.Task.TaskId}})
	require.Nil(t, reqErr)
	require.True(t, result.IsError, "the task did not fail validation")
	assert.Contains(t, result.Content[0].(mcp.TextContent).Text, "input schema validation failed")
	select {
	case <-handlerRan:
		t.Fatal("the handler ran on arguments that failed validation")
	default:
	}
}
