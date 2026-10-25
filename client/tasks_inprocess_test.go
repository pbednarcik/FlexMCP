package client

import (
	"context"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// taskFixture connects a client to a server with a task tool that finishes
// at once and one that runs until the test releases it.
type taskFixture struct {
	server  *server.MCPServer
	client  *Client
	session *sessionCapture
	release chan struct{}
}

func newTaskFixture(t *testing.T, version string) *taskFixture {
	t.Helper()
	hooks := &server.Hooks{}
	f := &taskFixture{session: captureSession(hooks), release: make(chan struct{})}
	t.Cleanup(func() { close(f.release) })
	f.server = server.NewMCPServer("test-server", "1.0.0", server.WithTaskCapabilities(true, true, true), server.WithHooks(hooks))
	f.server.AddTool(
		mcp.NewTool("noop", mcp.WithTaskSupport(mcp.TaskSupportRequired)),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		},
	)
	f.server.AddTool(
		mcp.NewTool("slow", mcp.WithTaskSupport(mcp.TaskSupportRequired)),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			<-f.release
			return mcp.NewToolResultText("late"), nil
		},
	)
	f.client = connectInProcess(t, f.server, version)
	return f
}

// createTask starts tool as a task on the client's session and returns the
// task ID.
func (f *taskFixture) createTask(t *testing.T, tool string) string {
	t.Helper()
	response := f.server.HandleMessage(f.server.WithContext(t.Context(), f.session.get(t)), []byte(`{
		"jsonrpc": "2.0",
		"id": 1,
		"method": "tools/call",
		"params": {"name": "`+tool+`", "task": {"ttl": 60000}}
	}`))
	success, ok := response.(mcp.JSONRPCResponse)
	require.Truef(t, ok, "expected JSONRPCResponse, got %T", response)
	created, ok := success.Result.(*mcp.CreateTaskResult)
	require.Truef(t, ok, "expected *CreateTaskResult, got %T", success.Result)
	return created.Task.TaskId
}

func TestCancelTaskStopsARunningTool(t *testing.T) {
	f := newTaskFixture(t, mcp.LATEST_LEGACY_PROTOCOL_VERSION)
	id := f.createTask(t, "slow")
	working, err := f.client.GetTask(t.Context(), mcp.GetTaskRequest{Params: mcp.GetTaskParams{TaskId: id}})
	require.NoError(t, err)
	require.Equal(t, mcp.TaskStatusWorking, working.Status)

	result, err := f.client.CancelTask(t.Context(), mcp.CancelTaskRequest{Params: mcp.CancelTaskParams{TaskId: id}})
	require.NoError(t, err)
	assert.Equal(t, id, result.TaskId)
	assert.Equal(t, mcp.TaskStatusCancelled, result.Status)

	cancelled, err := f.client.GetTask(t.Context(), mcp.GetTaskRequest{Params: mcp.GetTaskParams{TaskId: id}})
	require.NoError(t, err)
	assert.Equal(t, mcp.TaskStatusCancelled, cancelled.Status)

	_, err = f.client.CancelTask(t.Context(), mcp.CancelTaskRequest{Params: mcp.CancelTaskParams{TaskId: id}})
	require.ErrorContains(t, err, "terminal", "a finished task cannot be cancelled again")

	_, err = f.client.CancelTask(t.Context(), mcp.CancelTaskRequest{Params: mcp.CancelTaskParams{TaskId: "missing"}})
	require.Error(t, err)
}

func TestTaskResultReturnsTheFinishedToolResult(t *testing.T) {
	f := newTaskFixture(t, mcp.LATEST_LEGACY_PROTOCOL_VERSION)
	id := f.createTask(t, "noop")

	result, err := f.client.TaskResult(t.Context(), mcp.TaskResultRequest{Params: mcp.TaskResultParams{TaskId: id}})
	require.NoError(t, err)
	require.Len(t, result.Content, 1)
	assert.Equal(t, "ok", result.Content[0].(mcp.TextContent).Text)

	_, err = f.client.TaskResult(t.Context(), mcp.TaskResultRequest{Params: mcp.TaskResultParams{TaskId: "missing"}})
	require.Error(t, err)
}

func TestTaskResultIsRefusedOnAModernConnection(t *testing.T) {
	f := newTaskFixture(t, "")
	require.Equal(t, mcp.ProtocolVersion20260728, f.client.ProtocolVersion())

	_, err := f.client.TaskResult(t.Context(), mcp.TaskResultRequest{Params: mcp.TaskResultParams{TaskId: "any"}})
	require.ErrorContains(t, err, string(mcp.MethodTasksResult))
	assert.ErrorContains(t, err, "removed")
}
