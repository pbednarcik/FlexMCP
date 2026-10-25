package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// Arguments that are not an object validate under the opt-in exactly as they
// do without it, with or without a hook before the call: through their
// decode, as a call decoded at once carries it, when a hook may have changed
// them.
func TestArgumentsOnDemandValidationOfArgumentsNotAnObject(t *testing.T) {
	schema := json.RawMessage(`{"type":["object","array","string","number"],` +
		`"properties":{"limit":{"maximum":9007199254740992}},"items":{"type":"integer","maximum":9007199254740992}}`)
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
	assign := func(r *mcp.CallToolRequest) { r.Params.Arguments = []any{json.Number("9007199254740993")} }
	for _, hook := range []func(*mcp.CallToolRequest){nil, noop, assign} {
		for _, arguments := range []string{
			`[9007199254740993]`, `[9007199254740992]`, `[1.5]`, `[3]`, `[1e2]`, `[0e1000000]`, `["x"]`, `[]`,
			`"text"`, `9007199254740993`, `true`, `{"limit":9007199254740993}`,
		} {
			eager := onDemandCall(t, server(false, hook), arguments)
			onDemand := onDemandCall(t, server(true, hook), arguments)
			assert.Equal(t, eager, onDemand, "hook %v, arguments %s", hook != nil, arguments)
		}
	}
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

	// Tools registered through SetTools opt in as those AddTools registers.
	seen := newOnDemandSeen()
	srv = NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	srv.SetTools(ServerTool{
		Tool: mcp.NewTool("echo", mcp.WithString("region")),
		Handler: func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			seen.see("handler", &r)
			return mcp.NewToolResultText("ok"), nil
		},
		ArgumentsOnDemand: true,
	})
	assert.True(t, srv.argumentsOnDemand.Load(), "SetTools")
	assert.Contains(t, onDemandCall(t, srv, `{"region":"r"}`), `"text":"ok"`)
	assert.True(t, seen.argumentsNil["handler"], "SetTools: Params.Arguments nil in the handler")
	assert.Equal(t, "r", seen.region["handler"], "SetTools: GetString")
}

// A task-augmented call to an opted-in tool reads its arguments on demand as
// a synchronous one does.
func TestArgumentsOnDemandTaskCall(t *testing.T) {
	seen := make(chan bool, 1)
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithTaskCapabilities(true, true, true))
	srv.AddTools(ServerTool{
		Tool: mcp.NewTool("echo", mcp.WithString("region"), mcp.WithTaskSupport(mcp.TaskSupportOptional)),
		Handler: func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			seen <- r.Params.Arguments == nil
			return mcp.NewToolResultText(r.GetString("region", "none")), nil
		},
		ArgumentsOnDemand: true,
	})
	var request mcp.CallToolRequest
	require.NoError(t, srv.decodeToolCall(json.RawMessage(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"region":"r"},"task":{}}}`), &request))
	created, reqErr := srv.handleToolCall(t.Context(), 1, request)
	require.Nil(t, reqErr)
	assert.True(t, <-seen, "Params.Arguments nil in the handler of an opted-in tool run as a task")
	result, reqErr := srv.handleTaskResult(t.Context(), 2, mcp.TaskResultRequest{Params: mcp.TaskResultParams{TaskId: created.(*mcp.CreateTaskResult).Task.TaskId}})
	require.Nil(t, reqErr)
	assert.Equal(t, "r", result.Content[0].(mcp.TextContent).Text)
}

// A tool re-registered without the opt-in while a call to its opted-in
// predecessor is in flight sees the arguments as a call decoded at once
// carries them, here re-registered by the hook before the call.
func TestArgumentsOnDemandToolSwappedDuringTheCall(t *testing.T) {
	var srv *MCPServer
	seen := newOnDemandSeen()
	plain := ServerTool{
		Tool: mcp.NewTool("echo", mcp.WithString("region")),
		Handler: func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			seen.see("handler", &r)
			return mcp.NewToolResultText("ok"), nil
		},
	}
	hooks := &Hooks{}
	hooks.AddBeforeCallTool(func(context.Context, any, *mcp.CallToolRequest) { srv.SetTools(plain) })
	srv = NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithHooks(hooks))
	onDemand := plain
	onDemand.ArgumentsOnDemand = true
	srv.AddTools(onDemand)
	assert.Contains(t, onDemandCall(t, srv, `{"region":"r"}`), `"text":"ok"`)
	assert.False(t, seen.argumentsNil["handler"], "Params.Arguments nil in the handler of a tool that did not opt in")
	assert.Equal(t, "r", seen.region["handler"])
}

// The hooks after such a call, on success, on error and on the task path, see
// the arguments the handler saw; a tool that opted in still gives them nil.
func TestArgumentsOnDemandToolSwappedDuringTheCallHooks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fail    bool
		task    bool
		swapped bool
	}{
		{"success", false, false, true},
		{"error", true, false, true},
		{"task", false, true, true},
		{"opted in, success", false, false, false},
		{"opted in, error", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var srv *MCPServer
			seen := newOnDemandSeen()
			tool := mcp.NewTool("echo", mcp.WithString("region"))
			if tc.task {
				tool = mcp.NewTool("echo", mcp.WithString("region"), mcp.WithTaskSupport(mcp.TaskSupportOptional))
			}
			plain := ServerTool{
				Tool: tool,
				Handler: func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					if tc.fail {
						return nil, errors.New("refused")
					}
					return mcp.NewToolResultText("ok"), nil
				},
			}
			hooks := &Hooks{}
			hooks.AddBeforeCallTool(func(context.Context, any, *mcp.CallToolRequest) {
				if tc.swapped {
					srv.SetTools(plain)
				}
			})
			hooks.AddAfterCallTool(func(_ context.Context, _ any, r *mcp.CallToolRequest, _ any) { seen.see("after hook", r) })
			hooks.AddOnError(func(_ context.Context, _ any, method mcp.MCPMethod, message any, _ error) {
				if r, ok := message.(*mcp.CallToolRequest); ok && method == mcp.MethodToolsCall {
					seen.see("error hook", r)
				}
			})
			srv = NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithTaskCapabilities(true, true, true), WithHooks(hooks))
			onDemand := plain
			onDemand.ArgumentsOnDemand = true
			srv.AddTools(onDemand)
			params := `{"name":"echo","arguments":{"region":"r"}}`
			if tc.task {
				params = `{"name":"echo","arguments":{"region":"r"},"task":{}}`
			}
			_ = srv.HandleMessage(t.Context(), json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+params+`}`))
			reader := "after hook"
			if tc.fail {
				reader = "error hook"
			}
			require.Contains(t, seen.argumentsNil, reader, "the hook did not run")
			assert.Equal(t, !tc.swapped, seen.argumentsNil[reader], "%s: Params.Arguments nil", reader)
			assert.Equal(t, "r", seen.region[reader], "%s: GetString", reader)
		})
	}
}

// Validating an opted-in tool's arguments that are not an object, with a hook
// before the call, decodes them for the validator alone: the handler and the
// hooks after the call, on success and on error, still see Params.Arguments
// nil.
func TestArgumentsOnDemandValidationLeavesTheHooksNil(t *testing.T) {
	for _, arguments := range []string{`[1]`, `42`, `"text"`} {
		for _, fail := range []bool{false, true} {
			seen := newOnDemandSeen()
			hooks := &Hooks{}
			hooks.AddBeforeCallTool(func(context.Context, any, *mcp.CallToolRequest) {})
			hooks.AddAfterCallTool(func(_ context.Context, _ any, r *mcp.CallToolRequest, _ any) { seen.see("after hook", r) })
			hooks.AddOnError(func(_ context.Context, _ any, method mcp.MCPMethod, message any, _ error) {
				if r, ok := message.(*mcp.CallToolRequest); ok && method == mcp.MethodToolsCall {
					seen.see("error hook", r)
				}
			})
			srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation(), WithHooks(hooks))
			srv.AddTools(ServerTool{
				Tool: mcp.NewToolWithRawSchema("echo", "", json.RawMessage(`{"type":["array","number","string"]}`)),
				Handler: func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					seen.see("handler", &r)
					if fail {
						return nil, errors.New("refused")
					}
					return mcp.NewToolResultText("ok"), nil
				},
				ArgumentsOnDemand: true,
			})
			onDemandCall(t, srv, arguments)
			reader := "after hook"
			if fail {
				reader = "error hook"
			}
			for _, r := range []string{"handler", reader} {
				require.Contains(t, seen.argumentsNil, r, "%s did not run: arguments %s, fail %v", r, arguments, fail)
				assert.True(t, seen.argumentsNil[r], "%s: Params.Arguments nil, arguments %s, fail %v", r, arguments, fail)
			}
		}
	}
}

// The same on the task path, which looks the tool up again: a call decoded
// for an opted-in task tool reaches, as a task, the tool that replaced it.
func TestArgumentsOnDemandTaskToolSwappedDuringTheCall(t *testing.T) {
	seen := make(chan bool, 1)
	plain := ServerTool{
		Tool: mcp.NewTool("echo", mcp.WithString("region"), mcp.WithTaskSupport(mcp.TaskSupportOptional)),
		Handler: func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			seen <- r.Params.Arguments == nil
			return mcp.NewToolResultText(r.GetString("region", "none")), nil
		},
	}
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithTaskCapabilities(true, true, true))
	onDemand := plain
	onDemand.ArgumentsOnDemand = true
	srv.AddTools(onDemand)
	var request mcp.CallToolRequest
	require.NoError(t, srv.decodeToolCall(json.RawMessage(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"region":"r"},"task":{}}}`), &request))
	require.Nil(t, request.Params.Arguments, "the call was decoded for the opted-in tool")
	srv.SetTools(plain)
	created, reqErr := srv.handleTaskAugmentedToolCall(t.Context(), 1, request)
	require.Nil(t, reqErr)
	assert.False(t, <-seen, "Params.Arguments nil in the handler of a tool that did not opt in")
	result, reqErr := srv.handleTaskResult(t.Context(), 2, mcp.TaskResultRequest{Params: mcp.TaskResultParams{TaskId: created.Task.TaskId}})
	require.Nil(t, reqErr)
	assert.Equal(t, "r", result.Content[0].(mcp.TextContent).Text)
}

// complexTypedCall is a typed tool of bench/compare's complex shape and a call
// to it.
func complexTypedCall(t testing.TB, onDemand bool) (*MCPServer, json.RawMessage) {
	t.Helper()
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	srv.AddTools(ServerTool{
		Tool: mcp.NewTool("complex"),
		Handler: mcp.NewTypedToolHandler(func(_ context.Context, _ mcp.CallToolRequest, a complexTypedArgs) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(a.Region), nil
		}),
		ArgumentsOnDemand: onDemand,
	})
	return srv, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"complex","arguments":` + string(complexArguments()) + `}}`)
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

// A call decoded at once before its tool opted in, here when the hook before
// the call registers the tool again with ArgumentsOnDemand, keeps the
// arguments it was decoded with: the handler and the hooks after the call, on
// success and on error, see Params.Arguments filled, not nil.
func TestArgumentsOnDemandToolOptedInDuringTheCall(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("fail ", fail), func(t *testing.T) {
			var srv *MCPServer
			seen := newOnDemandSeen()
			plain := ServerTool{
				Tool: mcp.NewTool("echo", mcp.WithString("region")),
				Handler: func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					seen.see("handler", &r)
					if fail {
						return nil, errors.New("refused")
					}
					return mcp.NewToolResultText("ok"), nil
				},
			}
			onDemand := plain
			onDemand.ArgumentsOnDemand = true
			hooks := &Hooks{}
			hooks.AddBeforeCallTool(func(context.Context, any, *mcp.CallToolRequest) { srv.SetTools(onDemand) })
			hooks.AddAfterCallTool(func(_ context.Context, _ any, r *mcp.CallToolRequest, _ any) { seen.see("after hook", r) })
			hooks.AddOnError(func(_ context.Context, _ any, method mcp.MCPMethod, message any, _ error) {
				if r, ok := message.(*mcp.CallToolRequest); ok && method == mcp.MethodToolsCall {
					seen.see("error hook", r)
				}
			})
			srv = NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithHooks(hooks))
			srv.AddTools(plain)
			onDemandCall(t, srv, `{"region":"r"}`)
			require.True(t, srv.toolReadsArgumentsOnDemand("echo"), "the tool did not opt in during the call")
			reader := "after hook"
			if fail {
				reader = "error hook"
			}
			for _, r := range []string{"handler", reader} {
				require.Contains(t, seen.argumentsNil, r, "%s did not run", r)
				assert.False(t, seen.argumentsNil[r], "%s: Params.Arguments nil", r)
				assert.Equal(t, "r", seen.region[r], "%s: GetString", r)
			}
		})
	}
}
