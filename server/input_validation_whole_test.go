package server

import (
	"context"
	"encoding/json"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wholeReading is what a handler's RequireWholeInt("limit") answered.
type wholeReading struct {
	n   int
	err error
}

// wholeServer serves the tools "bounded", "unbounded" and "unvalidated",
// which has no input schema, so validation skips it. Each calls
// RequireWholeInt on "limit" and sends what it answered to readings. With
// hook set, a before-hook that changes nothing is registered, so validation
// reads a round trip of the arguments.
func wholeServer(hook bool, readings chan<- wholeReading) *MCPServer {
	options := []ServerOption{WithToolCapabilities(false), WithInputSchemaValidation(), WithTaskCapabilities(true, true, true)}
	if hook {
		hooks := &Hooks{}
		hooks.AddBeforeCallTool(func(context.Context, any, *mcp.CallToolRequest) {})
		options = append(options, WithHooks(hooks))
	}
	srv := NewMCPServer("test", "1.0.0", options...)
	handler := func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		n, err := r.RequireWholeInt("limit")
		readings <- wholeReading{n, err}
		return mcp.NewToolResultText("ok"), nil
	}
	for name, schema := range map[string]string{
		"bounded":   `{"type":"object","properties":{"limit":{"type":"integer","maximum":9007199254740992}}}`,
		"unbounded": `{"type":"object","properties":{"limit":{"type":"integer"}}}`,
	} {
		tool := mcp.NewToolWithRawSchema(name, "", json.RawMessage(schema))
		tool.Execution = &mcp.ToolExecution{TaskSupport: mcp.TaskSupportOptional}
		srv.AddTool(tool, handler)
	}
	srv.AddTool(mcp.Tool{Name: "unvalidated", Execution: &mcp.ToolExecution{TaskSupport: mcp.TaskSupportOptional}}, handler)
	return srv
}

// A handler's strict integer is what validation judged: under a before-hook
// validation judged a round trip of the arguments, so a float64 of 2^53 or
// more is inexact there, and without one it is the client's literal. The
// task-augmented call is the second validation site.
func TestRequireWholeIntUnderABeforeHook(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("the rows are 64-bit values")
	}
	tests := []struct {
		name    string
		hook    bool
		tool    string
		limit   string
		refused string
		n       int64
		err     error
	}{
		{"hook: past 2^53 is inexact", true, "bounded", `9007199254740993`, "", 0, mcp.ErrArgumentInexact},
		{"hook: 2^53 is inexact", true, "bounded", `9007199254740992`, "", 0, mcp.ErrArgumentInexact},
		{"hook: below 2^53 is exact", true, "bounded", `9007199254740991`, "", 9007199254740991, nil},
		{"hook: a fraction that rounds to 1 is 1", true, "bounded", `1.0000000000000001`, "", 1, nil},
		{"hook: past the range of int", true, "unbounded", `9223372036854775808`, "", 0, mcp.ErrArgumentOutOfRange},
		{"hook: MaxInt64 rounds to 2^63, out of range", true, "unbounded", `9223372036854775807`, "", 0, mcp.ErrArgumentOutOfRange},
		{"hook: past 2^53 without a bound is inexact", true, "unbounded", `9007199254740993`, "", 0, mcp.ErrArgumentInexact},
		{"hook: a tool validation skips reads the literal", true, "unvalidated", `9007199254740993`, "", 9007199254740993, nil},
		{"no hook: past the bound is refused", false, "bounded", `9007199254740993`, "maximum", 0, nil},
		{"no hook: a fraction is refused", false, "bounded", `1.0000000000000001`, "integer", 0, nil},
		{"no hook: the literal is read", false, "unbounded", `9007199254740993`, "", 9007199254740993, nil},
		{"no hook: MaxInt64 is read", false, "unbounded", `9223372036854775807`, "", math.MaxInt64, nil},
		{"no hook: past the range of int", false, "unbounded", `9223372036854775808`, "", 0, mcp.ErrArgumentOutOfRange},
	}
	for _, tc := range tests {
		message := func(task string) string {
			return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tc.tool +
				`","arguments":{"limit":` + tc.limit + `}` + task + `}}`
		}
		check := func(t *testing.T, readings chan wholeReading, ran bool) {
			t.Helper()
			if tc.refused != "" {
				require.False(t, ran, "the handler ran for a call validation refuses")
				return
			}
			require.True(t, ran, "the handler did not run")
			got := <-readings
			if tc.err != nil {
				assert.ErrorIs(t, got.err, tc.err)
				return
			}
			require.NoError(t, got.err)
			assert.Equal(t, tc.n, int64(got.n))
		}

		t.Run(tc.name+", synchronous", func(t *testing.T) {
			readings := make(chan wholeReading, 1)
			out := handled(t, wholeServer(tc.hook, readings), message(""))
			if tc.refused != "" {
				assert.Contains(t, out, tc.refused)
			}
			check(t, readings, len(readings) == 1)
		})

		t.Run(tc.name+", task-augmented", func(t *testing.T) {
			readings := make(chan wholeReading, 1)
			srv := wholeServer(tc.hook, readings)
			resp := srv.HandleMessage(t.Context(), json.RawMessage(message(`,"task":{}`)))
			jr, ok := resp.(mcp.JSONRPCResponse)
			require.True(t, ok, "expected JSON-RPC response, got %T", resp)
			created, ok := jr.Result.(*mcp.CreateTaskResult)
			require.True(t, ok, "expected *mcp.CreateTaskResult, got %T", jr.Result)
			ran := false
			if tc.refused == "" {
				select {
				case got := <-readings:
					readings <- got
					ran = true
				case <-time.After(5 * time.Second):
				}
			}
			result, reqErr := srv.handleTaskResult(t.Context(), 2, mcp.TaskResultRequest{Params: mcp.TaskResultParams{TaskId: created.Task.TaskId}})
			require.Nil(t, reqErr)
			if tc.refused != "" {
				require.True(t, result.IsError)
				assert.Contains(t, result.Content[0].(mcp.TextContent).Text, tc.refused)
				ran = len(readings) == 1
			}
			check(t, readings, ran)
		})
	}
}

// A key of invalid UTF-8 decodes to the replacement character, so it is one
// name with every spelling that decodes to it. Whichever spelling is last, the
// handler's strict integer is the last value, the one validation judged.
func TestRequireWholeIntReadsTheKeyDecodingKept(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("the rows are 64-bit values")
	}
	const key = "\xef\xbf\xbd"
	escaped := `\` + "ufffd"
	bounded := `{"type":"object","properties":{"` + key + `":{"type":"integer","maximum":9007199254740992}}}`
	unbounded := `{"type":"object","properties":{"` + key + `":{"type":"integer"}}}`
	for _, tc := range []struct {
		name, schema, arguments string
		want                    int64
	}{
		{"invalid spelling last", bounded, `{"` + key + `":9007199254740993,"` + "\xff" + `":9007199254740992}`, 9007199254740992},
		{"two invalid spellings", bounded, `{"` + "\xfe" + `":9007199254740993,"` + "\xff" + `":9007199254740992}`, 9007199254740992},
		{"escaped spelling first", bounded, `{"` + escaped + `":9007199254740993,"` + "\xff" + `":9007199254740992}`, 9007199254740992},
		{"exact spelling last", bounded, `{"` + "\xff" + `":9007199254740993,"` + key + `":9007199254740992}`, 9007199254740992},
		{"escaped spelling last", bounded, `{"` + "\xff" + `":9007199254740993,"` + escaped + `":9007199254740992}`, 9007199254740992},
		{"the only spelling is invalid", unbounded, `{"` + "\xff" + `":9007199254740993}`, 9007199254740993},
	} {
		message := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"t","arguments":` + tc.arguments + `}}`)
		for _, onDemand := range []bool{false, true} {
			t.Run(tc.name+", on demand "+strconv.FormatBool(onDemand), func(t *testing.T) {
				readings := make(chan wholeReading, 1)
				srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation())
				srv.AddTools(ServerTool{
					Tool: mcp.NewToolWithRawSchema("t", "", json.RawMessage(tc.schema)),
					Handler: func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
						n, err := r.RequireWholeInt(key)
						readings <- wholeReading{n, err}
						return mcp.NewToolResultText("ok"), nil
					},
					ArgumentsOnDemand: onDemand,
				})
				srv.HandleMessage(t.Context(), message)
				require.Len(t, readings, 1, "the handler did not run")
				got := <-readings
				require.NoError(t, got.err)
				assert.Equal(t, tc.want, int64(got.n))
			})
		}
	}
}

// A request built in process carries no raw bytes, so the server leaves it
// unmarked even under a before-hook: 2^53 is the handler's own float64,
// never inexact, at both validation sites.
func TestRequireWholeIntInProcessUnderABeforeHook(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("2^53 is a 64-bit value")
	}
	call := func(task *mcp.TaskParams) mcp.CallToolRequest {
		return mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "bounded", Arguments: map[string]any{"limit": float64(1 << 53)}, Task: task}}
	}
	check := func(t *testing.T, readings chan wholeReading) {
		t.Helper()
		require.Len(t, readings, 1, "the handler did not run")
		got := <-readings
		require.NoError(t, got.err)
		assert.Equal(t, int64(1<<53), int64(got.n))
	}

	t.Run("synchronous", func(t *testing.T) {
		readings := make(chan wholeReading, 1)
		_, reqErr := wholeServer(true, readings).handleToolCall(t.Context(), 1, call(nil))
		require.Nil(t, reqErr)
		check(t, readings)
	})

	t.Run("task-augmented", func(t *testing.T) {
		readings := make(chan wholeReading, 1)
		srv := wholeServer(true, readings)
		created, reqErr := srv.handleTaskAugmentedToolCall(t.Context(), 1, call(&mcp.TaskParams{}))
		require.Nil(t, reqErr)
		_, reqErr = srv.handleTaskResult(t.Context(), 2, mcp.TaskResultRequest{Params: mcp.TaskResultParams{TaskId: created.Task.TaskId}})
		require.Nil(t, reqErr)
		check(t, readings)
	})
}

// Marking a request costs nothing on a call with no before-hook, and one
// allocation on a call with one: the mark goes through an indirect call, so
// what it marks is copied to the heap, but only in the branch that marks.
// The ceilings are a passing call's counts.
func TestMarkRoundTripAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	for _, tc := range []struct {
		name    string
		hook    bool
		ceiling float64
	}{
		{"no hook", false, 42},
		{"a before-hook", true, 51},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := []ServerOption{WithToolCapabilities(false), WithInputSchemaValidation()}
			if tc.hook {
				hooks := &Hooks{}
				hooks.AddBeforeCallTool(func(context.Context, any, *mcp.CallToolRequest) {})
				options = append(options, WithHooks(hooks))
			}
			srv := NewMCPServer("test", "1.0.0", options...)
			srv.AddTool(mcp.NewToolWithRawSchema("bounded", "", json.RawMessage(
				`{"type":"object","properties":{"limit":{"type":"integer","maximum":9007199254740992}}}`)),
				func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return mcp.NewToolResultText("ok"), nil
				})
			msg := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"bounded","arguments":{"limit":5}}}`)
			ctx := t.Context()
			require.Contains(t, handled(t, srv, string(msg)), `"text":"ok"`)
			allocs := testing.AllocsPerRun(200, func() { srv.HandleMessage(ctx, msg) })
			assert.LessOrEqual(t, allocs, tc.ceiling)
		})
	}
}
