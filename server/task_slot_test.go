package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// A concurrency slot is held until the task's handler returns: a cancelled
// task whose handler ignores its context still counts, so cancelling cannot
// run more handlers at once than WithMaxConcurrentTasks allows.
func TestMaxConcurrentTasks_ACancelledTaskHoldsItsSlotWhileItsHandlerRuns(t *testing.T) {
	release := make(chan struct{})
	s := NewMCPServer("test", "1.0.0", WithTaskCapabilities(true, true, true), WithMaxConcurrentTasks(1))
	s.AddTool(mcp.Tool{Name: "stubborn", Execution: &mcp.ToolExecution{TaskSupport: mcp.TaskSupportRequired}},
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			<-release
			return mcp.NewToolResultText("late"), nil
		})
	call := mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "stubborn", Task: &mcp.TaskParams{}}}

	created, reqErr := s.handleTaskAugmentedToolCall(t.Context(), 1, call)
	require.Nil(t, reqErr)
	require.NoError(t, s.cancelTask(t.Context(), created.Task.TaskId))

	_, reqErr = s.handleTaskAugmentedToolCall(t.Context(), 2, call)
	require.NotNil(t, reqErr, "a cancelled task's running handler did not hold its slot")

	close(release)
	require.Eventually(t, func() bool {
		_, reqErr := s.handleTaskAugmentedToolCall(t.Context(), 3, call)
		return reqErr == nil
	}, 2*time.Second, 10*time.Millisecond, "the slot was not given back when the handler returned")
}

// A task whose handler never starts, because its created hook panicked,
// gives its slot back at once: nothing else would.
func TestMaxConcurrentTasks_APanickingCreatedHookFreesTheSlot(t *testing.T) {
	var s *MCPServer
	hooks := &TaskHooks{}
	hooks.AddOnTaskCreated(func(ctx context.Context, m TaskMetrics) {
		require.NoError(t, s.cancelTask(ctx, m.TaskID))
		panic("hook bug")
	})
	s = NewMCPServer("test", "1.0.0", WithTaskHooks(hooks), WithTaskCapabilities(true, true, true), WithMaxConcurrentTasks(1))

	func() {
		defer func() { require.Equal(t, "hook bug", recover()) }()
		_, _ = s.createTask(t.Context(), "task-1", "tool", nil, nil)
	}()

	hooks.OnTaskCreated = nil
	_, err := s.createTask(t.Context(), "task-2", "tool", nil, nil)
	require.NoError(t, err, "the slot of a task whose handler never started was kept")
}

// The same when the panic comes from a hook queued behind the created one:
// createTask owns the slot until it hands the entry back.
func TestMaxConcurrentTasks_APanickingQueuedHookFreesTheSlot(t *testing.T) {
	var s *MCPServer
	hooks := &TaskHooks{}
	hooks.AddOnTaskCreated(func(ctx context.Context, m TaskMetrics) {
		require.NoError(t, s.cancelTask(ctx, m.TaskID))
	})
	hooks.AddOnTaskCancelled(func(context.Context, TaskMetrics) { panic("hook bug") })
	s = NewMCPServer("test", "1.0.0", WithTaskHooks(hooks), WithTaskCapabilities(true, true, true), WithMaxConcurrentTasks(1))

	func() {
		defer func() { require.Equal(t, "hook bug", recover()) }()
		_, _ = s.createTask(t.Context(), "task-1", "tool", nil, nil)
	}()

	hooks.OnTaskCreated, hooks.OnTaskCancelled = nil, nil
	_, err := s.createTask(t.Context(), "task-2", "tool", nil, nil)
	require.NoError(t, err, "the slot of a task whose handler never started was kept")
}
