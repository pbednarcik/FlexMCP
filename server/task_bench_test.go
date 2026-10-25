package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
)

// BenchmarkTaskToolCall measures a task-augmented tools/call to a tool that
// may run as a task, through to its result: the call that creates the task,
// the handler's run on the task's goroutine, and the tasks/result that waits
// for it. The finished task is then removed from the store, so the store
// does not grow with b.N.
func BenchmarkTaskToolCall(b *testing.B) {
	srv := NewMCPServer("bench", "1.0.0", WithToolCapabilities(false), WithTaskCapabilities(true, true, true))
	srv.AddTool(mcp.NewTool("echo", mcp.WithString("region"), mcp.WithTaskSupport(mcp.TaskSupportOptional)),
		func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(request.GetString("region", "")), nil
		})
	ctx := b.Context()
	call := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"region":"us-east-1"},"task":{}}}`)
	run := func() (created, result mcp.JSONRPCMessage) {
		created = srv.HandleMessage(ctx, call)
		response, _ := created.(mcp.JSONRPCResponse)
		task, _ := response.Result.(*mcp.CreateTaskResult)
		if task == nil {
			return created, nil
		}
		id := task.Task.TaskId
		result = srv.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tasks/result","params":{"taskId":"`+id+`"}}`))
		srv.tasksMu.Lock()
		delete(srv.tasks, id)
		srv.tasksMu.Unlock()
		return created, result
	}

	// One call outside the loop proves a task is created, its result is the
	// handler's, and the store keeps no task after it.
	created, result := run()
	createdOut, _ := json.Marshal(created)
	resultOut, _ := json.Marshal(result)
	srv.tasksMu.RLock()
	kept := len(srv.tasks)
	srv.tasksMu.RUnlock()
	if !strings.Contains(string(createdOut), `"status":"working"`) || kept != 0 ||
		!strings.Contains(string(resultOut), `"text":"us-east-1"`) || strings.Contains(string(resultOut), `"isError":true`) {
		b.Fatalf("created %s, result %s, tasks kept %d", createdOut, resultOut, kept)
	}

	b.ReportAllocs()
	for b.Loop() {
		run()
	}
}
