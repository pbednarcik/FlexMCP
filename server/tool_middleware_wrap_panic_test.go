package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// A tool middleware that panics while wrapping, on a synchronous call or on
// a hybrid tool run as a task, does not keep the middleware lock: the chain
// is built outside it, so Use goes on, as it does for a task tool.
func TestToolMiddlewarePanickingWhileWrappingKeepsNoLock(t *testing.T) {
	useReturns := func(t *testing.T, s *MCPServer) {
		t.Helper()
		registered := make(chan struct{})
		go func() {
			s.Use(func(next ToolHandlerFunc) ToolHandlerFunc { return next })
			close(registered)
		}()
		select {
		case <-registered:
		case <-time.After(2 * time.Second):
			t.Fatal("a panic while wrapping kept the middleware lock")
		}
	}
	wrapBug := func(ToolHandlerFunc) ToolHandlerFunc { panic("wrap bug") }
	handler := func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ran"), nil
	}

	t.Run("a synchronous call", func(t *testing.T) {
		s := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
		s.AddTool(mcp.NewTool("t"), handler)
		s.Use(wrapBug)
		func() {
			defer func() { require.Equal(t, "wrap bug", recover()) }()
			_, _ = s.handleToolCall(t.Context(), 1, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "t"}})
		}()
		useReturns(t, s)
	})

	t.Run("a hybrid tool run as a task", func(t *testing.T) {
		s := NewMCPServer("test", "1.0.0", WithTaskCapabilities(true, true, true))
		s.AddTool(mcp.Tool{Name: "h", Execution: &mcp.ToolExecution{TaskSupport: mcp.TaskSupportOptional}}, handler)
		s.Use(wrapBug)
		created, reqErr := s.handleToolCall(t.Context(), 1,
			mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "h", Task: &mcp.TaskParams{}}})
		require.Nil(t, reqErr)
		id := created.(*mcp.CreateTaskResult).Task.TaskId
		_, done, err := s.getTask(t.Context(), id)
		require.NoError(t, err)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("the task never ended")
		}
		task, _, err := s.getTask(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, mcp.TaskStatusFailed, task.Status)
		useReturns(t, s)
	})
}
