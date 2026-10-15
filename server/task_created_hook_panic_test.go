package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// A panic from a hook while a task is created fails the task: its goroutine
// never starts, so nothing else would end it; its TTL still expires it.
func TestACreatedHookPanicFailsTheTask(t *testing.T) {
	hooks := &TaskHooks{}
	hooks.AddOnTaskCreated(func(context.Context, TaskMetrics) { panic("hook bug") })
	failed := make(chan TaskMetrics, 1)
	hooks.AddOnTaskFailed(func(_ context.Context, m TaskMetrics) { failed <- m })
	s := NewMCPServer("test", "1.0.0", WithTaskHooks(hooks), WithTaskCapabilities(true, true, true),
		WithTaskTTL(500*time.Millisecond))

	func() {
		defer func() { require.Equal(t, "hook bug", recover()) }()
		_, _ = s.createTask(t.Context(), "task-1", "tool", nil, nil)
	}()

	task, _, err := s.getTask(t.Context(), "task-1")
	require.NoError(t, err)
	assert.Equal(t, mcp.TaskStatusFailed, task.Status)
	assert.Equal(t, internalErrorMessage, task.StatusMessage)
	select {
	case m := <-failed:
		assert.Equal(t, "task-1", m.TaskID)
	case <-time.After(time.Second):
		t.Fatal("the failed hook never ran")
	}
	require.Eventually(t, func() bool {
		_, _, err := s.getTask(t.Context(), "task-1")
		return err != nil && err.Error() == "task has expired"
	}, 5*time.Second, 5*time.Millisecond, "the task's TTL did not expire it")
}
