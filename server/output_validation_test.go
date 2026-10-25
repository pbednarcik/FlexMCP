package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// weatherOutput mirrors the example in the issue: a structured result
// describing a temperature reading with required fields.
type weatherOutput struct {
	Temperature float64 `json:"temperature"`
	Unit        string  `json:"unit"`
}

// fakeWeatherTool returns a tool that declares an output schema for the
// weatherOutput shape via WithOutputSchema, matching the canonical usage
// pattern from the issue.
func fakeWeatherTool() mcp.Tool {
	return mcp.NewTool("get_weather",
		mcp.WithDescription("Get the current weather"),
		mcp.WithString("city", mcp.Required()),
		mcp.WithOutputSchema[weatherOutput](),
	)
}

// rawOutputSchemaTool returns a tool whose output schema is supplied via
// WithRawOutputSchema rather than the typed helper, so we can confirm the
// raw path participates in validation.
func rawOutputSchemaTool() mcp.Tool {
	return mcp.NewTool("raw_output",
		mcp.WithDescription("Tool with raw output schema"),
		mcp.WithRawOutputSchema(json.RawMessage(`{
			"type": "object",
			"properties": {
				"value": {"type": "string"}
			},
			"required": ["value"],
			"additionalProperties": false
		}`)),
	)
}

// TestOutputSchemaValidation covers the matrix of validation outcomes for a
// tool whose handler returns structured content. Each row exercises a
// single server lifecycle, registers the tool, makes one call, and asserts
// on either success or a tool execution error.
func TestOutputSchemaValidation(t *testing.T) {
	noOutputSchemaTool := mcp.NewTool("no_schema",
		mcp.WithDescription("Tool without an output schema"),
	)

	tests := []struct {
		name            string
		options         []ServerOption
		tool            mcp.Tool
		handlerResult   *mcp.CallToolResult
		wantSuccess     bool
		wantErrContains string
	}{
		{
			// Regression guard: with validation off, a non-conforming
			// structured result silently passes through to the client. Pinned
			// so we never flip the default to strict and break existing
			// servers that declare schemas but return loose output.
			name:    "disabled by default accepts non-conforming output",
			options: nil,
			tool:    fakeWeatherTool(),
			handlerResult: mcp.NewToolResultStructuredOnly(map[string]any{
				"temperature": "not-a-number",
			}),
			wantSuccess: true,
		},
		{
			// The motivating fix: with validation on, a structured result
			// that does not conform to the declared output schema must
			// surface as a tool execution error instead of being shipped to
			// the client.
			name:    "wrong type rejected",
			options: []ServerOption{WithOutputSchemaValidation()},
			tool:    fakeWeatherTool(),
			handlerResult: mcp.NewToolResultStructuredOnly(map[string]any{
				"temperature": "not-a-number",
				"unit":        "C",
			}),
			wantErrContains: "temperature",
		},
		{
			name:    "missing required field rejected",
			options: []ServerOption{WithOutputSchemaValidation()},
			tool:    fakeWeatherTool(),
			handlerResult: mcp.NewToolResultStructuredOnly(map[string]any{
				"temperature": 21.5,
			}),
			wantErrContains: "unit",
		},
		{
			name:    "valid structured content passes through",
			options: []ServerOption{WithOutputSchemaValidation()},
			tool:    fakeWeatherTool(),
			handlerResult: mcp.NewToolResultStructuredOnly(weatherOutput{
				Temperature: 21.5,
				Unit:        "C",
			}),
			wantSuccess: true,
		},
		{
			// Tools whose handlers return only text content (no
			// StructuredContent) are unaffected, even when the tool declares
			// an output schema. Validating absent structured content would
			// break a common pattern where the schema is documentation only.
			name:          "missing structured content is unaffected",
			options:       []ServerOption{WithOutputSchemaValidation()},
			tool:          fakeWeatherTool(),
			handlerResult: mcp.NewToolResultText("21.5C"),
			wantSuccess:   true,
		},
		{
			// Error results are diagnostic and need not match the success
			// shape declared by the output schema.
			name:          "error result skips validation",
			options:       []ServerOption{WithOutputSchemaValidation()},
			tool:          fakeWeatherTool(),
			handlerResult: mcp.NewToolResultError("upstream weather API unavailable"),
			wantSuccess:   false, // the result is itself an IsError result
		},
		{
			// A tool that does not declare an output schema is unaffected
			// regardless of what it returns.
			name:    "tool without output schema is unaffected",
			options: []ServerOption{WithOutputSchemaValidation()},
			tool:    noOutputSchemaTool,
			handlerResult: mcp.NewToolResultStructuredOnly(map[string]any{
				"anything": "goes",
			}),
			wantSuccess: true,
		},
		{
			name:    "raw output schema participates in validation",
			options: []ServerOption{WithOutputSchemaValidation()},
			tool:    rawOutputSchemaTool(),
			handlerResult: mcp.NewToolResultStructuredOnly(map[string]any{
				"value": "ok",
				"extra": "rejected",
			}),
			wantErrContains: "extra",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewMCPServer("test", "1.0.0", tt.options...)
			srv.AddTool(tt.tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return tt.handlerResult, nil
			})

			args := map[string]any{}
			if tt.tool.Name == "get_weather" {
				args["city"] = "London"
			}
			resp := callTool(t, srv, tt.tool.Name, args)

			switch {
			case tt.wantErrContains != "":
				requireToolErrorContaining(t, resp, tt.wantErrContains)
				// Output-validation failures are tagged with the
				// "output schema validation failed" prefix to distinguish
				// them from input validation errors. Verify the tag is
				// present so callers can disambiguate.
				jr := resp.(mcp.JSONRPCResponse)
				result := jr.Result.(*mcp.CallToolResult)
				tc := result.Content[0].(mcp.TextContent)
				assert.Contains(t, tc.Text, "output schema validation failed")
			case tt.wantSuccess:
				requireToolSuccess(t, resp)
			default:
				// Tests that explicitly want an error result without a
				// validation failure (handler returned an error result).
				jr, ok := resp.(mcp.JSONRPCResponse)
				require.True(t, ok)
				result, ok := jr.Result.(*mcp.CallToolResult)
				require.True(t, ok)
				require.True(t, result.IsError)
			}
		})
	}
}

// TestOutputSchemaValidation_DeleteToolInvalidatesCache makes a validated
// call to populate the output schema cache, deletes the tool, and asserts
// the cache entry is cleaned up so a tool re-added under the same name
// does not reuse a stale compilation.
func TestOutputSchemaValidation_DeleteToolInvalidatesCache(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithOutputSchemaValidation())
	srv.AddTool(fakeWeatherTool(), func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultStructuredOnly(weatherOutput{Temperature: 1, Unit: "C"}), nil
	})

	resp := callTool(t, srv, "get_weather", map[string]any{"city": "London"})
	requireToolSuccess(t, resp)
	require.Len(t, srv.outputValidator.cached, 1, "cache should be populated after a validated call")

	srv.DeleteTools("get_weather")
	require.Len(t, srv.outputValidator.cached, 0, "cache should be empty after DeleteTools")
}

// A call in flight when its tool is deleted caches no output schema for the
// deleted tool when it validates its result, on the synchronous path and the
// task-augmented one.
func TestOutputValidationInFlightCallCachesNoDeletedSchema(t *testing.T) {
	for _, task := range []bool{false, true} {
		name := "synchronous"
		if task {
			name = "task-augmented"
		}
		t.Run(name, func(t *testing.T) {
			srv := NewMCPServer("test", "1.0.0", WithOutputSchemaValidation(), WithTaskCapabilities(true, true, true))
			entered, proceed := make(chan struct{}), make(chan struct{})
			srv.AddTool(mcp.Tool{
				Name:            "x",
				RawOutputSchema: json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`),
				Execution:       &mcp.ToolExecution{TaskSupport: mcp.TaskSupportOptional},
			}, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				close(entered)
				<-proceed
				return mcp.NewToolResultStructuredOnly(map[string]any{"ok": true}), nil
			})
			req := mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "x"}}
			if task {
				req.Params.Task = &mcp.TaskParams{}
			}
			done := make(chan any, 1)
			go func() {
				res, _ := srv.handleToolCall(t.Context(), 1, req)
				done <- res
			}()

			<-entered
			srv.DeleteTools("x")
			require.NotContains(t, cachedDigests(srv.outputValidator.compiledSchemaCache), "x")
			close(proceed)
			if created, ok := (<-done).(*mcp.CreateTaskResult); ok {
				waitForTaskTerminal(t, srv, t.Context(), created.Task.TaskId)
			}
			assert.NotContains(t, cachedDigests(srv.outputValidator.compiledSchemaCache), "x",
				"a call in flight cached the deleted tool's output schema")
		})
	}
}

// outputRefused calls raw_output, task-augmented when task is set, and
// reports whether output validation refused its result.
func outputRefused(t *testing.T, srv *MCPServer, task bool) bool {
	t.Helper()
	req := mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "raw_output"}}
	if task {
		req.Params.Task = &mcp.TaskParams{}
	}
	res, reqErr := srv.handleToolCall(t.Context(), 1, req)
	require.Nil(t, reqErr)
	var isError bool
	var content []mcp.Content
	if created, ok := res.(*mcp.CreateTaskResult); ok {
		waitForTaskTerminal(t, srv, t.Context(), created.Task.TaskId)
		taskResult, resErr := srv.handleTaskResult(t.Context(), 2, mcp.TaskResultRequest{Params: mcp.TaskResultParams{TaskId: created.Task.TaskId}})
		require.Nil(t, resErr)
		isError, content = taskResult.IsError, taskResult.Content
	} else {
		result, ok := res.(*mcp.CallToolResult)
		require.True(t, ok, "expected *mcp.CallToolResult, got %T", res)
		isError, content = result.IsError, result.Content
	}
	if !isError {
		return false
	}
	tc, ok := content[0].(mcp.TextContent)
	require.True(t, ok, "expected TextContent, got %T", content[0])
	require.Contains(t, tc.Text, "output schema validation failed")
	return true
}

// A call validates its result against the output schema registration kept,
// not the tool's own: with the kept schema blanked, a result the tool's
// schema refuses gets through, on the synchronous path and both task paths.
func TestOutputValidationCallsValidateAgainstTheKeptSchema(t *testing.T) {
	bad := map[string]any{"value": 1}
	for name, tc := range map[string]struct {
		register func(*MCPServer)
		task     bool
	}{
		"synchronous": {func(s *MCPServer) {
			s.AddTool(withTasks(rawOutputSchemaTool()), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultStructuredOnly(bad), nil
			})
		}, false},
		"task-augmented": {func(s *MCPServer) {
			s.AddTool(withTasks(rawOutputSchemaTool()), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultStructuredOnly(bad), nil
			})
		}, true},
		"task tool": {func(s *MCPServer) {
			s.AddTaskTool(withTasks(rawOutputSchemaTool()), func(context.Context, mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
				return &mcp.CreateTaskResult{StructuredContent: bad}, nil
			})
		}, true},
	} {
		t.Run(name, func(t *testing.T) {
			srv := NewMCPServer("test", "1.0.0", WithOutputSchemaValidation(), WithTaskCapabilities(true, true, true))
			tc.register(srv)
			require.True(t, outputRefused(t, srv, tc.task), "the tool's schema as registered")
			srv.toolOutputSchemas["raw_output"] = nil
			assert.False(t, outputRefused(t, srv, tc.task), "the call read the tool's schema instead of the kept one")
		})
	}
}

// A tools/call whose structured result is validated against the tool's
// output schema validates against the schema registration kept, with no
// per-call digest or cache lookup.
func TestOutputValidationAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithOutputSchemaValidation())
	srv.AddTool(rawOutputSchemaTool(), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultStructuredOnly(map[string]any{"value": "ok"}), nil
	})
	ctx := t.Context()
	msg := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"raw_output"}}`)
	out, err := json.Marshal(srv.HandleMessage(ctx, msg))
	require.NoError(t, err)
	require.Contains(t, string(out), `"value":"ok"`)
	require.NotContains(t, string(out), `"isError":true`)

	allocs := testing.AllocsPerRun(1000, func() { srv.HandleMessage(ctx, msg) })
	assert.LessOrEqual(t, allocs, 51.0, "allocations per tools/call with a validated structured result")
}

// TestOutputSchemaValidation_TypedHandler exercises the canonical end-to-end
// path from the issue: a tool declared with WithOutputSchema[T] whose
// handler is wired up via NewStructuredToolHandler returning a Go struct.
// Because the handler returns a typed value, the only way it can produce a
// non-conforming result is by returning a different type — which we
// simulate by registering the typed schema against a handler that returns
// a deliberately-wrong map.
func TestOutputSchemaValidation_TypedHandler(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithOutputSchemaValidation())

	// Happy path: a properly-typed result passes through.
	srv.AddTool(fakeWeatherTool(), mcp.NewStructuredToolHandler(
		func(_ context.Context, _ mcp.CallToolRequest, _ struct {
			City string `json:"city"`
		}) (weatherOutput, error) {
			return weatherOutput{Temperature: 21.5, Unit: "C"}, nil
		},
	))
	resp := callTool(t, srv, "get_weather", map[string]any{"city": "London"})
	result := requireToolSuccess(t, resp)
	require.NotNil(t, result.StructuredContent)

	// Re-register the tool with a handler that returns a non-conforming
	// shape. The validator should reject it before it reaches the client.
	srv.DeleteTools("get_weather")
	srv.AddTool(fakeWeatherTool(), func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultStructuredOnly(map[string]any{
			"temperature": "string-not-number",
			"unit":        "C",
		}), nil
	})
	resp = callTool(t, srv, "get_weather", map[string]any{"city": "London"})
	requireToolErrorContaining(t, resp, "temperature")
}

// waitForTaskTerminal polls the server until the named task reaches a
// terminal status or the deadline elapses, returning the latest observed
// status. It exists to keep the task-validation tests focused on the
// validation behaviour rather than retry plumbing.
func waitForTaskTerminal(t *testing.T, srv *MCPServer, ctx context.Context, taskID string) mcp.TaskStatus {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var status mcp.TaskStatus
	for time.Now().Before(deadline) {
		task, _, err := srv.getTask(ctx, taskID)
		require.NoError(t, err)
		status = task.Status
		if status.IsTerminal() {
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("task %s did not reach terminal status (last: %s)", taskID, status)
	return status
}

// TestOutputSchemaValidation_TaskAugmented_RegularTool exercises the
// hybrid-mode path: a regular tool with TaskSupportOptional invoked with a
// task param, whose handler returns non-conforming StructuredContent. The
// validator must intercept the bad result before it is persisted to the
// task entry, so that tasks/result surfaces a validation error instead of
// the original payload.
func TestOutputSchemaValidation_TaskAugmented_RegularTool(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0",
		WithTaskCapabilities(true, true, true),
		WithOutputSchemaValidation(),
	)

	tool := mcp.NewTool("get_weather",
		mcp.WithDescription("Get the current weather"),
		mcp.WithString("city", mcp.Required()),
		mcp.WithOutputSchema[weatherOutput](),
		mcp.WithTaskSupport(mcp.TaskSupportOptional),
	)
	srv.AddTool(tool, func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultStructuredOnly(map[string]any{
			"temperature": "not-a-number",
			"unit":        "C",
		}), nil
	})

	ctx := t.Context()
	createRes, reqErr := srv.handleToolCall(ctx, 1, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "get_weather",
			Arguments: map[string]any{"city": "London"},
			Task:      &mcp.TaskParams{},
		},
	})
	require.Nil(t, reqErr)
	created, ok := createRes.(*mcp.CreateTaskResult)
	require.True(t, ok, "task-augmented call must return CreateTaskResult")
	taskID := created.Task.TaskId
	require.NotEmpty(t, taskID)

	status := waitForTaskTerminal(t, srv, ctx, taskID)
	assert.Equal(t, mcp.TaskStatusCompleted, status,
		"task should complete (the validation error is recorded as a successful tool error result, not a task failure)")

	taskResult, resErr := srv.handleTaskResult(ctx, 2, mcp.TaskResultRequest{
		Params: mcp.TaskResultParams{TaskId: taskID},
	})
	require.Nil(t, resErr)
	require.NotNil(t, taskResult)

	assert.True(t, taskResult.IsError, "validation failure must surface as a tool execution error")
	require.NotEmpty(t, taskResult.Content)
	tc, ok := taskResult.Content[0].(mcp.TextContent)
	require.True(t, ok)
	assert.Contains(t, tc.Text, "output schema validation failed")
	assert.Contains(t, tc.Text, "temperature")
	assert.Nil(t, taskResult.StructuredContent,
		"the bad structured content must not survive to the client")
}

// TestOutputSchemaValidation_TaskTool exercises the task-only path: a tool
// registered via AddTaskTool whose handler returns a *CreateTaskResult
// carrying non-conforming StructuredContent. Validation must run on the
// CreateTaskResult before the task is completed.
func TestOutputSchemaValidation_TaskTool(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0",
		WithTaskCapabilities(true, true, true),
		WithOutputSchemaValidation(),
	)

	tool := mcp.Tool{
		Name:        "compute_weather",
		Description: "Compute weather asynchronously",
		Execution:   &mcp.ToolExecution{TaskSupport: mcp.TaskSupportRequired},
	}
	// Reuse the WithOutputSchema helper by piggy-backing on a NewTool call,
	// then merge its OutputSchema onto the task tool definition above so the
	// task tool path validates the same shape.
	template := mcp.NewTool("compute_weather",
		mcp.WithDescription("Compute weather asynchronously"),
		mcp.WithOutputSchema[weatherOutput](),
		mcp.WithTaskSupport(mcp.TaskSupportRequired),
	)
	tool.OutputSchema = template.OutputSchema
	tool.RawOutputSchema = template.RawOutputSchema

	srv.AddTaskTool(tool, func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
		return &mcp.CreateTaskResult{
			StructuredContent: map[string]any{
				"temperature": "definitely-not-a-number",
				"unit":        "C",
			},
		}, nil
	})

	ctx := t.Context()
	createRes, reqErr := srv.handleToolCall(ctx, 1, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "compute_weather",
			Task: &mcp.TaskParams{},
		},
	})
	require.Nil(t, reqErr)
	created, ok := createRes.(*mcp.CreateTaskResult)
	require.True(t, ok)
	taskID := created.Task.TaskId
	require.NotEmpty(t, taskID)

	status := waitForTaskTerminal(t, srv, ctx, taskID)
	assert.Equal(t, mcp.TaskStatusCompleted, status)

	taskResult, resErr := srv.handleTaskResult(ctx, 2, mcp.TaskResultRequest{
		Params: mcp.TaskResultParams{TaskId: taskID},
	})
	require.Nil(t, resErr)
	require.NotNil(t, taskResult)

	assert.True(t, taskResult.IsError, "validation failure must surface as a tool execution error")
	require.NotEmpty(t, taskResult.Content)
	tc, ok := taskResult.Content[0].(mcp.TextContent)
	require.True(t, ok)
	assert.Contains(t, tc.Text, "output schema validation failed")
	assert.Contains(t, tc.Text, "temperature")
	assert.Nil(t, taskResult.StructuredContent)
}

// TestOutputSchemaValidation_TaskAugmented_HappyPath confirms a conforming
// task result is left untouched: the structured payload reaches the client
// through tasks/result and IsError remains false.
func TestOutputSchemaValidation_TaskAugmented_HappyPath(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0",
		WithTaskCapabilities(true, true, true),
		WithOutputSchemaValidation(),
	)

	tool := mcp.NewTool("get_weather",
		mcp.WithDescription("Get the current weather"),
		mcp.WithString("city", mcp.Required()),
		mcp.WithOutputSchema[weatherOutput](),
		mcp.WithTaskSupport(mcp.TaskSupportOptional),
	)
	srv.AddTool(tool, func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultStructuredOnly(weatherOutput{Temperature: 21.5, Unit: "C"}), nil
	})

	ctx := t.Context()
	createRes, reqErr := srv.handleToolCall(ctx, 1, mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name:      "get_weather",
			Arguments: map[string]any{"city": "London"},
			Task:      &mcp.TaskParams{},
		},
	})
	require.Nil(t, reqErr)
	created := createRes.(*mcp.CreateTaskResult)
	taskID := created.Task.TaskId

	status := waitForTaskTerminal(t, srv, ctx, taskID)
	assert.Equal(t, mcp.TaskStatusCompleted, status)

	taskResult, resErr := srv.handleTaskResult(ctx, 2, mcp.TaskResultRequest{
		Params: mcp.TaskResultParams{TaskId: taskID},
	})
	require.Nil(t, resErr)
	require.NotNil(t, taskResult)
	assert.False(t, taskResult.IsError)
	require.NotNil(t, taskResult.StructuredContent)
}
