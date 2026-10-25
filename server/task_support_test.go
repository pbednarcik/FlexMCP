package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// A task-augmented call to a tool that does not support tasks, or on a
// server without the tasks capability, is refused as method not found
// (2025-11-25 Tasks.md row 28), not run as a synchronous call.
func TestTaskAugmentedCallIsRefusedWhereUnsupported(t *testing.T) {
	echo := func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ran"), nil
	}
	taskCall := func(name string) mcp.CallToolRequest {
		return mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Task: &mcp.TaskParams{}}}
	}

	t.Run("a tool without task support", func(t *testing.T) {
		s := NewMCPServer("test", "1.0.0", WithTaskCapabilities(true, true, true))
		s.AddTool(mcp.NewTool("plain"), echo)
		_, reqErr := s.handleToolCall(t.Context(), 1, taskCall("plain"))
		require.NotNil(t, reqErr, "the call ran synchronously")
		require.Equal(t, mcp.METHOD_NOT_FOUND, reqErr.code)
	})

	t.Run("a tool forbidding tasks", func(t *testing.T) {
		s := NewMCPServer("test", "1.0.0", WithTaskCapabilities(true, true, true))
		s.AddTool(mcp.Tool{Name: "never", Execution: &mcp.ToolExecution{TaskSupport: mcp.TaskSupportForbidden}}, echo)
		_, reqErr := s.handleToolCall(t.Context(), 1, taskCall("never"))
		require.NotNil(t, reqErr, "the call ran synchronously")
		require.Equal(t, mcp.METHOD_NOT_FOUND, reqErr.code)
	})

	t.Run("a server whose tasks capability excludes tool calls", func(t *testing.T) {
		s := NewMCPServer("test", "1.0.0", WithTaskCapabilities(true, true, false))
		s.AddTool(mcp.Tool{Name: "hybrid", Execution: &mcp.ToolExecution{TaskSupport: mcp.TaskSupportOptional}}, echo)
		_, reqErr := s.handleToolCall(t.Context(), 1, taskCall("hybrid"))
		require.NotNil(t, reqErr, "a task was created on a server that does not task-call tools")
		require.Equal(t, mcp.METHOD_NOT_FOUND, reqErr.code)
	})

	t.Run("a server without the tasks capability", func(t *testing.T) {
		s := NewMCPServer("test", "1.0.0")
		s.AddTool(mcp.Tool{Name: "hybrid", Execution: &mcp.ToolExecution{TaskSupport: mcp.TaskSupportOptional}}, echo)
		_, reqErr := s.handleToolCall(t.Context(), 1, taskCall("hybrid"))
		require.NotNil(t, reqErr, "a task was created on a server that serves no tasks")
		require.Equal(t, mcp.METHOD_NOT_FOUND, reqErr.code)
	})
}
