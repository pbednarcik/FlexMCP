package server

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
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
		synctest.Test(t, func(t *testing.T) {
			s := newServer()
			ttl, poll := int64(60000), int64(1000)
			_, err := s.createTask(t.Context(), "task-2", "tool", &ttl, &poll)
			require.NoError(t, err)
			answered := make(chan mcp.JSONRPCMessage, 1)
			go func() { answered <- send(t, s, 2, string(mcp.MethodTasksResult), "task-2") }()
			synctest.Wait()
			select {
			case <-answered:
				t.Fatal("tasks/result answered before the task ended")
			default:
			}
			cancel(t, s, "task-2")

			select {
			case response := <-answered:
				requireInterrupted(t, response)
			case <-time.After(5 * time.Second):
				t.Fatal("tasks/result did not return after the cancel")
			}
		})
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

	// A handler that sees its context end before any tasks/cancel marks the
	// task cancelled itself, in either runner.
	t.Run("the handler saw the cancel first", func(t *testing.T) {
		ended, end := context.WithCancel(t.Context())
		end()
		runners := map[string]func(s *MCPServer, entry *taskEntry){
			"a task tool": func(s *MCPServer, entry *taskEntry) {
				s.executeTaskTool(ended, entry, ServerTaskTool{Handler: func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
					<-ctx.Done()
					return nil, ctx.Err()
				}}, mcp.CallToolRequest{})
			},
			"a regular tool run as a task": func(s *MCPServer, entry *taskEntry) {
				s.executeRegularToolAsTask(ended, entry, ServerTool{Handler: func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					<-ctx.Done()
					return nil, ctx.Err()
				}}, mcp.CallToolRequest{})
			},
		}
		for name, run := range runners {
			t.Run(name, func(t *testing.T) {
				s := newServer()
				entry, err := s.createTask(t.Context(), "task-4", "tool", nil, nil)
				require.NoError(t, err)
				run(s, entry)

				requireInterrupted(t, send(t, s, 2, string(mcp.MethodTasksResult), "task-4"))
			})
		}
	})
}
