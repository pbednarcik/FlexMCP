package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// hookSchema wants n, an integer of at most 10.
const hookSchema = `{"type":"object","properties":{"n":{"type":"integer","maximum":10}},"required":["n"]}`

// hookSetup is a server of hookServer's.
type hookSetup struct {
	// answer answers the failures; nil sets no input validation handler, only
	// input validation.
	answer InputValidationHandler
	// nilAnswer, with answer nil, turns validation on through
	// WithInputValidationHandler(nil).
	nilAnswer bool
	// onDemand registers the tool with ArgumentsOnDemand.
	onDemand bool
	// before, when set, is a hook before the call.
	before func(*mcp.CallToolRequest)
	// block, when set, holds the tool's handler until it is closed.
	block chan struct{}
	opts  []ServerOption
}

// hookRuns records what the input validation handler, the tool's handler and
// the error hook saw.
type hookRuns struct {
	mu       sync.Mutex
	args     []CallArguments
	failures []InputValidationFailure
	handled  []mcp.CallToolRequest
	errs     []error
}

func (r *hookRuns) record(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f()
}

func (r *hookRuns) counts() (hooks, handlers int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.failures), len(r.handled)
}

// hookTool is the tool "t" of hookServer: it takes task augmentation, wants
// hookSchema and declares an output schema its handler's result fails.
func hookTool() mcp.Tool {
	tool := mcp.NewToolWithRawSchema("t", "", json.RawMessage(hookSchema))
	tool.Execution = &mcp.ToolExecution{TaskSupport: mcp.TaskSupportOptional}
	tool.RawOutputSchema = json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)
	return tool
}

// hookServer serves hookTool as setup says, and records what is seen of its
// calls.
func hookServer(t testing.TB, setup hookSetup) (*MCPServer, *hookRuns) {
	t.Helper()
	runs := &hookRuns{}
	hooks := &Hooks{}
	hooks.AddOnError(func(_ context.Context, _ any, method mcp.MCPMethod, _ any, err error) {
		if method == mcp.MethodToolsCall {
			runs.record(func() { runs.errs = append(runs.errs, err) })
		}
	})
	if setup.before != nil {
		hooks.AddBeforeCallTool(func(_ context.Context, _ any, r *mcp.CallToolRequest) { setup.before(r) })
	}
	opts := []ServerOption{WithToolCapabilities(false), WithTaskCapabilities(true, true, true), WithHooks(hooks)}
	if setup.answer != nil {
		opts = append(opts, WithInputValidationHandler(func(ctx context.Context, tool mcp.Tool, args CallArguments, failure InputValidationFailure) (*mcp.CallToolResult, bool) {
			runs.record(func() {
				runs.args = append(runs.args, args)
				runs.failures = append(runs.failures, failure)
			})
			return setup.answer(ctx, tool, args, failure)
		}))
	} else if setup.nilAnswer {
		opts = append(opts, WithInputValidationHandler(nil))
	} else {
		opts = append(opts, WithInputSchemaValidation())
	}
	srv := NewMCPServer("test", "1.0.0", append(opts, setup.opts...)...)
	srv.AddTools(ServerTool{
		Tool: hookTool(),
		Handler: func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			runs.record(func() { runs.handled = append(runs.handled, r) })
			if setup.block != nil {
				<-setup.block
			}
			return mcp.NewToolResultStructured(map[string]any{"ok": "ran"}, "ran"), nil
		},
		ArgumentsOnDemand: setup.onDemand,
	})
	return srv, runs
}

// hookCall calls "t" with arguments, JSON text, or with none when arguments
// is "", as a legacy task when task is set. It returns the answer: the
// JSON-RPC response, or the tasks/result of a task the call created.
func hookCall(t testing.TB, srv *MCPServer, arguments string, task bool) string {
	t.Helper()
	out, err := callForAnswer(t.Context(), srv, arguments, task)
	require.NoError(t, err)
	return out
}

// callForAnswer is hookCall, its failures returned, for a goroutine other
// than the test's.
func callForAnswer(ctx context.Context, srv *MCPServer, arguments string, task bool) (string, error) {
	params := `{"name":"t"`
	if arguments != "" {
		params += `,"arguments":` + arguments
	}
	if task {
		params += `,"task":{}`
	}
	out, err := json.Marshal(srv.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+params+`}}`)))
	if err != nil || !task {
		return string(out), err
	}
	var created struct {
		Result struct {
			Task struct {
				TaskID string `json:"taskId"`
			} `json:"task"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &created); err != nil || created.Result.Task.TaskID == "" {
		return string(out), err
	}
	result, reqErr := srv.handleTaskResult(ctx, 2, mcp.TaskResultRequest{Params: mcp.TaskResultParams{TaskId: created.Result.Task.TaskID}})
	if reqErr != nil {
		return "", reqErr.err
	}
	got, err := json.Marshal(result)
	return string(got), err
}

// hookAnswer reads the tool result of a hookCall answer: its first text and
// whether it is an error.
func hookAnswer(t testing.TB, out string) (text string, isError bool) {
	t.Helper()
	text, isError, err := toolAnswer(out)
	require.NoError(t, err)
	return text, isError
}

// toolAnswer is hookAnswer, its failure returned.
func toolAnswer(out string) (text string, isError bool, err error) {
	var answer struct {
		Result *struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal([]byte(out), &answer); err != nil {
		return "", false, err
	}
	content, isError := answer.Content, answer.IsError
	if answer.Result != nil {
		content, isError = answer.Result.Content, answer.Result.IsError
	}
	if len(content) == 0 {
		return "", false, fmt.Errorf("no tool result: %s", out)
	}
	return content[0].Text, isError, nil
}

// tasksOf counts the tasks srv holds.
func tasksOf(srv *MCPServer) int {
	srv.tasksMu.RLock()
	defer srv.tasksMu.RUnlock()
	return len(srv.tasks)
}

// bothSites runs f on the synchronous site and on the legacy task site.
func bothSites(t *testing.T, f func(t *testing.T, task bool)) {
	for _, task := range []bool{false, true} {
		name := "synchronous"
		if task {
			name = "legacy task"
		}
		t.Run(name, func(t *testing.T) { f(t, task) })
	}
}

func refuseWith(result *mcp.CallToolResult) InputValidationHandler {
	return func(context.Context, mcp.Tool, CallArguments, InputValidationFailure) (*mcp.CallToolResult, bool) {
		return result, true
	}
}

func proceed(context.Context, mcp.Tool, CallArguments, InputValidationFailure) (*mcp.CallToolResult, bool) {
	return nil, false
}

// A refusal is a tool execution error whatever the hook set, and the hook's
// own result is left as it returned it.
func TestInputValidationHandlerRefusalIsAnError(t *testing.T) {
	bothSites(t, func(t *testing.T, task bool) {
		mine := mcp.NewToolResultText("mine")
		srv, runs := hookServer(t, hookSetup{answer: refuseWith(mine)})
		text, isError := hookAnswer(t, hookCall(t, srv, `{"n":11}`, task))
		assert.Equal(t, "mine", text)
		assert.True(t, isError, "the refusal arrived without isError")
		assert.False(t, mine.IsError, "the hook's result was written to")
		hooks, handlers := runs.counts()
		assert.Equal(t, 1, hooks)
		assert.Zero(t, handlers, "the handler ran on a refusal")
	})
}

// A refusal with no result answers the default text, byte for byte as a
// server without the hook does, and so does a nil hook, which turns
// validation on.
func TestInputValidationHandlerRefusesWithTheDefaultText(t *testing.T) {
	bothSites(t, func(t *testing.T, task bool) {
		plain, _ := hookServer(t, hookSetup{})
		for name, setup := range map[string]hookSetup{
			"a nil result": {answer: refuseWith(nil)},
			"a nil hook":   {nilAnswer: true},
		} {
			hooked, runs := hookServer(t, setup)
			for _, args := range []string{`{"n":11}`, `{"n":"x"}`, `[]`, `null`, ``} {
				got, want := hookCall(t, hooked, args, task), hookCall(t, plain, args, task)
				if !task {
					assert.Equal(t, want, got, "%s, arguments %s", name, args)
				}
				gotText, gotError := hookAnswer(t, got)
				wantText, wantError := hookAnswer(t, want)
				assert.Equal(t, wantText, gotText, "%s, arguments %s", name, args)
				assert.True(t, gotError && wantError, "%s, arguments %s", name, args)
				assert.True(t, strings.HasPrefix(gotText, "input schema validation failed: "), gotText)
			}
			_, handlers := runs.counts()
			assert.Zero(t, handlers, "%s: the handler ran", name)
		}
	})
}

// A refusal larger than the result size limit is replaced, as the default
// refusal is.
func TestInputValidationHandlerRefusalIsLimited(t *testing.T) {
	bothSites(t, func(t *testing.T, task bool) {
		srv, _ := hookServer(t, hookSetup{
			answer: refuseWith(mcp.NewToolResultText(strings.Repeat("x", 1000))),
			opts:   []ServerOption{WithToolResultSizeLimit(200)},
		})
		text, isError := hookAnswer(t, hookCall(t, srv, `{"n":11}`, task))
		assert.Equal(t, "tool result exceeds 200 bytes", text)
		assert.True(t, isError)
	})
}

// A refusal whose result would be written as a task or an input-required
// answer is a fault of the hook: the client gets an internal error without its
// detail, the error hook gets the fault, no task is created and no handler
// runs.
func TestInputValidationHandlerFault(t *testing.T) {
	for name, result := range map[string]*mcp.CallToolResult{
		"Task":          {Task: &mcp.ExtensionTask{}},
		"InputRequests": {MultiRoundTripResult: mcp.MultiRoundTripResult{InputRequests: mcp.InputRequests{"x": {}}}},
		"RequestState":  {MultiRoundTripResult: mcp.MultiRoundTripResult{RequestState: "s"}},
		"ResultType":    {Result: mcp.Result{ResultType: mcp.ResultTypeInputRequired}},
	} {
		t.Run(name, func(t *testing.T) {
			bothSites(t, func(t *testing.T, task bool) {
				srv, runs := hookServer(t, hookSetup{answer: refuseWith(result)})
				out := hookCall(t, srv, `{"n":11}`, task)
				assert.Contains(t, out, `"code":-32603`)
				assert.NotContains(t, out, "input validation handler")
				assert.Zero(t, tasksOf(srv), "a task was created")
				_, handlers := runs.counts()
				assert.Zero(t, handlers)
				runs.mu.Lock()
				defer runs.mu.Unlock()
				require.Len(t, runs.errs, 1, "the error hook did not see the fault")
				assert.ErrorContains(t, runs.errs[0], name)
			})
		})
	}
}

// A panicking hook is recovered without WithRecovery, as an internal error
// the error hook sees and the client gets without its detail.
func TestInputValidationHandlerPanic(t *testing.T) {
	bothSites(t, func(t *testing.T, task bool) {
		srv, runs := hookServer(t, hookSetup{answer: func(context.Context, mcp.Tool, CallArguments, InputValidationFailure) (*mcp.CallToolResult, bool) {
			panic("boom")
		}})
		out := hookCall(t, srv, `{"n":11}`, task)
		assert.Contains(t, out, `"code":-32603`)
		assert.NotContains(t, out, "boom")
		assert.Zero(t, tasksOf(srv), "a task was created")
		runs.mu.Lock()
		defer runs.mu.Unlock()
		require.Len(t, runs.errs, 1, "the error hook did not see the panic")
		assert.ErrorContains(t, runs.errs[0], "boom")
		assert.Empty(t, runs.handled)
	})
}

// A panic in what FlexMCP builds for the hook is recovered as a fault, as a
// panic in the hook is. No validation returns a failure the projection panics
// on, so the test hands answerInvalidInput one: an error tree node without a
// kind.
func TestInputValidationHandlerProjectionPanic(t *testing.T) {
	srv, runs := hookServer(t, hookSetup{answer: refuseWith(nil)})
	compiled, err := compileSchema("t", []byte(hookSchema))
	require.NoError(t, err)
	inv := &inputInvalid{cause: &jsonschema.ValidationError{}, instance: map[string]any{}, schema: &cachedToolSchema{compiled: compiled}, text: "input schema validation failed"}
	var fault error
	require.NotPanics(t, func() {
		_, _, fault = srv.answerInvalidInput(t.Context(), hookTool(), mcp.CallToolRequest{}, inv)
	})
	assert.ErrorContains(t, fault, "panic recovered")
	hooks, _ := runs.counts()
	assert.Zero(t, hooks, "the hook was called")
}

// A proceeded call runs the handler, and its result is still judged against
// the tool's output schema.
func TestInputValidationHandlerProceeds(t *testing.T) {
	bothSites(t, func(t *testing.T, task bool) {
		srv, _ := hookServer(t, hookSetup{answer: proceed})
		text, isError := hookAnswer(t, hookCall(t, srv, `{"n":11}`, task))
		assert.Equal(t, "ran", text)
		assert.False(t, isError)

		srv, _ = hookServer(t, hookSetup{answer: proceed, opts: []ServerOption{WithOutputSchemaValidation()}})
		text, isError = hookAnswer(t, hookCall(t, srv, `{"n":11}`, task))
		assert.True(t, strings.HasPrefix(text, "output schema validation failed: "), text)
		assert.True(t, isError)
	})
}

// A hook before the call that rewrites the arguments rewrites what the
// input validation handler and the tool's handler both see.
func TestInputValidationHandlerSeesTheBeforeHookRewrite(t *testing.T) {
	bothSites(t, func(t *testing.T, task bool) {
		srv, runs := hookServer(t, hookSetup{
			answer: proceed,
			before: func(r *mcp.CallToolRequest) { r.Params.Arguments = map[string]any{"n": "rewritten"} },
		})
		text, _ := hookAnswer(t, hookCall(t, srv, `{"n":1}`, task))
		assert.Equal(t, "ran", text)
		runs.mu.Lock()
		defer runs.mu.Unlock()
		require.Len(t, runs.args, 1)
		assert.Equal(t, map[string]any{"n": "rewritten"}, runs.args[0].Effective)
		assert.JSONEq(t, `{"n":1}`, string(runs.args[0].Raw))
		require.Len(t, runs.failures[0].Violations, 1)
		assert.Equal(t, "/n", runs.failures[0].Violations[0].Pointer)
		assert.Equal(t, "type", runs.failures[0].Violations[0].Keyword)
		require.Len(t, runs.handled, 1)
		assert.Equal(t, "rewritten", runs.handled[0].GetString("n", ""))
	})
}

// Null arguments and none are validated as an empty object, which the hook
// sees as Effective; Raw is what was sent.
func TestInputValidationHandlerNullArguments(t *testing.T) {
	bothSites(t, func(t *testing.T, task bool) {
		for args, raw := range map[string]json.RawMessage{`null`: json.RawMessage(`null`), ``: nil} {
			srv, runs := hookServer(t, hookSetup{answer: refuseWith(nil)})
			_, isError := hookAnswer(t, hookCall(t, srv, args, task))
			assert.True(t, isError)
			runs.mu.Lock()
			require.Len(t, runs.args, 1, "arguments %q", args)
			assert.NotNil(t, runs.args[0].Effective, "arguments %q", args)
			assert.Empty(t, runs.args[0].Effective, "arguments %q", args)
			assert.Equal(t, raw, runs.args[0].Raw, "arguments %q", args)
			require.Len(t, runs.failures[0].Violations, 1)
			assert.Equal(t, "required", runs.failures[0].Violations[0].Keyword)
			runs.mu.Unlock()
		}
	})
}

// Arguments that are no object reach the hook as the root type violation,
// with no Effective and Raw as sent.
func TestInputValidationHandlerArgumentsNotAnObject(t *testing.T) {
	bothSites(t, func(t *testing.T, task bool) {
		srv, runs := hookServer(t, hookSetup{answer: refuseWith(nil)})
		_, isError := hookAnswer(t, hookCall(t, srv, `[]`, task))
		assert.True(t, isError)
		runs.mu.Lock()
		defer runs.mu.Unlock()
		require.Len(t, runs.args, 1)
		assert.Nil(t, runs.args[0].Effective)
		assert.Equal(t, json.RawMessage(`[]`), runs.args[0].Raw)
		require.Len(t, runs.failures[0].Violations, 1)
		v := runs.failures[0].Violations[0]
		assert.Equal(t, "", v.Pointer)
		assert.Equal(t, "type", v.Keyword)
		assert.Equal(t, "array", v.Actual.Kind)
	})
}

// A number the decode refuses answers invalid params before validation, so
// neither the hook nor the handler runs, eagerly and on demand.
func TestInputValidationHandlerNotCalledOnAnUndecodableNumber(t *testing.T) {
	for _, onDemand := range []bool{false, true} {
		t.Run(fmt.Sprintf("on demand %v", onDemand), func(t *testing.T) {
			bothSites(t, func(t *testing.T, task bool) {
				srv, runs := hookServer(t, hookSetup{answer: refuseWith(nil), onDemand: onDemand})
				out := hookCall(t, srv, `{"n":1e400}`, task)
				assert.Contains(t, out, `"code":-32602`)
				hooks, handlers := runs.counts()
				assert.Zero(t, hooks)
				assert.Zero(t, handlers)
			})
		})
	}
}

// A legacy task the hook refuses takes no concurrency slot, and completes
// with the hook's result, which tasks/result returns.
func TestInputValidationHandlerRefusesALegacyTask(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	srv, runs := hookServer(t, hookSetup{
		answer: refuseWith(mcp.NewToolResultText("mine")),
		block:  block,
		opts:   []ServerOption{WithMaxConcurrentTasks(1)},
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
}

// A proceeded call of a tool that reads its arguments on demand reaches the
// handler with no Arguments, and the getters read the client's values, the
// strict integer getter its literal.
func TestInputValidationHandlerProceedsOnDemand(t *testing.T) {
	bothSites(t, func(t *testing.T, task bool) {
		srv, runs := hookServer(t, hookSetup{answer: proceed, onDemand: true})
		text, _ := hookAnswer(t, hookCall(t, srv, `{"n":9007199254740993,"s":"x"}`, task))
		assert.Equal(t, "ran", text)
		runs.mu.Lock()
		defer runs.mu.Unlock()
		require.Len(t, runs.failures, 1)
		assert.Equal(t, "maximum", runs.failures[0].Violations[0].Keyword)
		require.Len(t, runs.handled, 1)
		r := runs.handled[0]
		assert.Nil(t, r.Params.Arguments)
		assert.Equal(t, "x", r.GetString("s", ""))
		n, err := r.RequireWholeInt("n")
		if strconv.IntSize != 64 {
			assert.ErrorIs(t, err, mcp.ErrArgumentOutOfRange)
			return
		}
		require.NoError(t, err)
		assert.Equal(t, int64(9007199254740993), int64(n))
	})
}

// Failing calls in parallel each reach the hook with their own failure and
// arguments, whole, and get their own refusal.
func TestInputValidationHandlerConcurrent(t *testing.T) {
	const workers, calls = 32, 25
	srv, _ := hookServer(t, hookSetup{answer: func(_ context.Context, _ mcp.Tool, args CallArguments, failure InputValidationFailure) (*mcp.CallToolResult, bool) {
		if len(failure.Violations) != 1 {
			return mcp.NewToolResultText(fmt.Sprintf("%d violations", len(failure.Violations))), true
		}
		return mcp.NewToolResultText(fmt.Sprintf("%s|%v|%s", failure.Violations[0].Actual.Value, args.Effective["n"], args.Raw)), true
	}})
	ctx := t.Context()
	var wg sync.WaitGroup
	wrong := make(chan string, workers*calls)
	for w := range workers {
		wg.Go(func() {
			for c := range calls {
				n := 100 + w*calls + c
				out, err := callForAnswer(ctx, srv, fmt.Sprintf(`{"n":%d}`, n), (w+c)%2 == 1)
				text, isError, answerErr := toolAnswer(out)
				if err = cmp.Or(err, answerErr); err != nil {
					wrong <- fmt.Sprintf("call %d: %v", n, err)
				} else if want := fmt.Sprintf(`%d|%d|{"n":%d}`, n, n, n); text != want || !isError {
					wrong <- fmt.Sprintf("call %d: got %q (isError %v), want %q", n, text, isError, want)
				}
			}
		})
	}
	wg.Wait()
	close(wrong)
	for w := range wrong {
		t.Error(w)
	}
}

// A call whose schema cannot be compiled at call time is not validated, so
// the hook is not called and the handler runs.
func TestInputValidationHandlerNotCalledWhenValidationSkips(t *testing.T) {
	bothSites(t, func(t *testing.T, task bool) {
		srv, runs := hookServer(t, hookSetup{answer: refuseWith(nil)})
		srv.toolsMu.Lock()
		schemaJSON, ok := schemaJSONFor(srv.tools["t"].Tool)
		delete(srv.toolInputSchemas, "t")
		srv.toolsMu.Unlock()
		require.True(t, ok)
		srv.inputValidator.keep("t", &cachedToolSchema{digest: schemaDigest(schemaJSON), compileErr: errors.New("cannot compile")})

		text, isError := hookAnswer(t, hookCall(t, srv, `{"n":11}`, task))
		assert.Equal(t, "ran", text)
		assert.False(t, isError)
		hooks, handlers := runs.counts()
		assert.Zero(t, hooks, "the hook was called")
		assert.Equal(t, 1, handlers)
	})
}

// A call that passes validation allocates the same with a hook set as
// without one.
func TestInputValidationHandlerPassingCallAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	msg := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"limited","arguments":{"limit":5}}}`)
	measure := func(opts ...ServerOption) float64 {
		srv := NewMCPServer("test", "1.0.0", append([]ServerOption{WithToolCapabilities(false)}, opts...)...)
		srv.AddTool(mcp.NewToolWithRawSchema("limited", "a bounded limit", json.RawMessage(
			`{"type":"object","properties":{"limit":{"type":"integer","maximum":9007199254740992}}}`)),
			func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultText("ok"), nil
			})
		ctx := t.Context()
		out, err := json.Marshal(srv.HandleMessage(ctx, msg))
		require.NoError(t, err)
		require.Contains(t, string(out), `"text":"ok"`)
		return testing.AllocsPerRun(1000, func() { srv.HandleMessage(ctx, msg) })
	}
	plain := measure(WithInputSchemaValidation())
	hooked := measure(WithInputValidationHandler(refuseWith(nil)))
	t.Logf("allocations per passing tools/call: %v without a hook, %v with one", plain, hooked)
	assert.Equal(t, plain, hooked, "a hook costs a passing call")
}

// A hook that answers a failure itself pays nothing for the default text:
// answering with the text costs at least the text's own allocations more.
func TestInputValidationHandlerOwnRefusalBuildsNoDefaultText(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	const schema = `{"type":"object","properties":{"a":{"type":"string"},"b":{"minimum":10},"c":{"enum":["x"]}}}`
	const args = `{"a":1,"b":5,"c":"y"}`
	msg := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"checked","arguments":` + args + `}}`)
	measure := func(answer *mcp.CallToolResult) float64 {
		srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputValidationHandler(refuseWith(answer)))
		srv.AddTool(mcp.NewToolWithRawSchema("checked", "", json.RawMessage(schema)), okHandler)
		ctx := t.Context()
		out, err := json.Marshal(srv.HandleMessage(ctx, msg))
		require.NoError(t, err)
		require.Contains(t, string(out), `"isError":true`)
		return testing.AllocsPerRun(1000, func() { srv.HandleMessage(ctx, msg) })
	}
	own, withText := measure(mcp.NewToolResultError("refused")), measure(nil)
	inv := invalidCall(t, schema, args)
	text := testing.AllocsPerRun(1000, func() { validationText(inv.cause) })
	assert.GreaterOrEqual(t, withText-own, text, "allocations: %v answering with the text, %v without", withText, own)
}

// The default text is built once, on its first read.
func TestInputInvalidTextIsBuiltOnce(t *testing.T) {
	inv := invalidCall(t, `{"properties":{"a":{"type":"string"}}}`, `{"a":1}`)
	assert.Equal(t, "input schema validation failed: /a: got number, want string", inv.Error())
	if !raceEnabled {
		assert.Zero(t, testing.AllocsPerRun(10, func() { _ = inv.Error() }))
	}
}

type hookContextKey struct{}

// The hook gets the request's own context at both sites: its values and its
// cancellation, and on the synchronous site not the task origin the tool
// handler's context carries.
func TestInputValidationHandlerContext(t *testing.T) {
	var mu sync.Mutex
	var hookCtx, handlerCtx context.Context
	record := func(into *context.Context, ctx context.Context) {
		mu.Lock()
		defer mu.Unlock()
		*into = ctx
	}
	answer := func(ctx context.Context, _ mcp.Tool, _ CallArguments, _ InputValidationFailure) (*mcp.CallToolResult, bool) {
		record(&hookCtx, ctx)
		return nil, false
	}
	ctx := context.WithValue(t.Context(), hookContextKey{}, "caller")
	checkHook := func(t *testing.T) {
		mu.Lock()
		defer mu.Unlock()
		require.NotNil(t, hookCtx, "the hook was not called")
		assert.Equal(t, "caller", hookCtx.Value(hookContextKey{}), "the caller's value")
		assert.NotNil(t, hookCtx.Done(), "the hook's context cannot be cancelled")
	}

	t.Run("synchronous", func(t *testing.T) {
		hookCtx, handlerCtx = nil, nil
		srv, _ := hookServer(t, hookSetup{answer: answer, opts: []ServerOption{
			WithTaskHandler(newFakeTasks()),
			WithToolHandlerMiddleware(func(next ToolHandlerFunc) ToolHandlerFunc {
				return func(ctx context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					record(&handlerCtx, ctx)
					return next(ctx, r)
				}
			}),
		}})
		srv.HandleMessage(ctx, modernBody(t, mcp.MethodToolsCall, map[string]any{"name": "t", "arguments": map[string]any{"n": 11}}, declaringTasks()))
		checkHook(t)
		mu.Lock()
		defer mu.Unlock()
		require.NotNil(t, handlerCtx, "the call did not proceed")
		require.NotNil(t, taskCallFromContext(handlerCtx), "the tool handler got no task origin, so its absence below proves nothing")
		assert.Nil(t, taskCallFromContext(hookCtx), "the hook got the task origin")
	})

	t.Run("legacy task", func(t *testing.T) {
		hookCtx = nil
		srv, _ := hookServer(t, hookSetup{answer: answer})
		out, err := callForAnswer(ctx, srv, `{"n":11}`, true)
		require.NoError(t, err)
		text, _, err := toolAnswer(out)
		require.NoError(t, err)
		assert.Equal(t, "ran", text)
		checkHook(t)
	})
}
