package main

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"
	"weak"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
)

// handOverHolding hands a call of srv's "job" tool over as a task, on a
// request whose context holds a value only it refers to, and returns a weak
// pointer to that value.
func handOverHolding(t *testing.T, srv *server.MCPServer) weak.Pointer[[1024]byte] {
	t.Helper()
	type heldKey struct{}
	held := new([1024]byte)
	request := weak.Make(held)
	ctx := context.WithValue(t.Context(), heldKey{}, held)
	reply := srv.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"job","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"test","version":"1"},"io.modelcontextprotocol/clientCapabilities":{"extensions":{"io.modelcontextprotocol/tasks":{}}}}}}`))
	encoded, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	var answer struct {
		Result struct {
			TaskID string `json:"taskId"`
		} `json:"result"`
	}
	if err := json.Unmarshal(encoded, &answer); err != nil {
		t.Fatal(err)
	}
	if answer.Result.TaskID == "" {
		t.Fatalf("the call was not handed over: %s", encoded)
	}
	return request
}

// An ended job keeps its outcome, not the request it was handed over from:
// its cancellation closure, which holds the request's context, is let go
// when the work returns, as server.MemoryTasks lets go of it.
func TestAnEndedJobLetsGoOfItsRequest(t *testing.T) {
	reg := newRegistry()
	srv := server.NewMCPServer("jobs", "1.0.0", server.WithToolCapabilities(false), server.WithTaskHandler(reg))
	srv.AddTool(mcp.NewTool("job"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return reg.start(ctx, func(context.Context) (*mcp.CallToolResult, error) { return mcp.NewToolResultText("done"), nil })
	})
	request := handOverHolding(t, srv)

	deadline := time.Now().Add(5 * time.Second)
	for {
		reg.mu.Lock()
		ended := len(reg.jobs) == 1
		for _, j := range reg.jobs {
			ended = ended && j.status == mcp.TaskStatusCompleted
		}
		reg.mu.Unlock()
		if ended {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the job did not complete")
		}
		time.Sleep(time.Millisecond)
	}

	// The work's goroutine may still be returning, its context on its stack.
	for range 200 {
		runtime.GC()
		if request.Value() == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if request.Value() != nil {
		t.Fatal("the completed job still holds the request's context")
	}
	runtime.KeepAlive(reg)
}
