package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawValidationServer validates a tool whose limit may be at most 2^53.
func rawValidationServer(t testing.TB, hooks *Hooks) *MCPServer {
	t.Helper()
	opts := []ServerOption{WithToolCapabilities(false), WithInputSchemaValidation()}
	if hooks != nil {
		opts = append(opts, WithHooks(hooks))
	}
	srv := NewMCPServer("test", "1.0.0", opts...)
	srv.AddTool(mcp.NewToolWithRawSchema("limited", "a bounded limit", json.RawMessage(
		`{"type":"object","properties":{"limit":{"type":"integer","maximum":9007199254740992}}}`)),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})
	return srv
}

func callLimited(t testing.TB, srv *MCPServer, arguments string) string {
	t.Helper()
	out, err := json.Marshal(srv.HandleMessage(t.Context(), json.RawMessage(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"limited","arguments":`+arguments+`}}`)))
	require.NoError(t, err)
	return string(out)
}

// With no hook before the call, the arguments are validated as the client
// sent them: 2^53+1 is over the limit, though as a float64 it rounds onto it.
func TestInputValidationReadsTheArgumentsAsSent(t *testing.T) {
	srv := rawValidationServer(t, nil)
	assert.Contains(t, callLimited(t, srv, `{"limit":9007199254740993}`), `"isError":true`)
	assert.Contains(t, callLimited(t, srv, `{"limit":9007199254740992}`), `"text":"ok"`)
}

// A hook that runs before the call may change the arguments, so validation
// reads the arguments it left: here it brings an invalid limit into range.
func TestInputValidationReadsWhatAHookLeft(t *testing.T) {
	for name, hooks := range map[string]func() *Hooks{
		"before the call": func() *Hooks {
			h := &Hooks{}
			h.AddBeforeCallTool(func(_ context.Context, _ any, r *mcp.CallToolRequest) {
				r.Params.Arguments = map[string]any{"limit": 1}
			})
			return h
		},
		"before any request": func() *Hooks {
			h := &Hooks{}
			h.AddBeforeAny(func(_ context.Context, _ any, _ mcp.MCPMethod, message any) {
				if r, ok := message.(*mcp.CallToolRequest); ok {
					r.Params.Arguments = map[string]any{"limit": 1}
				}
			})
			return h
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := rawValidationServer(t, hooks())
			assert.Contains(t, callLimited(t, srv, `{"limit":"not a number"}`), `"text":"ok"`)
		})
	}
}

// Null arguments are validated as an empty object, as they always were.
func TestInputValidationTakesNullArgumentsAsEmpty(t *testing.T) {
	srv := rawValidationServer(t, nil)
	assert.Contains(t, callLimited(t, srv, `null`), `"text":"ok"`)
}

// Validated as sent, the arguments are not encoded again to be read: 77
// allocations when they were, 73 as sent, 70 with no span started for a
// server without a tracer, 67 with the arguments copied once, 65 with the
// context layers made in one allocation, 63 with the schema kept from
// registration, 62 with the arguments passed to the validator unboxed, 58
// with them read into Arguments in one pass, 42 validated from Arguments.
func TestInputValidationAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	srv := rawValidationServer(t, nil)
	msg := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"limited","arguments":{"limit":5}}}`)
	ctx := t.Context()
	allocs := testing.AllocsPerRun(1000, func() { srv.HandleMessage(ctx, msg) })
	assert.LessOrEqual(t, allocs, 42.0, "allocations per validated tools/call")
}
