package server

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// A task tool's handler runs inside the task tool middlewares, as a tool's
// runs inside the tool middlewares: a middleware that refuses the call fails
// the task, and one that passes it on sees every call.
func TestTaskToolMiddlewareWrapsTheHandler(t *testing.T) {
	var calls atomic.Int32
	refused := errors.New("refused by the middleware")
	s := NewMCPServer("test", "1.0.0", WithTaskCapabilities(true, true, true))
	s.UseForTaskTools(func(next TaskToolHandlerFunc) TaskToolHandlerFunc {
		return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
			calls.Add(1)
			if request.Params.Name == "refused" {
				return nil, refused
			}
			return next(ctx, request)
		}
	})
	handler := func(context.Context, mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
		return &mcp.CreateTaskResult{}, nil
	}
	taskTool := func(name string) ServerTaskTool {
		return ServerTaskTool{
			Tool:    mcp.Tool{Name: name, Execution: &mcp.ToolExecution{TaskSupport: mcp.TaskSupportRequired}},
			Handler: handler,
		}
	}
	s.AddTaskTools(taskTool("refused"), taskTool("allowed"))

	for name, want := range map[string]mcp.TaskStatus{"refused": mcp.TaskStatusFailed, "allowed": mcp.TaskStatusCompleted} {
		created, reqErr := s.handleToolCall(t.Context(), 1,
			mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Task: &mcp.TaskParams{}}})
		require.Nil(t, reqErr)
		id := created.(*mcp.CreateTaskResult).Task.TaskId
		_, done, err := s.getTask(t.Context(), id)
		require.NoError(t, err)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("task %s never ended", name)
		}
		task, _, err := s.getTask(t.Context(), id)
		require.NoError(t, err)
		require.Equal(t, want, task.Status, name)
		if want == mcp.TaskStatusFailed {
			s.tasksMu.RLock()
			require.ErrorIs(t, s.tasks[id].resultErr, refused)
			s.tasksMu.RUnlock()
		}
	}
	require.Equal(t, int32(2), calls.Load(), "the middleware did not see every call")
}

// A middleware that panics while wrapping fails the task, and the server's
// middleware registration goes on: the chain is built outside the lock.
func TestTaskToolMiddlewarePanickingWhileWrappingKeepsNoLock(t *testing.T) {
	s := NewMCPServer("test", "1.0.0", WithTaskCapabilities(true, true, true))
	s.UseForTaskTools(func(TaskToolHandlerFunc) TaskToolHandlerFunc { panic("wrap bug") })
	s.AddTaskTools(ServerTaskTool{
		Tool: mcp.Tool{Name: "t", Execution: &mcp.ToolExecution{TaskSupport: mcp.TaskSupportRequired}},
		Handler: func(context.Context, mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
			return &mcp.CreateTaskResult{}, nil
		},
	})
	created, reqErr := s.handleToolCall(t.Context(), 1,
		mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "t", Task: &mcp.TaskParams{}}})
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

	registered := make(chan struct{})
	go func() {
		s.UseForTaskTools(func(next TaskToolHandlerFunc) TaskToolHandlerFunc { return next })
		close(registered)
	}()
	select {
	case <-registered:
	case <-time.After(2 * time.Second):
		t.Fatal("a panic while wrapping kept the middleware lock")
	}
}

// The task tool middlewares run outermost first, in the order they were
// added, as the tool middlewares do.
func TestTaskToolMiddlewaresRunOutermostFirst(t *testing.T) {
	var order []string
	var mu sync.Mutex
	named := func(name string) TaskToolHandlerMiddleware {
		return func(next TaskToolHandlerFunc) TaskToolHandlerFunc {
			return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
				mu.Lock()
				order = append(order, name)
				mu.Unlock()
				return next(ctx, request)
			}
		}
	}
	s := NewMCPServer("test", "1.0.0", WithTaskCapabilities(true, true, true), WithTaskToolHandlerMiddleware(named("first")))
	s.UseForTaskTools(named("second"), named("third"))
	s.AddTaskTools(ServerTaskTool{
		Tool: mcp.Tool{Name: "t", Execution: &mcp.ToolExecution{TaskSupport: mcp.TaskSupportRequired}},
		Handler: func(context.Context, mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
			return &mcp.CreateTaskResult{}, nil
		},
	})
	created, reqErr := s.handleToolCall(t.Context(), 1,
		mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "t", Task: &mcp.TaskParams{}}})
	require.Nil(t, reqErr)
	_, done, err := s.getTask(t.Context(), created.(*mcp.CreateTaskResult).Task.TaskId)
	require.NoError(t, err)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the task never ended")
	}
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"first", "second", "third"}, order)
}

// A context error returned while the task's own context is live comes from
// the middleware or the handler, a deadline of their own, and fails the
// task with it; only a cancelled task context makes a task cancelled. The
// same for a hybrid tool run as a task.
func TestATaskWhoseHandlerReturnsItsOwnContextErrorFails(t *testing.T) {
	run := func(t *testing.T, s *MCPServer, name string) {
		created, reqErr := s.handleToolCall(t.Context(), 1,
			mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Task: &mcp.TaskParams{}}})
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
		require.Equal(t, mcp.TaskStatusFailed, task.Status, "the task ended %s", task.Status)
		s.tasksMu.RLock()
		defer s.tasksMu.RUnlock()
		require.ErrorIs(t, s.tasks[id].resultErr, context.DeadlineExceeded)
	}

	t.Run("a task tool middleware", func(t *testing.T) {
		s := NewMCPServer("test", "1.0.0", WithTaskCapabilities(true, true, true))
		s.UseForTaskTools(func(TaskToolHandlerFunc) TaskToolHandlerFunc {
			return func(context.Context, mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
				return nil, context.DeadlineExceeded
			}
		})
		s.AddTaskTools(ServerTaskTool{
			Tool: mcp.Tool{Name: "t", Execution: &mcp.ToolExecution{TaskSupport: mcp.TaskSupportRequired}},
			Handler: func(context.Context, mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
				return &mcp.CreateTaskResult{}, nil
			},
		})
		run(t, s, "t")
	})

	t.Run("a hybrid tool's handler", func(t *testing.T) {
		s := NewMCPServer("test", "1.0.0", WithTaskCapabilities(true, true, true))
		s.AddTool(mcp.Tool{Name: "h", Execution: &mcp.ToolExecution{TaskSupport: mcp.TaskSupportOptional}},
			func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return nil, context.DeadlineExceeded
			})
		run(t, s, "h")
	})
}
