package server

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// relaxedListTool is fakeListTool without additionalProperties: false, so an
// unknown `cursor` argument passes.
func relaxedListTool() mcp.Tool {
	return mcp.NewTool("kubernetes_list",
		mcp.WithDescription("List Kubernetes resources"),
		mcp.WithString("resourceType", mcp.Required()),
		mcp.WithString("continue"),
	)
}

// callWithCursor calls kubernetes_list with the unknown `cursor` argument and
// reports whether validation refused it.
func callWithCursor(t *testing.T, srv *MCPServer) (refused bool) {
	t.Helper()
	resp := callTool(t, srv, "kubernetes_list", map[string]any{"resourceType": "pods", "cursor": "abc"})
	jr, ok := resp.(mcp.JSONRPCResponse)
	require.True(t, ok, "expected JSON-RPC response, got %T", resp)
	result, ok := jr.Result.(*mcp.CallToolResult)
	require.True(t, ok, "expected *mcp.CallToolResult, got %T", jr.Result)
	if !result.IsError {
		return false
	}
	tc, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected TextContent, got %T", result.Content[0])
	require.Contains(t, tc.Text, "cursor")
	return true
}

func okTaskHandler(context.Context, mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
	return &mcp.CreateTaskResult{}, nil
}

// withTasks returns tool with task augmentation allowed.
func withTasks(tool mcp.Tool) mcp.Tool {
	tool.Execution = &mcp.ToolExecution{TaskSupport: mcp.TaskSupportOptional}
	return tool
}

// callWithCursorAsTask makes callWithCursor's call task-augmented and reports
// whether validation refused it: a refused call's task is completed at once
// with the validation error, an accepted one's is still working.
func callWithCursorAsTask(t *testing.T, srv *MCPServer) (refused bool) {
	t.Helper()
	msg := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"kubernetes_list","arguments":{"resourceType":"pods","cursor":"abc"},"task":{}}}`)
	resp := srv.HandleMessage(t.Context(), msg)
	jr, ok := resp.(mcp.JSONRPCResponse)
	require.True(t, ok, "expected JSON-RPC response, got %T", resp)
	created, ok := jr.Result.(*mcp.CreateTaskResult)
	require.True(t, ok, "expected *mcp.CreateTaskResult, got %T", jr.Result)
	if created.Task.Status == mcp.TaskStatusWorking {
		return false
	}
	result, reqErr := srv.handleTaskResult(t.Context(), 2, mcp.TaskResultRequest{Params: mcp.TaskResultParams{TaskId: created.Task.TaskId}})
	require.Nil(t, reqErr)
	require.True(t, result.IsError, "the task ended at once without a validation error")
	tc, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected TextContent, got %T", result.Content[0])
	require.Contains(t, tc.Text, "cursor")
	return true
}

// keptSchemaPaths are the ways a tool is registered, each with the call that
// reaches its kept schema.
func keptSchemaPaths() map[string]struct {
	register func(*MCPServer, mcp.Tool)
	refused  func(*testing.T, *MCPServer) bool
} {
	type path = struct {
		register func(*MCPServer, mcp.Tool)
		refused  func(*testing.T, *MCPServer) bool
	}
	return map[string]path{
		"AddTool": {func(s *MCPServer, tool mcp.Tool) { s.AddTool(tool, okHandler) }, callWithCursor},
		"AddTools": {func(s *MCPServer, tool mcp.Tool) {
			s.AddTools(ServerTool{Tool: tool, Handler: okHandler})
		}, callWithCursor},
		"SetTools": {func(s *MCPServer, tool mcp.Tool) {
			s.SetTools(ServerTool{Tool: tool, Handler: okHandler})
		}, callWithCursor},
		"DeleteTools then AddTool": {func(s *MCPServer, tool mcp.Tool) {
			s.DeleteTools(tool.Name)
			s.AddTool(tool, okHandler)
		}, callWithCursor},
		"AddTool, task-augmented call": {func(s *MCPServer, tool mcp.Tool) {
			s.AddTool(withTasks(tool), okHandler)
		}, callWithCursorAsTask},
		"AddTaskTool": {func(s *MCPServer, tool mcp.Tool) {
			s.AddTaskTool(withTasks(tool), okTaskHandler)
		}, callWithCursorAsTask},
		"AddTaskTools": {func(s *MCPServer, tool mcp.Tool) {
			s.AddTaskTools(ServerTaskTool{Tool: withTasks(tool), Handler: okTaskHandler})
		}, callWithCursorAsTask},
	}
}

// The schema a call is validated against is the one the tool was last
// registered with, on every registration path, in both directions: a stricter
// schema refuses what the old one let through, and a relaxed one lets it
// through again.
func TestInputValidationReregistrationUsesTheNewSchema(t *testing.T) {
	for name, path := range keptSchemaPaths() {
		t.Run(name, func(t *testing.T) {
			srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation(), WithTaskCapabilities(true, true, true))
			path.register(srv, relaxedListTool())
			assert.False(t, path.refused(t, srv), "relaxed schema, first registration")
			path.register(srv, fakeListTool())
			assert.True(t, path.refused(t, srv), "strict schema registered over the relaxed one")
			path.register(srv, relaxedListTool())
			assert.False(t, path.refused(t, srv), "relaxed schema registered over the strict one")
		})
	}
}

// A call is validated against the schema registration kept, not the tool's
// own: with the kept schema blanked, arguments the tool's schema refuses get
// through, on every registration path and on both call paths.
func TestInputValidationCallsValidateAgainstTheKeptSchema(t *testing.T) {
	for name, path := range keptSchemaPaths() {
		t.Run(name, func(t *testing.T) {
			srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation(), WithTaskCapabilities(true, true, true))
			path.register(srv, fakeListTool())
			require.True(t, path.refused(t, srv), "the strict schema as registered")
			srv.toolInputSchemas["kubernetes_list"] = nil
			assert.False(t, path.refused(t, srv), "the call read the tool's schema instead of the kept one")
		})
	}
}

// Each way of registering a tool keeps its compiled input schema, and
// deleting or replacing tools drops their entries.
func TestInputValidationSchemaIsKeptOnEveryRegistrationPath(t *testing.T) {
	for name, path := range keptSchemaPaths() {
		t.Run(name, func(t *testing.T) {
			srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation())
			path.register(srv, fakeListTool())
			require.Contains(t, srv.toolInputSchemas, "kubernetes_list")
			assert.NotNil(t, srv.toolInputSchemas["kubernetes_list"])
			assert.Len(t, srv.toolInputSchemas, len(srv.tools)+len(srv.taskTools))
		})
	}

	t.Run("DeleteTools and SetTools drop the entry", func(t *testing.T) {
		srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation())
		srv.AddTool(fakeListTool(), okHandler)
		srv.DeleteTools("kubernetes_list")
		assert.NotContains(t, srv.toolInputSchemas, "kubernetes_list")

		srv.AddTool(fakeListTool(), okHandler)
		srv.SetTools(ServerTool{Tool: mcp.NewTool("other"), Handler: okHandler})
		assert.NotContains(t, srv.toolInputSchemas, "kubernetes_list")
		assert.Contains(t, srv.toolInputSchemas, "other")
	})
}

// Whether a schema holds draft 4 is asked by the first failure that needs the
// answer, which stays with the compiled schema: a call that passes does not
// ask, nor does a failure of another keyword, and a new schema starts unasked.
func TestInputValidationDraft4AnswerStaysWithTheCompiledSchema(t *testing.T) {
	register := func(srv *MCPServer, schema string) *cachedToolSchema {
		srv.AddTool(mcp.NewToolWithRawSchema("t", "", json.RawMessage(schema)), okHandler)
		return srv.toolInputSchemas["t"]
	}
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), refusingInputValidation())

	kept := register(srv, `{"type":"object","properties":{"n":{"exclusiveMaximum":5},"s":{"type":"string"}}}`)
	assert.Equal(t, draft4Unknown, kept.draft4.Load(), "registration does not ask")
	callTool(t, srv, "t", map[string]any{"n": 1})
	assert.Equal(t, draft4Unknown, kept.draft4.Load(), "a call that passes does not ask")
	callTool(t, srv, "t", map[string]any{"s": 1})
	assert.Equal(t, draft4Unknown, kept.draft4.Load(), "a failure of another keyword does not ask")
	callTool(t, srv, "t", map[string]any{"n": 9})
	assert.Equal(t, draft4Absent, kept.draft4.Load(), "an exclusive bound's failure asks")

	kept = register(srv, `{`+draft4+`,"type":"object","properties":{"n":{"maximum":5,"exclusiveMaximum":true}}}`)
	assert.Equal(t, draft4Unknown, kept.draft4.Load(), "a new schema starts unasked")
	callTool(t, srv, "t", map[string]any{"n": 5})
	assert.Equal(t, draft4Held, kept.draft4.Load())
}

// Failures of one schema may ask at the same moment; run with -race.
func TestInputValidationDraft4AnswerToConcurrentFailures(t *testing.T) {
	inv := draft4ExclusiveBranches(t, 2)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { assert.Len(t, projectFailure(inv).Violations, 2) })
	}
	wg.Wait()
	assert.Equal(t, draft4Held, inv.schema.draft4.Load())
}

// A raw schema registered again is read again too.
func TestInputValidationReregistrationUsesTheNewRawSchema(t *testing.T) {
	raw := func(additional bool) mcp.Tool {
		schema := `{"type":"object","properties":{"resourceType":{"type":"string"}},"additionalProperties":false}`
		if additional {
			schema = strings.Replace(schema, "false", "true", 1)
		}
		return mcp.NewToolWithRawSchema("kubernetes_list", "", json.RawMessage(schema))
	}
	srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation())
	srv.AddTool(raw(true), okHandler)
	assert.False(t, callWithCursor(t, srv))
	srv.AddTool(raw(false), okHandler)
	assert.True(t, callWithCursor(t, srv))
}

// Calls validate while the tool is registered again and again, synchronous
// calls to a tool and task-augmented calls to a task tool. Under -race this
// shows the kept schema is read under the same lock it is written under;
// every call is answered under one schema or the other.
func TestInputValidationCallsRaceReregistration(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation(), WithTaskCapabilities(true, true, true))
	taskTool := func(tool mcp.Tool) mcp.Tool {
		tool.Name = "kubernetes_list_task"
		return withTasks(tool)
	}
	srv.AddTool(relaxedListTool(), okHandler)
	srv.AddTaskTool(taskTool(relaxedListTool()), okTaskHandler)
	msg := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"kubernetes_list","arguments":{"resourceType":"pods","cursor":"abc"}}}`)
	taskMsg := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"kubernetes_list_task","arguments":{"resourceType":"pods","cursor":"abc"},"task":{}}}`)

	var registrar, callers sync.WaitGroup
	stop := make(chan struct{})
	registrar.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			switch i % 4 {
			case 0:
				srv.AddTool(fakeListTool(), okHandler)
			case 1:
				srv.SetTools(ServerTool{Tool: relaxedListTool(), Handler: okHandler})
			case 2:
				srv.AddTaskTool(taskTool(fakeListTool()), okTaskHandler)
			default:
				srv.AddTaskTools(ServerTaskTool{Tool: taskTool(relaxedListTool()), Handler: okTaskHandler})
			}
		}
	})
	for i := range 6 {
		// Two of the callers make task-augmented calls: the task is working
		// under the relaxed schema, completed with the error under the strict.
		m, accepted, refused := msg, `"text":"ok"`, `cursor`
		if i >= 4 {
			m, accepted, refused = taskMsg, `"status":"working"`, `"status":"completed"`
		}
		callers.Go(func() {
			for range 200 {
				out, err := json.Marshal(srv.HandleMessage(t.Context(), m))
				if err != nil {
					t.Error(err)
					return
				}
				if s := string(out); !strings.Contains(s, accepted) && !strings.Contains(s, refused) {
					t.Errorf("answered under neither schema: %s", s)
					return
				}
			}
		})
	}
	callers.Wait()
	close(stop)
	registrar.Wait()
}

// structSchemaTool is a tool whose input schema is built with mcp.NewTool's
// options, so it reaches the server as a structured InputSchema.
func structSchemaTool() mcp.Tool {
	return mcp.NewTool("structured",
		mcp.WithDescription("a tool with a structured input schema"),
		mcp.WithString("region", mcp.Required(), mcp.Description("the region to query"),
			mcp.Enum("us-east-1", "us-west-2", "eu-west-1", "ap-south-1")),
		mcp.WithNumber("limit", mcp.Description("the most rows to return"), mcp.Min(1), mcp.Max(1000)),
		mcp.WithBoolean("verbose", mcp.Description("include every column")),
		mcp.WithString("cursor", mcp.Description("where the previous page ended")),
		mcp.WithArray("tags", mcp.Description("tags to match"), mcp.WithStringItems()),
		mcp.WithObject("filter", mcp.Description("a filter on the rows"), mcp.Properties(map[string]any{
			"label": map[string]any{"type": "string"},
			"since": map[string]any{"type": "string", "format": "date-time"},
		})),
		mcp.WithSchemaAdditionalProperties(false),
	)
}

// A tool built with a structured input schema is validated against the
// schema registration compiled, not one marshalled and hashed again per call:
// 96 allocations when it was, 71 with the schema kept from registration, 70
// with the arguments passed to the validator unboxed, 62 with them read into
// Arguments in one pass, 42 validated from Arguments.
func TestInputValidationStructSchemaAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation())
	srv.AddTool(structSchemaTool(), okHandler)
	ctx := t.Context()
	msg := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"structured","arguments":{"region":"us-east-1","limit":5}}}`)
	out, err := json.Marshal(srv.HandleMessage(ctx, msg))
	require.NoError(t, err)
	require.Contains(t, string(out), `"text":"ok"`)
	refused, err := json.Marshal(srv.HandleMessage(ctx, json.RawMessage(strings.Replace(string(msg), `"limit":5`, `"limit":5000`, 1))))
	require.NoError(t, err)
	require.Contains(t, string(refused), `"isError":true`)

	allocs := testing.AllocsPerRun(1000, func() { srv.HandleMessage(ctx, msg) })
	assert.LessOrEqual(t, allocs, 42.0, "allocations per validated tools/call on a structured schema")
}
