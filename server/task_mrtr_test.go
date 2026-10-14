package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// A task's handler that asks the client for input cannot be answered: that
// goes through tasks/update (Tasks.md row 15), which this server does not
// serve, so the task fails, with the synchronous call's error for a client
// that cannot be asked at all. It never ends as an empty completed task. A
// handler that asks nothing and sheds load instead fails the task as busy.
func TestATaskNeedingInputFails(t *testing.T) {
	modern := &RequestProtocolInfo{
		Modern:             true,
		ProtocolVersion:    mcp.ProtocolVersion20260728,
		ClientCapabilities: &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapability{}},
	}
	ask := func(b *InputRequestBuilder) *InputRequestBuilder {
		return b.Elicit("topic", mcp.ElicitationParams{
			Mode:            mcp.ElicitationModeForm,
			Message:         "Which topic?",
			RequestedSchema: map[string]any{"type": "object"},
		})
	}
	shed := func(b *InputRequestBuilder) *InputRequestBuilder { return b }
	for _, tt := range []struct {
		name    string
		info    *RequestProtocolInfo
		result  func(*InputRequestBuilder) *InputRequestBuilder
		wantErr error
	}{
		{"a legacy client", &RequestProtocolInfo{}, ask, ErrInputRequiresModernClient},
		{"a modern client", modern, ask, ErrTaskInputUnsupported},
		{"a modern client, load shed", modern, shed, ErrLoadShedding},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := NewMCPServer("test", "1.0.0", WithTaskCapabilities(true, true, true))
			s.AddTool(mcp.Tool{Name: "ask", Execution: &mcp.ToolExecution{TaskSupport: mcp.TaskSupportOptional}},
				func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return tt.result(NewInputRequestBuilder("step=1")).ToolResult(), nil
				})

			ctx := WithRequestProtocolInfo(t.Context(), tt.info)
			created, reqErr := s.handleTaskAugmentedToolCall(ctx, 1,
				mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "ask", Task: &mcp.TaskParams{}}})
			require.Nil(t, reqErr)
			_, done, err := s.getTask(ctx, created.Task.TaskId)
			require.NoError(t, err)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the task never ended")
			}

			task, _, err := s.getTask(ctx, created.Task.TaskId)
			require.NoError(t, err)
			assert.Equal(t, mcp.TaskStatusFailed, task.Status, "a task needing input ended %s", task.Status)
			s.tasksMu.RLock()
			defer s.tasksMu.RUnlock()
			assert.ErrorIs(t, s.tasks[created.Task.TaskId].resultErr, tt.wantErr)
		})
	}
}
