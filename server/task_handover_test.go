package server

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// callEra is how a test tools/call declares itself.
type callEra int

const (
	legacyCall     callEra = iota // initialize-era: no _meta
	undeclaredCall                // 2026-07-28, declaring no extension
	declaredCall                  // 2026-07-28, declaring the Tasks extension
)

// declaringTasks is the clientCapabilities of a request that declares the
// Tasks extension.
func declaringTasks() map[string]any {
	return map[string]any{"extensions": map[string]any{mcp.ExtensionTasks: map[string]any{}}}
}

// callToolIn sends a tools/call of tool to srv through HandleMessage in era,
// with ctx's values, and returns the JSON-RPC answer as the client reads it.
func callToolIn(t *testing.T, ctx context.Context, srv *MCPServer, tool string, era callEra) map[string]any {
	t.Helper()
	params := map[string]any{"name": tool}
	if era != legacyCall {
		meta := modernMeta()
		if era == declaredCall {
			meta[mcp.MetaKeyClientCapabilities] = declaringTasks()
		}
		params["_meta"] = meta
		name, ok := mcp.EncodeHeaderValue(tool)
		require.True(t, ok)
		ctx = context.WithValue(ctx, requestHeader, http.Header{
			"Mcp-Protocol-Version": {mcp.ProtocolVersion20260728},
			"Mcp-Method":           {string(mcp.MethodToolsCall)},
			"Mcp-Name":             {name},
		})
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": mcp.MethodToolsCall, "params": params})
	require.NoError(t, err)
	out, err := json.Marshal(srv.HandleMessage(ctx, body))
	require.NoError(t, err)
	var message map[string]any
	require.NoError(t, json.Unmarshal(out, &message))
	return message
}

// errorCode returns the JSON-RPC error code of message, failing when it is
// not an error.
func errorCode(t *testing.T, message map[string]any) int {
	t.Helper()
	rpcErr, ok := message["error"].(map[string]any)
	require.True(t, ok, "expected an error, got %v", message)
	code, ok := rpcErr["code"].(float64)
	require.True(t, ok, "expected an error code, got %v", rpcErr)
	return int(code)
}

// taskServer serves the tool "slow", whose handler is handler, on a server
// with a task handler unless withoutHandler is set.
func taskServer(withoutHandler bool, handler ToolHandlerFunc, opts ...ServerOption) *MCPServer {
	if !withoutHandler {
		opts = append(opts, WithTaskHandler(stubTaskHandler{}))
	}
	srv := NewMCPServer("handover", "1.0.0", append([]ServerOption{WithToolCapabilities(true)}, opts...)...)
	srv.AddTool(mcp.NewTool("slow"), handler)
	return srv
}

// returningTask is a tool handler that answers task.
func returningTask(task *mcp.ExtensionTask) ToolHandlerFunc {
	return func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Task: task}, nil
	}
}

func TestNewTaskResultRefuses(t *testing.T) {
	tests := []struct {
		name           string
		withoutHandler bool
		era            callEra
		handlerID      string
		want           []error
		notWant        error
	}{
		{"no handler", true, declaredCall, "job-1", []error{ErrNoTaskHandler}, nil},
		{"a legacy request", false, legacyCall, "job-1", []error{ErrNotModernRequest, ErrTasksNotDeclared}, nil},
		{"an undeclared request", false, undeclaredCall, "job-1", []error{ErrTasksNotDeclared}, ErrNotModernRequest},
		{"an empty handler id", false, declaredCall, "", []error{ErrEmptyHandlerID}, nil},
		{"a 55-byte handler id", false, declaredCall, strings.Repeat("x", 55), []error{ErrTaskIDTooLong}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got error
			srv := taskServer(tt.withoutHandler, func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				res, err := NewTaskResult(ctx, tt.handlerID)
				got = err
				assert.Nil(t, res)
				return mcp.NewToolResultText("sync"), nil
			})
			callToolIn(t, t.Context(), srv, "slow", tt.era)
			require.Error(t, got)
			for _, want := range tt.want {
				assert.ErrorIs(t, got, want)
			}
			if tt.notWant != nil {
				assert.NotErrorIs(t, got, tt.notWant)
			}
		})
	}
}

func TestNewTaskResultAnswersATask(t *testing.T) {
	srv := taskServer(false, func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return NewTaskResult(ctx, "job-1", WithTaskResultTTL(time.Minute), WithTaskResultPollInterval(2*time.Second))
	})
	httpServer := httptest.NewServer(NewStreamableHTTPServer(srv))
	t.Cleanup(httpServer.Close)

	meta := modernMeta()
	meta[mcp.MetaKeyClientCapabilities] = declaringTasks()
	message := decodeJSONRPC(t, postModernWithMeta(t, httpServer.URL, mcp.MethodToolsCall, map[string]any{"name": "slow"}, meta))
	result, ok := message["result"].(map[string]any)
	require.True(t, ok, "expected a result, got %v", message)
	assert.Equal(t, "task", result["resultType"])
	assert.Equal(t, "working", result["status"])
	assert.InDelta(t, 60000, result["ttlMs"], 0)
	assert.InDelta(t, 2000, result["pollIntervalMs"], 0)
	taskID, ok := result["taskId"].(string)
	require.True(t, ok, "expected a taskId, got %v", result)
	assert.NotEmpty(t, taskID)
	assert.LessOrEqual(t, len(taskID), 128)
	for _, field := range []string{"createdAt", "lastUpdatedAt"} {
		stamp, ok := result[field].(string)
		require.True(t, ok, "expected %s, got %v", field, result)
		_, err := time.Parse("2006-01-02T15:04:05.000Z", stamp)
		assert.NoError(t, err, "%s %q is not RFC 3339 UTC with milliseconds", field, stamp)
	}
}

func TestAHandlerCannotMintATaskId(t *testing.T) {
	t.Run("a forged id", func(t *testing.T) {
		srv := taskServer(false, returningTask(&mcp.ExtensionTask{TaskId: "forged", Status: mcp.TaskStatusWorking}))
		assert.Equal(t, mcp.INTERNAL_ERROR, errorCode(t, callToolIn(t, t.Context(), srv, "slow", declaredCall)))
	})
	t.Run("an id sealed for another caller", func(t *testing.T) {
		type callerKey struct{}
		task := &mcp.ExtensionTask{Status: mcp.TaskStatusWorking}
		srv := taskServer(false, returningTask(task), WithTaskCaller(func(ctx context.Context) string {
			caller, _ := ctx.Value(callerKey{}).(string)
			return caller
		}))
		var err error
		task.TaskId, err = srv.taskSealer.seal("job-1", "bob")
		require.NoError(t, err)
		ctx := context.WithValue(t.Context(), callerKey{}, "alice")
		assert.Equal(t, mcp.INTERNAL_ERROR, errorCode(t, callToolIn(t, ctx, srv, "slow", declaredCall)))
	})
}

func TestAnAfterHookCannotInjectATask(t *testing.T) {
	hooks := &Hooks{}
	hooks.AddAfterCallTool(func(_ context.Context, _ any, _ *mcp.CallToolRequest, result any) {
		if res, ok := result.(*mcp.CallToolResult); ok {
			res.Task = &mcp.ExtensionTask{TaskId: "injected", Status: mcp.TaskStatusWorking}
		}
	})
	srv := taskServer(false, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("sync"), nil
	}, WithHooks(hooks))
	assert.Equal(t, mcp.INTERNAL_ERROR, errorCode(t, callToolIn(t, t.Context(), srv, "slow", declaredCall)))
}

func TestATaskIsNeverAnsweredToALegacyRequest(t *testing.T) {
	task := &mcp.ExtensionTask{Status: mcp.TaskStatusWorking}
	srv := taskServer(false, returningTask(task))
	var err error
	task.TaskId, err = srv.taskSealer.seal("job-1", "")
	require.NoError(t, err)

	t.Run("an initialize-era call", func(t *testing.T) {
		assert.Equal(t, mcp.INTERNAL_ERROR, errorCode(t, callToolIn(t, t.Context(), srv, "slow", legacyCall)))
	})
	// The wire never gives a legacy request capabilities; a context built with
	// WithRequestProtocolInfo can.
	t.Run("a legacy context carrying the declaration", func(t *testing.T) {
		var caps mcp.ClientCapabilities
		raw, err := json.Marshal(declaringTasks())
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &caps))
		ctx := WithRequestProtocolInfo(t.Context(), &RequestProtocolInfo{ClientCapabilities: &caps})
		reqErr := srv.guardToolCallResult(ctx, 1, &mcp.CallToolResult{Task: task})
		require.NotNil(t, reqErr)
		assert.Equal(t, mcp.INTERNAL_ERROR, reqErr.code)
	})
}

func TestATaskIsNeverAnsweredToAnUndeclaredRequest(t *testing.T) {
	task := &mcp.ExtensionTask{Status: mcp.TaskStatusWorking}
	srv := taskServer(false, returningTask(task))
	var err error
	task.TaskId, err = srv.taskSealer.seal("job-1", "")
	require.NoError(t, err)
	assert.Equal(t, mcp.INTERNAL_ERROR, errorCode(t, callToolIn(t, t.Context(), srv, "slow", undeclaredCall)))
}

func TestTaskOriginIsStampedOnlyWhenATaskIsPossible(t *testing.T) {
	tests := []struct {
		name           string
		withoutHandler bool
		era            callEra
		want           string
	}{
		{"no handler", true, declaredCall, ""},
		{"a legacy request", false, legacyCall, ""},
		{"an undeclared request", false, undeclaredCall, ""},
		{"a declared request", false, declaredCall, "slow"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var origin TaskOrigin
			srv := taskServer(tt.withoutHandler, func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				origin = TaskOriginFromContext(ctx)
				return mcp.NewToolResultText("sync"), nil
			})
			message := callToolIn(t, t.Context(), srv, "slow", tt.era)
			require.Contains(t, message, "result")
			assert.Equal(t, tt.want, origin.Tool())
			assert.Equal(t, tt.want == "", origin.IsZero())
		})
	}
}

// The fallback idiom of the design: a handler that cannot hand over answers
// synchronously, a legacy client included.
func TestALegacyRequestFallsBackToASynchronousAnswer(t *testing.T) {
	srv := taskServer(false, func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := NewTaskResult(ctx, "job-1")
		if errors.Is(err, ErrTasksNotDeclared) {
			return mcp.NewToolResultText("sync"), nil
		}
		return res, err
	})
	for _, era := range []callEra{legacyCall, undeclaredCall} {
		result, ok := callToolIn(t, t.Context(), srv, "slow", era)["result"].(map[string]any)
		require.True(t, ok, "era %d: expected a result", era)
		assert.Equal(t, []any{map[string]any{"type": "text", "text": "sync"}}, result["content"], "era %d", era)
	}
}

func TestNewTaskResultOutsideAToolCall(t *testing.T) {
	t.Run("a context with no server", func(t *testing.T) {
		_, err := NewTaskResult(t.Context(), "job-1")
		assert.ErrorIs(t, err, ErrNotToolCall)
	})
	t.Run("a declared request's context that is not a tool call's", func(t *testing.T) {
		srv := taskServer(false, returningTask(nil))
		var caps mcp.ClientCapabilities
		raw, err := json.Marshal(declaringTasks())
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(raw, &caps))
		ctx := &serverContext{Context: WithRequestProtocolInfo(t.Context(), &RequestProtocolInfo{Modern: true, ClientCapabilities: &caps}), srv: srv}
		_, err = NewTaskResult(ctx, "job-1")
		assert.ErrorIs(t, err, ErrNotToolCall)
	})
}

// taskResultOf returns the result of message, failing unless it is a task.
func taskResultOf(t *testing.T, message map[string]any) map[string]any {
	t.Helper()
	result, ok := message["result"].(map[string]any)
	require.True(t, ok, "expected a result, got %v", message)
	require.Equal(t, "task", result["resultType"], "expected a task, got %v", result)
	return result
}

// recentUTC asserts that stamp is a UTC timestamp within a minute of now.
func recentUTC(t *testing.T, stamp any) {
	t.Helper()
	s, ok := stamp.(string)
	require.True(t, ok, "expected a timestamp, got %v", stamp)
	require.True(t, strings.HasSuffix(s, "Z"), "%q is not UTC", s)
	at, err := time.Parse(time.RFC3339Nano, s)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), at, time.Minute, "%q", s)
}

func TestNewTaskResultOptions(t *testing.T) {
	start := time.Date(2026, 3, 4, 5, 6, 7, 890_000_000, time.FixedZone("UTC+2", 2*60*60))
	later := time.Now().Add(time.Hour).Truncate(time.Millisecond)
	tests := []struct {
		name  string
		opts  []TaskResultOption
		check func(t *testing.T, result map[string]any)
	}{
		{"no options", nil, func(t *testing.T, result map[string]any) {
			ttl, present := result["ttlMs"]
			assert.True(t, present, "ttlMs must be sent, null for unlimited")
			assert.Nil(t, ttl)
			assert.NotContains(t, result, "pollIntervalMs")
			assert.NotContains(t, result, "statusMessage")
			recentUTC(t, result["createdAt"])
			assert.Equal(t, result["createdAt"], result["lastUpdatedAt"])
		}},
		{"every option", []TaskResultOption{
			WithTaskResultTTL(90 * time.Second),
			WithTaskResultPollInterval(1500 * time.Millisecond),
			WithTaskResultStatusMessage("queued"),
			WithTaskResultCreatedAt(start),
		}, func(t *testing.T, result map[string]any) {
			assert.InDelta(t, 90000, result["ttlMs"], 0)
			assert.InDelta(t, 1500, result["pollIntervalMs"], 0)
			assert.Equal(t, "queued", result["statusMessage"])
			assert.Equal(t, "2026-03-04T03:06:07.890Z", result["createdAt"])
			recentUTC(t, result["lastUpdatedAt"])
		}},
		{"a sub-millisecond poll interval rounds up", []TaskResultOption{WithTaskResultPollInterval(time.Microsecond)}, func(t *testing.T, result map[string]any) {
			assert.InDelta(t, 1, result["pollIntervalMs"], 0)
		}},
		{"a zero poll interval sends none", []TaskResultOption{WithTaskResultPollInterval(0)}, func(t *testing.T, result map[string]any) {
			assert.NotContains(t, result, "pollIntervalMs")
		}},
		{"a negative TTL is zero", []TaskResultOption{WithTaskResultTTL(-time.Second)}, func(t *testing.T, result map[string]any) {
			assert.InDelta(t, 0, result["ttlMs"], 0)
		}},
		{"a creation after now is also the last update", []TaskResultOption{WithTaskResultCreatedAt(later)}, func(t *testing.T, result map[string]any) {
			want := later.UTC().Format("2006-01-02T15:04:05.000Z07:00")
			assert.Equal(t, want, result["createdAt"])
			assert.Equal(t, want, result["lastUpdatedAt"])
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := taskServer(false, func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return NewTaskResult(ctx, "job-1", tt.opts...)
			})
			tt.check(t, taskResultOf(t, callToolIn(t, t.Context(), srv, "slow", declaredCall)))
		})
	}
}

// A poll interval goes out in whole milliseconds rounded up, and a TTL in
// whole milliseconds truncated, over the whole range of time.Duration: the
// longest one does not wrap around to a negative interval, and the rounding
// stops at the longest interval a client can turn back into a duration.
func TestTaskDurationsInMilliseconds(t *testing.T) {
	const longest = time.Duration(math.MaxInt64)
	for _, tt := range []struct {
		d         time.Duration
		poll, ttl int64
	}{
		{time.Nanosecond, 1, 0},
		{time.Millisecond, 1, 1},
		{time.Millisecond + time.Nanosecond, 2, 1},
		{1500 * time.Millisecond, 1500, 1500},
		{longest - longest%time.Millisecond, int64(longest / time.Millisecond), int64(longest / time.Millisecond)},
		{longest, int64(longest / time.Millisecond), int64(longest / time.Millisecond)},
	} {
		poll := taskPollIntervalMs(&tt.d)
		require.NotNil(t, poll, "%d", tt.d)
		assert.Equal(t, tt.poll, *poll, "pollIntervalMs of %d ns", int64(tt.d))
		assert.Equal(t, tt.ttl, *taskTTLMs(&tt.d), "ttlMs of %d ns", int64(tt.d))
	}
}

func TestAResultTypeOfTaskWithoutATaskIsRefused(t *testing.T) {
	srv := taskServer(false, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res := mcp.NewToolResultText("not a task")
		res.ResultType = mcp.ResultTypeTask
		return res, nil
	})
	for _, era := range []callEra{legacyCall, undeclaredCall, declaredCall} {
		assert.Equal(t, mcp.INTERNAL_ERROR, errorCode(t, callToolIn(t, t.Context(), srv, "slow", era)), "era %d", era)
	}
}

// TestABoundCallerHandsOver pins the caller binding's positive path, and that
// the caller is resolved on the request's context as the transport built it:
// a principal tool middleware sets is not seen, by the seal or by the guard.
func TestABoundCallerHandsOver(t *testing.T) {
	type principalKey struct{}
	caller := WithTaskCaller(func(ctx context.Context) string {
		p, _ := ctx.Value(principalKey{}).(string)
		return p
	})
	handOver := func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return NewTaskResult(ctx, "job-1")
	}
	setsMallory := WithToolHandlerMiddleware(func(next ToolHandlerFunc) ToolHandlerFunc {
		return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return next(context.WithValue(ctx, principalKey{}, "mallory"), request)
		}
	})
	tests := []struct {
		name string
		opts []ServerOption
	}{
		{"the request's principal", []ServerOption{caller}},
		{"a principal set by tool middleware is not seen", []ServerOption{caller, setsMallory}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := taskServer(false, handOver, tt.opts...)
			ctx := context.WithValue(t.Context(), principalKey{}, "alice")
			taskID, ok := taskResultOf(t, callToolIn(t, ctx, srv, "slow", declaredCall))["taskId"].(string)
			require.True(t, ok)
			handlerID, err := srv.taskSealer.open(taskID, "alice")
			require.NoError(t, err)
			assert.Equal(t, "job-1", handlerID)
			for _, other := range []string{"bob", "", "mallory"} {
				_, err := srv.taskSealer.open(taskID, other)
				assert.ErrorIs(t, err, errTaskUnknown, "the id opened for %q", other)
			}
		})
	}
}

// A declared tools/call on a server with a task handler pays one allocation
// over the same call without one: the stamp of its origin and caller. 39
// allocations without a handler, 40 with one.
func TestHandleMessageDeclaredTaskCallAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	srv, ctx := readOnceServer(t)
	srv.taskHandler = stubTaskHandler{}
	msg := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"c","version":"1"},"io.modelcontextprotocol/clientCapabilities":{"extensions":{"io.modelcontextprotocol/tasks":{}}}}}}`)
	out, err := json.Marshal(srv.HandleMessage(ctx, msg))
	require.NoError(t, err)
	require.Contains(t, string(out), `"text":"ok"`)
	allocs := testing.AllocsPerRun(1000, func() { srv.HandleMessage(ctx, msg) })
	assert.LessOrEqual(t, allocs, 40.0, "allocations per declared tools/call on a server with a task handler")
}

// The caller function runs only when a call is handed over: once for the seal
// and once for the guard, never on a declared call that answers at once.
func TestTheCallerIsResolvedOnlyOnAHandOver(t *testing.T) {
	var calls atomic.Int32
	srv := taskServer(false, func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return NewTaskResult(ctx, "job-1")
	}, WithTaskCaller(func(context.Context) string {
		calls.Add(1)
		return "alice"
	}))
	srv.AddTool(mcp.NewTool("quick"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("sync"), nil
	})

	require.Contains(t, callToolIn(t, t.Context(), srv, "quick", declaredCall), "result")
	assert.Equal(t, int32(0), calls.Load(), "the caller ran on a synchronous answer")

	taskResultOf(t, callToolIn(t, t.Context(), srv, "slow", declaredCall))
	assert.Equal(t, int32(2), calls.Load(), "the caller runs for the seal and for the guard")
}

// DetachTaskContext keeps the call's values, and drops its cancellation, its
// session and its task stamp: what is sent under it reaches no client, and a
// Run under it runs at once.
func TestDetachTaskContextKeepsValuesAndDropsTheRest(t *testing.T) {
	type valueKey struct{}
	var (
		detached context.Context
		session  ClientSession
		stamped  bool
	)
	srv := taskServer(false, func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		session = ClientSessionFromContext(ctx)
		stamped = !TaskOriginFromContext(ctx).IsZero()
		detached = DetachTaskContext(ctx)
		return mcp.NewToolResultText("sync"), nil
	})
	reqCtx := srv.WithContext(context.WithValue(t.Context(), valueKey{}, "kept"),
		fakeSession{sessionID: "s1", notificationChannel: make(chan mcp.JSONRPCNotification, 1), initialized: true})
	reqCtx, cancel := context.WithCancel(reqCtx)
	callToolIn(t, reqCtx, srv, "slow", declaredCall)
	cancel()
	require.NotNil(t, session, "the call carried a session")
	require.True(t, stamped, "the call could become a task")
	require.NotNil(t, detached)

	assert.Equal(t, "kept", detached.Value(valueKey{}))
	assert.Same(t, srv, ServerFromContext(detached))
	assert.True(t, IsModernRequest(detached), "the request's protocol information is kept")
	assert.NoError(t, detached.Err(), "the call's cancellation is dropped")
	assert.Nil(t, ClientSessionFromContext(detached), "the call's session is dropped")
	assert.ErrorIs(t, srv.SendNotificationToClient(detached, "notifications/progress", nil), ErrNotificationNotInitialized)
	assert.True(t, TaskOriginFromContext(detached).IsZero(), "the task stamp is dropped")
	_, err := NewTaskResult(detached, "job-1")
	assert.ErrorIs(t, err, ErrNotToolCall)

	mem := NewMemoryTasks()
	t.Cleanup(mem.Close)
	ran := false
	result, err := mem.Run(detached, func(context.Context, func(string)) (*mcp.CallToolResult, error) {
		ran = true
		return mcp.NewToolResultText("at once"), nil
	})
	require.NoError(t, err)
	assert.True(t, ran, "a Run under a detached context runs its function at once")
	assert.Nil(t, result.Task)
}
