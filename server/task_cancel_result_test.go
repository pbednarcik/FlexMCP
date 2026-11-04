package server

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// tasks/result of a cancelled task answers what the cancelled request would
// have, an error (-32800), never a success with nothing in it: 2025-11-25
// tasks/result returns the underlying request's result or its JSON-RPC error.
func TestTaskResultOfACancelledTaskIsAnError(t *testing.T) {
	newServer := func() *MCPServer {
		return NewMCPServer("test", "1.0.0", WithTaskCapabilities(true, true, true))
	}
	send := func(t *testing.T, s *MCPServer, id int, method, taskID string) mcp.JSONRPCMessage {
		t.Helper()
		return s.HandleMessage(t.Context(), fmt.Appendf(nil,
			`{"jsonrpc":"2.0","id":%d,"method":%q,"params":{"taskId":%q}}`, id, method, taskID))
	}
	cancel := func(t *testing.T, s *MCPServer, taskID string) {
		t.Helper()
		_, ok := send(t, s, 1, string(mcp.MethodTasksCancel), taskID).(mcp.JSONRPCResponse)
		require.True(t, ok, "tasks/cancel failed")
	}
	requireInterrupted := func(t *testing.T, response mcp.JSONRPCMessage) {
		t.Helper()
		rpcErr, ok := response.(mcp.JSONRPCError)
		require.True(t, ok, "tasks/result of a cancelled task answered %#v", response)
		assert.Equal(t, mcp.REQUEST_INTERRUPTED, rpcErr.Error.Code)
	}

	t.Run("cancelled before tasks/result", func(t *testing.T) {
		s := newServer()
		ttl, poll := int64(60000), int64(1000)
		_, err := s.createTask(t.Context(), "task-1", "tool", &ttl, &poll)
		require.NoError(t, err)
		cancel(t, s, "task-1")

		requireInterrupted(t, send(t, s, 2, string(mcp.MethodTasksResult), "task-1"))
	})

	t.Run("cancelled while tasks/result waits", func(t *testing.T) {
		s := newServer()
		ttl, poll := int64(60000), int64(1000)
		_, err := s.createTask(t.Context(), "task-2", "tool", &ttl, &poll)
		require.NoError(t, err)
		answered := make(chan mcp.JSONRPCMessage, 1)
		go func() { answered <- send(t, s, 2, string(mcp.MethodTasksResult), "task-2") }()
		// Let tasks/result block on the running task; landing after the cancel
		// instead would still have to answer the same.
		time.Sleep(50 * time.Millisecond)
		cancel(t, s, "task-2")

		select {
		case response := <-answered:
			requireInterrupted(t, response)
		case <-time.After(5 * time.Second):
			t.Fatal("tasks/result did not return after the cancel")
		}
	})

	t.Run("a task tool cancelled while it runs", func(t *testing.T) {
		s := newServer()
		s.AddTool(mcp.NewTool("wait", mcp.WithTaskSupport(mcp.TaskSupportRequired)),
			func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			})
		created, callErr := s.handleToolCall(t.Context(), 1, mcp.CallToolRequest{
			Params: mcp.CallToolParams{Name: "wait", Task: &mcp.TaskParams{}},
		})
		require.Nil(t, callErr)
		taskID := created.(*mcp.CreateTaskResult).Task.TaskId
		cancel(t, s, taskID)

		requireInterrupted(t, send(t, s, 2, string(mcp.MethodTasksResult), taskID))
	})
}
