package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// onDemandSeen records what each reader of a call saw of its arguments.
type onDemandSeen struct {
	argumentsNil map[string]bool
	region       map[string]string
}

func newOnDemandSeen() *onDemandSeen {
	return &onDemandSeen{argumentsNil: map[string]bool{}, region: map[string]string{}}
}

func (s *onDemandSeen) see(reader string, r *mcp.CallToolRequest) {
	s.argumentsNil[reader] = r.Params.Arguments == nil
	s.region[reader] = r.GetString("region", "")
}

// onDemandServer serves a tool echo whose handler, middleware and hooks
// record what they see; onDemand registers it with ArgumentsOnDemand.
func onDemandServer(t testing.TB, onDemand bool, seen *onDemandSeen, opts ...ServerOption) *MCPServer {
	t.Helper()
	hooks := &Hooks{}
	hooks.AddBeforeCallTool(func(_ context.Context, _ any, r *mcp.CallToolRequest) { seen.see("before hook", r) })
	hooks.AddAfterCallTool(func(_ context.Context, _ any, r *mcp.CallToolRequest, _ any) { seen.see("after hook", r) })
	opts = append([]ServerOption{
		WithToolCapabilities(false), WithHooks(hooks),
		WithToolHandlerMiddleware(func(next ToolHandlerFunc) ToolHandlerFunc {
			return func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				seen.see("middleware", &r)
				return next(ctx, r)
			}
		}),
	}, opts...)
	srv := NewMCPServer("test", "1.0.0", opts...)
	srv.AddTools(ServerTool{
		Tool: mcp.NewTool("echo", mcp.WithString("region")),
		Handler: func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			seen.see("handler", &r)
			return mcp.NewToolResultText(r.GetString("region", "none")), nil
		},
		ArgumentsOnDemand: onDemand,
	})
	return srv
}

func onDemandCall(t testing.TB, srv *MCPServer, arguments string) string {
	t.Helper()
	out, err := json.Marshal(srv.HandleMessage(t.Context(), json.RawMessage(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":`+arguments+`}}`)))
	require.NoError(t, err)
	return string(out)
}

// Under the opt-in every reader sees Params.Arguments nil and reads the
// arguments through the getters; without it, every reader sees the map.
func TestArgumentsOnDemandReaders(t *testing.T) {
	for _, onDemand := range []bool{false, true} {
		t.Run(fmt.Sprint("on demand ", onDemand), func(t *testing.T) {
			seen := newOnDemandSeen()
			out := onDemandCall(t, onDemandServer(t, onDemand, seen), `{"region":"eu-west-1"}`)
			assert.Contains(t, out, `"text":"eu-west-1"`)
			for _, reader := range []string{"before hook", "middleware", "handler", "after hook"} {
				assert.Equal(t, onDemand, seen.argumentsNil[reader], "%s: Params.Arguments nil", reader)
				assert.Equal(t, "eu-west-1", seen.region[reader], "%s: GetString", reader)
			}
		})
	}
}

// A typed handler, which binds RawArguments, works unchanged under the opt-in.
func TestArgumentsOnDemandTypedHandler(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	type args struct {
		Region string `json:"region"`
		Limit  int    `json:"limit"`
	}
	srv.AddTools(ServerTool{
		Tool: mcp.NewTool("echo"),
		Handler: mcp.NewTypedToolHandler(func(_ context.Context, r mcp.CallToolRequest, a args) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(fmt.Sprintf("%s/%d/%v", a.Region, a.Limit, r.Params.Arguments == nil)), nil
		}),
		ArgumentsOnDemand: true,
	})
	assert.Contains(t, onDemandCall(t, srv, `{"region":"r","limit":3}`), `"text":"r/3/true"`)
}

// A call whose arguments json.Unmarshal refuses fails as it did, opt-in or not.
func TestArgumentsOnDemandErrorsUnchanged(t *testing.T) {
	for _, arguments := range []string{`{"region":1e400}`, `[1,-1e999]`} {
		eager := onDemandCall(t, onDemandServer(t, false, newOnDemandSeen()), arguments)
		onDemand := onDemandCall(t, onDemandServer(t, true, newOnDemandSeen()), arguments)
		assert.Contains(t, eager, `"error"`, arguments)
		assert.Equal(t, eager, onDemand, arguments)
	}
}

// Input validation reads what it read before under the opt-in: the arguments
// as sent, or, with a hook before the call, the map the getters share or what
// the hook left.
func TestArgumentsOnDemandValidation(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","maximum":9007199254740992}}}`)
	server := func(onDemand bool, hook func(*mcp.CallToolRequest)) *MCPServer {
		opts := []ServerOption{WithToolCapabilities(false), WithInputSchemaValidation()}
		if hook != nil {
			h := &Hooks{}
			h.AddBeforeCallTool(func(_ context.Context, _ any, r *mcp.CallToolRequest) { hook(r) })
			opts = append(opts, WithHooks(h))
		}
		srv := NewMCPServer("test", "1.0.0", opts...)
		srv.AddTools(ServerTool{Tool: mcp.NewToolWithRawSchema("echo", "", schema), Handler: okHandler, ArgumentsOnDemand: onDemand})
		return srv
	}
	noop := func(*mcp.CallToolRequest) {}
	assign := func(r *mcp.CallToolRequest) { r.Params.Arguments = map[string]any{"limit": 1} }
	for _, tc := range []struct {
		hook      func(*mcp.CallToolRequest)
		arguments string
	}{
		{nil, `{"limit":5}`}, {nil, `{"limit":"x"}`}, {nil, `{"limit":9007199254740993}`}, {nil, `[1]`}, {nil, `null`},
		{noop, `{"limit":5}`}, {noop, `{"limit":"x"}`}, {noop, `{"limit":9007199254740993}`}, {noop, `null`},
		{assign, `{"limit":"x"}`},
	} {
		eager := onDemandCall(t, server(false, tc.hook), tc.arguments)
		onDemand := onDemandCall(t, server(true, tc.hook), tc.arguments)
		assert.Equal(t, eager, onDemand, "hook %v, arguments %s", tc.hook != nil, tc.arguments)
	}
	assert.Contains(t, onDemandCall(t, server(true, assign), `{"limit":"x"}`), `"text":"ok"`, "a hook's assignment wins")
	assert.Contains(t, onDemandCall(t, server(true, nil), `{"limit":"x"}`), `"isError":true`)
}

// A server with no tool that opted in never looks a tool up while decoding.
func TestArgumentsOnDemandGate(t *testing.T) {
	srv := onDemandServer(t, false, newOnDemandSeen())
	assert.False(t, srv.argumentsOnDemand.Load())
	srv.AddTools(ServerTool{Tool: mcp.NewTool("lazy"), Handler: okHandler, ArgumentsOnDemand: true})
	assert.True(t, srv.argumentsOnDemand.Load())
	assert.True(t, srv.toolReadsArgumentsOnDemand("lazy"))
	assert.False(t, srv.toolReadsArgumentsOnDemand("echo"))
	assert.True(t, srv.ListTools()["lazy"].ArgumentsOnDemand, "ListTools copies the opt-in")
}

// complexTypedCall is a typed tool of bench/compare's complex shape and a call
// to it.
func complexTypedCall(t testing.TB, onDemand bool) (*MCPServer, json.RawMessage) {
	t.Helper()
	var args strings.Builder
	args.WriteString(`{"items":[`)
	for i := range 20 {
		if i > 0 {
			args.WriteByte(',')
		}
		fmt.Fprintf(&args, `{"id":%d,"name":"item-%02d","tags":["alpha","beta","gamma"]}`, i, i)
	}
	args.WriteByte(']')
	for i := range 40 {
		fmt.Fprintf(&args, `,"p%03d":"value %03d"`, i, i)
	}
	args.WriteString(`,"region":"us-east-1"}`)
	type item struct {
		ID   int      `json:"id"`
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	type complexArgs struct {
		Region string `json:"region"`
		Items  []item `json:"items"`
	}
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	srv.AddTools(ServerTool{
		Tool: mcp.NewTool("complex"),
		Handler: mcp.NewTypedToolHandler(func(_ context.Context, _ mcp.CallToolRequest, a complexArgs) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(a.Region), nil
		}),
		ArgumentsOnDemand: onDemand,
	})
	return srv, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"complex","arguments":` + args.String() + `}}`)
}

// A typed complex call to a tool that opted in builds no arguments map: 259
// allocations decoded at once, 105 on demand.
func TestArgumentsOnDemandTypedComplexAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	ctx := t.Context()
	measure := func(onDemand bool) float64 {
		srv, msg := complexTypedCall(t, onDemand)
		out, err := json.Marshal(srv.HandleMessage(ctx, msg))
		require.NoError(t, err)
		require.Contains(t, string(out), `"text":"us-east-1"`)
		return testing.AllocsPerRun(200, func() { srv.HandleMessage(ctx, msg) })
	}
	eager, onDemand := measure(false), measure(true)
	t.Logf("allocations per typed complex call: %v at once, %v on demand", eager, onDemand)
	assert.LessOrEqual(t, onDemand, 105.0, "allocations per typed complex tools/call on demand")
}
