package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// A hybrid tool that panics under task augmentation fails its task, as a
// task tool's panic does; it does not end the process.
func TestExecuteRegularToolAsTask_PanicRecovery(t *testing.T) {
	s := NewMCPServer("test", "1.0.0", WithTaskCapabilities(true, true, true))
	s.AddTool(mcp.Tool{Name: "hybrid", Execution: &mcp.ToolExecution{TaskSupport: mcp.TaskSupportOptional}},
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			panic("deliberate panic in hybrid handler")
		})

	created, reqErr := s.handleTaskAugmentedToolCall(t.Context(), 1,
		mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "hybrid", Task: &mcp.TaskParams{}}})
	require.Nil(t, reqErr)
	taskID := created.Task.TaskId

	_, done, err := s.getTask(t.Context(), taskID)
	require.NoError(t, err)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the task of a panicking hybrid handler never ended")
	}

	task, _, err := s.getTask(t.Context(), taskID)
	require.NoError(t, err)
	assert.Equal(t, mcp.TaskStatusFailed, task.Status)
	assert.Equal(t, internalErrorMessage, task.StatusMessage)
	s.tasksMu.RLock()
	defer s.tasksMu.RUnlock()
	assert.ErrorContains(t, s.tasks[taskID].resultErr, "deliberate panic in hybrid handler")
}
