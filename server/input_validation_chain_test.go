package server

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// chainKey is the key under which recordingMiddleware hands its value down.
type chainKey struct{}

// chainRuns records what recordingMiddleware saw of the calls it wrapped.
type chainRuns struct {
	mu sync.Mutex
	// sent are the arguments of each call as the middleware was handed it.
	sent    []map[string]any
	results []*mcp.CallToolResult
	errs    []error
}

func (r *chainRuns) record(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f()
}

// recordingMiddleware is a tool handler middleware that records each call it
// is handed and what the rest of the chain answered it, and hands the chain
// "middleware" under chainKey in the context.
func recordingMiddleware(runs *chainRuns) ServerOption {
	return WithToolHandlerMiddleware(func(next ToolHandlerFunc) ToolHandlerFunc {
		return func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			runs.record(func() { runs.sent = append(runs.sent, r.GetArguments()) })
			result, err := next(context.WithValue(ctx, chainKey{}, "middleware"), r)
			runs.record(func() {
				runs.results = append(runs.results, result)
				runs.errs = append(runs.errs, err)
			})
			return result, err
		}
	})
}

// rewritingMiddleware is a tool handler middleware that hands the chain the
// call with its Arguments replaced by arguments.
func rewritingMiddleware(arguments map[string]any) ServerOption {
	return WithToolHandlerMiddleware(func(next ToolHandlerFunc) ToolHandlerFunc {
		return func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			r.Params.Arguments = arguments
			return next(ctx, r)
		}
	})
}

// With validation inside the middleware, a middleware sees a refused call as
// it sees any other: it is handed the call, and the refusal, the default one
// or the input validation handler's, is what the chain answers it.
func TestInputValidationInsideMiddlewareMiddlewareSeesRefusal(t *testing.T) {
	for name, answer := range map[string]InputValidationHandler{
		"default refusal": nil,
		"hook's refusal":  refuseWith(mcp.NewToolResultText("mine")),
	} {
		t.Run(name, func(t *testing.T) {
			chain := &chainRuns{}
			srv, runs := hookServer(t, hookSetup{answer: answer, opts: []ServerOption{WithInputValidationInsideMiddleware(), recordingMiddleware(chain)}})
			text, isError := hookAnswer(t, hookCall(t, srv, `{"n":11}`, false))
			assert.True(t, isError, "the refusal arrived without isError")
			if answer == nil {
				assert.True(t, strings.HasPrefix(text, "input schema validation failed: "), text)
			} else {
				assert.Equal(t, "mine", text)
			}
			_, handlers := runs.counts()
			assert.Zero(t, handlers, "the handler ran on a refusal")

			chain.mu.Lock()
			defer chain.mu.Unlock()
			require.Len(t, chain.results, 1, "the middleware did not see the refused call")
			assert.Equal(t, map[string]any{"n": float64(11)}, chain.sent[0])
			assert.NoError(t, chain.errs[0])
			require.NotNil(t, chain.results[0])
			assert.True(t, chain.results[0].IsError)
			require.NotEmpty(t, chain.results[0].Content)
			assert.Equal(t, text, chain.results[0].Content[0].(mcp.TextContent).Text)
		})
	}
}

// The input validation handler gets the context the chain hands the tool
// handler: a value a middleware put in it is there.
func TestInputValidationInsideMiddlewareHookSeesChainContext(t *testing.T) {
	var mu sync.Mutex
	var hookCtx, handlerCtx context.Context
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false),
		WithInputValidationInsideMiddleware(),
		WithInputValidationHandler(func(ctx context.Context, _ mcp.Tool, _ CallArguments, _ InputValidationFailure) (*mcp.CallToolResult, bool) {
			mu.Lock()
			defer mu.Unlock()
			hookCtx = ctx
			return nil, false
		}),
		recordingMiddleware(&chainRuns{}),
	)
	srv.AddTool(mcp.NewToolWithRawSchema("t", "", json.RawMessage(hookSchema)), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		mu.Lock()
		defer mu.Unlock()
		handlerCtx = ctx
		return mcp.NewToolResultText("ran"), nil
	})
	text, isError := hookAnswer(t, hookCall(t, srv, `{"n":11}`, false))
	assert.Equal(t, "ran", text)
	assert.False(t, isError)

	mu.Lock()
	defer mu.Unlock()
	require.NotNil(t, hookCtx, "the hook was not called")
	assert.Equal(t, "middleware", hookCtx.Value(chainKey{}), "the middleware's value is missing from the hook's context")
	assert.True(t, hookCtx == handlerCtx, "the hook's context is not the one the handler got")
}

// Validation reads the call the chain hands it: a middleware that replaces
// the arguments has its own validated, not the client's, whichever way the
// client's numbers were written and however the tool reads its arguments.
func TestInputValidationInsideMiddlewareValidatesTheRewrite(t *testing.T) {
	for _, leg := range []struct {
		name     string
		sent     string
		rewrite  map[string]any
		onDemand bool
		refused  bool
	}{
		{name: "to invalid", sent: `{"n":1}`, rewrite: map[string]any{"n": 11}, refused: true},
		{name: "to invalid, an exponent sent", sent: `{"n":1e0}`, rewrite: map[string]any{"n": 11}, refused: true},
		{name: "to invalid, on demand", sent: `{"n":1}`, rewrite: map[string]any{"n": 11}, onDemand: true, refused: true},
		{name: "to valid", sent: `{"n":11}`, rewrite: map[string]any{"n": 1}},
		{name: "to valid, an exponent sent", sent: `{"n":11e0}`, rewrite: map[string]any{"n": 1}},
	} {
		t.Run(leg.name, func(t *testing.T) {
			srv, runs := hookServer(t, hookSetup{
				answer:   refuseWith(nil),
				onDemand: leg.onDemand,
				opts:     []ServerOption{WithInputValidationInsideMiddleware(), rewritingMiddleware(leg.rewrite)},
			})
			text, isError := hookAnswer(t, hookCall(t, srv, leg.sent, false))
			hooks, handlers := runs.counts()
			if !leg.refused {
				assert.Equal(t, "ran", text)
				assert.False(t, isError)
				assert.Zero(t, hooks, "the rewrite was refused")
				assert.Equal(t, 1, handlers)
				return
			}
			assert.True(t, isError, "the rewrite passed: %s", text)
			assert.Zero(t, handlers, "the handler ran on a refusal")
			require.Equal(t, 1, hooks)
			runs.mu.Lock()
			defer runs.mu.Unlock()
			// The rewrite is read as a round trip, as under a hook before
			// the call: its number arrives as a json.Number.
			assert.Equal(t, map[string]any{"n": json.Number("11")}, runs.args[0].Effective)
			assert.Equal(t, json.RawMessage(leg.sent), runs.args[0].Raw)
			require.Len(t, runs.failures[0].Violations, 1)
			assert.Equal(t, "maximum", runs.failures[0].Violations[0].Keyword)
		})
	}
}

// A middleware that answers without calling the rest of the chain answers
// the call: nothing is validated, so the input validation handler is not
// called, and the handler does not run.
func TestInputValidationInsideMiddlewareShortCircuitSkips(t *testing.T) {
	srv, runs := hookServer(t, hookSetup{answer: refuseWith(nil), opts: []ServerOption{
		WithInputValidationInsideMiddleware(),
		WithToolHandlerMiddleware(func(ToolHandlerFunc) ToolHandlerFunc {
			return func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultText("answered"), nil
			}
		}),
	}})
	text, isError := hookAnswer(t, hookCall(t, srv, `{"n":11}`, false))
	assert.Equal(t, "answered", text)
	assert.False(t, isError)
	hooks, handlers := runs.counts()
	assert.Zero(t, hooks, "the hook was called for a call the middleware answered")
	assert.Zero(t, handlers)
}

// A fault of the input validation handler reaches the middleware as an
// error, and the client as an internal error without its detail, which the
// error hook sees.
func TestInputValidationInsideMiddlewareFaultIsInternalError(t *testing.T) {
	for name, leg := range map[string]struct {
		answer InputValidationHandler
		detail string
	}{
		"panic": {answer: func(context.Context, mcp.Tool, CallArguments, InputValidationFailure) (*mcp.CallToolResult, bool) {
			panic("boom")
		}, detail: "boom"},
		"forbidden result": {answer: refuseWith(&mcp.CallToolResult{Task: &mcp.ExtensionTask{}}), detail: "Task"},
	} {
		t.Run(name, func(t *testing.T) {
			chain := &chainRuns{}
			srv, runs := hookServer(t, hookSetup{answer: leg.answer, opts: []ServerOption{WithInputValidationInsideMiddleware(), recordingMiddleware(chain)}})
			out := hookCall(t, srv, `{"n":11}`, false)
			assert.Contains(t, out, `"code":-32603`)
			assert.NotContains(t, out, leg.detail)
			assert.NotContains(t, out, `"isError"`)

			chain.mu.Lock()
			require.Len(t, chain.errs, 1, "the middleware did not see the call")
			assert.ErrorContains(t, chain.errs[0], leg.detail)
			assert.Nil(t, chain.results[0])
			chain.mu.Unlock()

			runs.mu.Lock()
			defer runs.mu.Unlock()
			assert.Empty(t, runs.handled)
			require.Len(t, runs.errs, 1, "the error hook did not see the fault")
			assert.ErrorContains(t, runs.errs[0], leg.detail)
		})
	}
}

// Without WithInputValidationInsideMiddleware a refused call is answered
// before the middleware, which never sees it, and the input validation
// handler gets the request's context; a passing call reaches the middleware.
func TestInputValidationDefaultPlacementUnchanged(t *testing.T) {
	var mu sync.Mutex
	var hookCtx context.Context
	chain := &chainRuns{}
	srv, runs := hookServer(t, hookSetup{
		answer: func(ctx context.Context, _ mcp.Tool, _ CallArguments, _ InputValidationFailure) (*mcp.CallToolResult, bool) {
			mu.Lock()
			defer mu.Unlock()
			hookCtx = ctx
			return nil, true
		},
		opts: []ServerOption{recordingMiddleware(chain)},
	})
	text, isError := hookAnswer(t, hookCall(t, srv, `{"n":11}`, false))
	assert.True(t, isError)
	assert.True(t, strings.HasPrefix(text, "input schema validation failed: "), text)
	hooks, handlers := runs.counts()
	assert.Equal(t, 1, hooks)
	assert.Zero(t, handlers)
	chain.mu.Lock()
	assert.Empty(t, chain.sent, "the middleware saw a refused call")
	chain.mu.Unlock()
	mu.Lock()
	require.NotNil(t, hookCtx)
	assert.Nil(t, hookCtx.Value(chainKey{}), "the hook got the middleware's context")
	mu.Unlock()

	text, isError = hookAnswer(t, hookCall(t, srv, `{"n":1}`, false))
	assert.Equal(t, "ran", text)
	assert.False(t, isError)
	chain.mu.Lock()
	defer chain.mu.Unlock()
	assert.Len(t, chain.sent, 1, "the middleware did not see a passing call")
}

// The option leaves the 2025-11-25 task path as it is: a task with
// arguments that fail is refused before the task is created, so it takes no
// concurrency slot, and the tool middleware never sees it.
func TestInputValidationInsideMiddlewareLegacyTaskUnchanged(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	chain := &chainRuns{}
	srv, runs := hookServer(t, hookSetup{
		answer: refuseWith(mcp.NewToolResultText("mine")),
		block:  block,
		opts:   []ServerOption{WithInputValidationInsideMiddleware(), recordingMiddleware(chain), WithMaxConcurrentTasks(1)},
	})
	call := func(n any) mcp.CallToolRequest {
		return mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "t", Arguments: map[string]any{"n": n}, Task: &mcp.TaskParams{}}}
	}
	_, reqErr := srv.handleTaskAugmentedToolCall(t.Context(), 1, call(1))
	require.Nil(t, reqErr)

	created, reqErr := srv.handleTaskAugmentedToolCall(t.Context(), 2, call(11))
	require.Nil(t, reqErr, "the refused call was refused for the concurrency limit")
	result, reqErr := srv.handleTaskResult(t.Context(), 3, mcp.TaskResultRequest{Params: mcp.TaskResultParams{TaskId: created.Task.TaskId}})
	require.Nil(t, reqErr)
	assert.True(t, result.IsError)
	require.NotEmpty(t, result.Content)
	assert.Equal(t, "mine", result.Content[0].(mcp.TextContent).Text)
	hooks, _ := runs.counts()
	assert.Equal(t, 1, hooks)
	chain.mu.Lock()
	defer chain.mu.Unlock()
	for _, sent := range chain.sent {
		assert.NotEqual(t, 11, sent["n"], "the middleware saw the refused task")
	}
}

// Validation inside the middleware costs a passing validated call at most
// one allocation, the link it adds to the chain, with an input validation
// handler as without, whether the arguments are an object, an array or a
// scalar: none of them is taken for a middleware's replacement and decoded
// again. A server without the option builds no link, so the chain's
// innermost handler is the tool's own.
func TestInputValidationInsideMiddlewarePassingCallAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	handler := func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	}
	for _, leg := range []struct{ name, schema, args string }{
		{"object", `{"type":"object","properties":{"limit":{"type":"integer","maximum":9007199254740992}}}`, `{"limit":5}`},
		{"array", `{"type":"array","items":{"type":"integer","maximum":10}}`, `[1,2]`},
		{"string", `{"type":"string","maxLength":4}`, `"x"`},
		{"number", `{"type":"integer","maximum":10}`, `5`},
		{"boolean", `{"type":"boolean"}`, `true`},
	} {
		t.Run(leg.name, func(t *testing.T) {
			msg := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"limited","arguments":` + leg.args + `}}`)
			measure := func(opts ...ServerOption) (allocs float64, innermostIsHandler bool) {
				innermost := false
				srv := NewMCPServer("test", "1.0.0", append([]ServerOption{
					WithToolCapabilities(false),
					WithToolHandlerMiddleware(func(next ToolHandlerFunc) ToolHandlerFunc {
						innermost = reflect.ValueOf(next).Pointer() == reflect.ValueOf(handler).Pointer()
						return next
					}),
				}, opts...)...)
				srv.AddTool(mcp.NewToolWithRawSchema("limited", "", json.RawMessage(leg.schema)), handler)
				ctx := t.Context()
				out, err := json.Marshal(srv.HandleMessage(ctx, msg))
				require.NoError(t, err)
				require.Contains(t, string(out), `"text":"ok"`)
				innermostIsHandler = innermost
				return testing.AllocsPerRun(1000, func() { srv.HandleMessage(ctx, msg) }), innermostIsHandler
			}
			none, noneDirect := measure()
			plain, plainDirect := measure(WithInputSchemaValidation())
			inside, insideDirect := measure(WithInputValidationInsideMiddleware())
			hooked, _ := measure(WithInputValidationInsideMiddleware(), WithInputValidationHandler(refuseWith(nil)))
			t.Logf("allocations per passing tools/call: %v unvalidated, %v validated before the middleware, %v inside, %v inside with a hook", none, plain, inside, hooked)

			assert.True(t, noneDirect, "an unvalidated server built a validation link")
			assert.True(t, plainDirect, "a server validating before the middleware built a validation link")
			assert.False(t, insideDirect, "the option built no validation link")
			assert.LessOrEqual(t, inside, plain+1, "validation inside the middleware costs a passing call more than one allocation")
			assert.Equal(t, inside, hooked, "a hook costs a passing call")
		})
	}
}

// inPlaceMiddleware is a tool handler middleware that sets n to value in the
// call's own map, changing it in place.
func inPlaceMiddleware(value any) ServerOption {
	return WithToolHandlerMiddleware(func(next ToolHandlerFunc) ToolHandlerFunc {
		return func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if m := r.GetArguments(); m != nil {
				m["n"] = value
			}
			return next(ctx, r)
		}
	})
}

// A middleware that hands down a new map has it validated, however the
// client wrote its numbers. One that changes the decoded map in place has
// the change validated only when every number the client sent was exact;
// when one was not, written with an exponent or 16 digits, the bytes the
// client sent are validated, and the change reaches the handler unchecked.
func TestInputValidationInsideMiddlewareInPlaceChange(t *testing.T) {
	for _, leg := range []struct {
		name    string
		sent    string
		set     float64
		inPlace bool
		ran     bool
	}{
		{name: "in place to invalid, exact", sent: `{"n":1}`, set: 11, inPlace: true},
		{name: "in place to invalid, an exponent sent", sent: `{"n":1e0}`, set: 11, inPlace: true, ran: true},
		{name: "in place to invalid, 16 digits sent", sent: `{"n":1.000000000000000}`, set: 11, inPlace: true, ran: true},
		{name: "in place to valid, an exponent sent", sent: `{"n":11e0}`, set: 1, inPlace: true},
		{name: "new map to invalid, exact", sent: `{"n":1}`, set: 11},
		{name: "new map to invalid, an exponent sent", sent: `{"n":1e0}`, set: 11},
		{name: "new map to valid, an exponent sent", sent: `{"n":11e0}`, set: 1, ran: true},
	} {
		t.Run(leg.name, func(t *testing.T) {
			change := rewritingMiddleware(map[string]any{"n": leg.set})
			if leg.inPlace {
				change = inPlaceMiddleware(leg.set)
			}
			srv, runs := hookServer(t, hookSetup{opts: []ServerOption{WithInputValidationInsideMiddleware(), change}})
			text, isError := hookAnswer(t, hookCall(t, srv, leg.sent, false))
			_, handlers := runs.counts()
			if !leg.ran {
				assert.True(t, isError, "the call ran: %s", text)
				assert.Zero(t, handlers)
				return
			}
			assert.Equal(t, "ran", text)
			require.Equal(t, 1, handlers)
			runs.mu.Lock()
			defer runs.mu.Unlock()
			assert.Equal(t, leg.set, runs.handled[0].GetFloat("n", 0), "the handler did not get the middleware's value")
		})
	}
}

// Under the option a hook before the call still has the arguments it left
// validated, however the client wrote its numbers.
func TestInputValidationInsideMiddlewareBeforeHookRewrite(t *testing.T) {
	for _, sent := range []string{`{"n":1}`, `{"n":1e0}`} {
		t.Run(sent, func(t *testing.T) {
			srv, runs := hookServer(t, hookSetup{
				before: func(r *mcp.CallToolRequest) { r.Params.Arguments = map[string]any{"n": float64(11)} },
				opts:   []ServerOption{WithInputValidationInsideMiddleware()},
			})
			text, isError := hookAnswer(t, hookCall(t, srv, sent, false))
			assert.True(t, isError, "the hook's arguments passed: %s", text)
			_, handlers := runs.counts()
			assert.Zero(t, handlers)
		})
	}
}

// A call whose arguments a middleware replaced is validated as a round trip
// and marked as one, as under a hook before the call: the strict integer
// getter then refuses a number the round trip may have rounded, here a
// 16-digit literal that the schema's maximum of 2^53 would refuse unrounded.
func TestInputValidationInsideMiddlewareMarksTheRewrite(t *testing.T) {
	var got error
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false),
		WithInputValidationInsideMiddleware(),
		WithToolHandlerMiddleware(func(next ToolHandlerFunc) ToolHandlerFunc {
			return func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				clone := map[string]any{"added": true}
				maps.Copy(clone, r.GetArguments())
				r.Params.Arguments = clone
				return next(ctx, r)
			}
		}))
	srv.AddTool(mcp.NewToolWithRawSchema("w", "", json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer","maximum":9007199254740992}}}`)),
		func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			_, got = r.RequireWholeInt("n")
			return mcp.NewToolResultText("ran"), nil
		})
	out, err := json.Marshal(srv.HandleMessage(t.Context(), json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"w","arguments":{"n":9007199254740993}}}`)))
	require.NoError(t, err)
	require.Contains(t, string(out), `"text":"ran"`)
	assert.ErrorIs(t, got, mcp.ErrArgumentInexact, "the handler read the rounded number as exact")
}

// A middleware that replaces only RawArguments has the Arguments it left
// validated, as a round trip: the bytes it handed down are not, and
// BindArguments reads them unchecked, as under a hook before the call.
func TestInputValidationInsideMiddlewareRawReplacement(t *testing.T) {
	srv, runs := hookServer(t, hookSetup{answer: refuseWith(nil), opts: []ServerOption{
		WithInputValidationInsideMiddleware(),
		WithToolHandlerMiddleware(func(next ToolHandlerFunc) ToolHandlerFunc {
			return func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				r.Params.RawArguments = json.RawMessage(`{"n":11e0}`)
				return next(ctx, r)
			}
		}),
	}})
	text, isError := hookAnswer(t, hookCall(t, srv, `{"n":1}`, false))
	assert.Equal(t, "ran", text)
	assert.False(t, isError)
	hooks, handlers := runs.counts()
	assert.Zero(t, hooks, "the replaced bytes were validated")
	require.Equal(t, 1, handlers)
	runs.mu.Lock()
	defer runs.mu.Unlock()
	var bound struct{ N float64 }
	require.NoError(t, runs.handled[0].BindArguments(&bound))
	assert.Equal(t, float64(11), bound.N, "BindArguments did not read the replaced bytes")
}

// Without a middleware the client gets the same bytes whichever way
// validation is placed: refusals, limited ones, refusals beside an output
// schema, a hook that proceeds, faults, the cap on values and null.
func TestInputValidationPlacementsAnswerAlike(t *testing.T) {
	big := mcp.NewToolResultText(strings.Repeat("x", 4096))
	for name, leg := range map[string]struct {
		answer InputValidationHandler
		opts   []ServerOption
		args   string
	}{
		"default refusal":                        {args: `{"n":11}`},
		"default refusal, output schema":         {args: `{"n":11}`, opts: []ServerOption{WithOutputSchemaValidation()}},
		"hook refusal":                           {answer: refuseWith(mcp.NewToolResultText("mine")), args: `{"n":11}`},
		"hook refusal, limited":                  {answer: refuseWith(big), args: `{"n":11}`, opts: []ServerOption{WithToolResultSizeLimit(256)}},
		"default refusal, limited":               {args: `{"n":11,"pad":"` + strings.Repeat("y", 2048) + `"}`, opts: []ServerOption{WithToolResultSizeLimit(64)}},
		"hook structured refusal, output schema": {answer: refuseWith(mcp.NewToolResultStructured(map[string]any{"ok": "nope"}, "nope")), args: `{"n":11}`, opts: []ServerOption{WithOutputSchemaValidation()}},
		"hook proceeds, output schema":           {answer: proceed, args: `{"n":11}`, opts: []ServerOption{WithOutputSchemaValidation()}},
		"hook panic": {answer: func(context.Context, mcp.Tool, CallArguments, InputValidationFailure) (*mcp.CallToolResult, bool) {
			panic("boom")
		}, args: `{"n":11}`},
		"hook forbidden result": {answer: refuseWith(&mcp.CallToolResult{RequestState: "x"}), args: `{"n":11}`},
		"too many values":       {args: `{"n":[1,2,3,4,5]}`, opts: []ServerOption{WithMaxValidatedValues(2)}},
		"null":                  {args: `null`},
	} {
		t.Run(name, func(t *testing.T) {
			var outs []string
			for _, inside := range []bool{false, true} {
				opts := append([]ServerOption{}, leg.opts...)
				if inside {
					opts = append(opts, WithInputValidationInsideMiddleware())
				}
				srv, _ := hookServer(t, hookSetup{answer: leg.answer, opts: opts})
				outs = append(outs, hookCall(t, srv, leg.args, false))
			}
			assert.Equal(t, outs[0], outs[1], "the answer differs inside the middleware")
			assert.NotContains(t, outs[0], `"text":"ran"`, "the leg reached the handler")
		})
	}
}
