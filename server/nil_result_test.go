package server

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A prompt handler that returns neither a result nor an error used to crash
// the dispatcher with a nil pointer dereference. Over streamable HTTP the
// client saw the connection drop; with the in-process client the panic reached
// the caller.
func TestGetPromptNilResultIsAnError(t *testing.T) {
	srv := NewMCPServer("test-server", "1.0.0", WithPromptCapabilities(true))
	srv.AddPrompt(mcp.NewPrompt("broken"), func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return nil, nil
	})

	var response mcp.JSONRPCMessage
	require.NotPanics(t, func() {
		response = srv.HandleMessage(t.Context(), []byte(`{
			"jsonrpc": "2.0",
			"id": 1,
			"method": "prompts/get",
			"params": {"name": "broken"}
		}`))
	})

	errorResponse, ok := response.(mcp.JSONRPCError)
	require.True(t, ok, "expected an error response, got %T", response)
	assert.Equal(t, mcp.INTERNAL_ERROR, errorResponse.Error.Code)
	assert.Equal(t, "prompt 'broken' handler returned no result", errorResponse.Error.Message)
}

// A tool handler that returns neither a result nor an error used to be
// answered with a null result, which clients reject as malformed.
func TestCallToolNilResultIsAnError(t *testing.T) {
	srv := NewMCPServer("test-server", "1.0.0", WithToolCapabilities(true))
	srv.AddTool(mcp.NewTool("broken"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, nil
	})

	response := srv.HandleMessage(t.Context(), []byte(`{
		"jsonrpc": "2.0",
		"id": 1,
		"method": "tools/call",
		"params": {"name": "broken"}
	}`))

	errorResponse, ok := response.(mcp.JSONRPCError)
	require.True(t, ok, "expected an error response, got %T", response)
	assert.Equal(t, mcp.INTERNAL_ERROR, errorResponse.Error.Code)
	assert.Equal(t, "tool 'broken' handler returned no result", errorResponse.Error.Message)
}

// Run as a task, the same handler used to complete the task with a nil
// result, and tasks/result then panicked reading it. The task fails instead.
func TestTaskNilResultFailsTheTask(t *testing.T) {
	tests := []struct {
		name     string
		register func(srv *MCPServer)
	}{
		{
			name: "task tool",
			register: func(srv *MCPServer) {
				srv.AddTaskTool(mcp.NewTool("broken", mcp.WithTaskSupport(mcp.TaskSupportRequired)),
					func(context.Context, mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
						return nil, nil
					})
			},
		},
		{
			name: "regular tool run as a task",
			register: func(srv *MCPServer) {
				srv.AddTool(mcp.NewTool("broken", mcp.WithTaskSupport(mcp.TaskSupportOptional)),
					func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
						return nil, nil
					})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewMCPServer("test-server", "1.0.0", WithTaskCapabilities(true, true, true))
			tt.register(srv)

			created, reqErr := srv.handleToolCall(t.Context(), 1, mcp.CallToolRequest{
				Params: mcp.CallToolParams{Name: "broken", Task: &mcp.TaskParams{}},
			})
			require.Nil(t, reqErr)
			taskID := created.(*mcp.CreateTaskResult).Task.TaskId

			var result *mcp.TaskResultResult
			require.NotPanics(t, func() {
				result, reqErr = srv.handleTaskResult(t.Context(), 2, mcp.TaskResultRequest{
					Params: mcp.TaskResultParams{TaskId: taskID},
				})
			})
			assert.Nil(t, result)
			require.NotNil(t, reqErr)
			assert.Equal(t, mcp.INTERNAL_ERROR, reqErr.code)
			assert.EqualError(t, reqErr.err, "tool 'broken' handler returned no result")

			task, _, err := srv.getTask(t.Context(), taskID)
			require.NoError(t, err)
			assert.Equal(t, mcp.TaskStatusFailed, task.Status)
		})
	}
}
