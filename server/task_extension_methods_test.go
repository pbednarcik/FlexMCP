package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	taskCreated = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	taskUpdated = taskCreated.Add(time.Second)
)

// extensionTaskMethods are the task methods of a modern request.
var extensionTaskMethods = []mcp.MCPMethod{mcp.MethodTasksGet, mcp.MethodTasksCancel, mcp.MethodTasksUpdate}

// fakeTasks is a TaskHandler over states kept by handler id. It records the
// refs its methods see; err, when set, is what every method returns,
// panicWith makes every method panic, and onContext sees the context each
// method is called with, under the lock.
type fakeTasks struct {
	mu        sync.Mutex
	states    map[string]TaskState
	err       error
	panicWith any
	onContext func(method string, ctx context.Context)
	stateRefs []TaskRef
	cancelled []TaskRef
}

func newFakeTasks() *fakeTasks {
	return &fakeTasks{states: map[string]TaskState{}}
}

func (f *fakeTasks) TaskState(ctx context.Context, ref TaskRef) (TaskState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panicWith != nil {
		panic(f.panicWith)
	}
	f.stateRefs = append(f.stateRefs, ref)
	if f.onContext != nil {
		f.onContext("TaskState", ctx)
	}
	state, ok := f.states[ref.HandlerID]
	switch {
	case f.err != nil:
		return TaskState{}, f.err
	case !ok:
		return TaskState{}, ErrTaskGone
	}
	return state, nil
}

func (f *fakeTasks) CancelTask(ctx context.Context, ref TaskRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panicWith != nil {
		panic(f.panicWith)
	}
	f.cancelled = append(f.cancelled, ref)
	if f.onContext != nil {
		f.onContext("CancelTask", ctx)
	}
	if f.err != nil {
		return f.err
	}
	if _, ok := f.states[ref.HandlerID]; !ok {
		return ErrTaskGone
	}
	return nil
}

// set edits the state of job, a working task when it is new.
func (f *fakeTasks) set(job string, edit func(*TaskState)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state, ok := f.states[job]
	if !ok {
		state = TaskState{
			Status:        mcp.TaskStatusWorking,
			CreatedAt:     taskCreated,
			LastUpdatedAt: taskUpdated,
			TTL:           new(time.Minute),
			PollInterval:  new(2 * time.Second),
		}
	}
	edit(&state)
	f.states[job] = state
}

// forget drops job, so the handler answers ErrTaskGone for it.
func (f *fakeTasks) forget(job string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.states, job)
}

// fakeInputTasks is a fakeTasks that takes input, recording what it is given.
type fakeInputTasks struct {
	*fakeTasks
	updated   []TaskRef
	responses []mcp.InputResponses
}

func (f *fakeInputTasks) UpdateTask(ctx context.Context, ref TaskRef, responses mcp.InputResponses) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panicWith != nil {
		panic(f.panicWith)
	}
	f.updated = append(f.updated, ref)
	if f.onContext != nil {
		f.onContext("UpdateTask", ctx)
	}
	f.responses = append(f.responses, responses)
	return f.err
}

// taskFixture is a server whose tool "slow" hands each call over as a task of
// the job its "job" argument names, keeping the call's origin with the job.
type taskFixture struct {
	srv   *MCPServer
	tasks *fakeTasks
	input *fakeInputTasks
}

// newTaskFixture builds a taskFixture whose handler takes input when input is
// set.
func newTaskFixture(t *testing.T, input bool, opts ...ServerOption) *taskFixture {
	t.Helper()
	f := &taskFixture{tasks: newFakeTasks()}
	var handler TaskHandler = f.tasks
	if input {
		f.input = &fakeInputTasks{fakeTasks: f.tasks}
		handler = f.input
	}
	f.srv = NewMCPServer("tasks", "1.0.0", append([]ServerOption{WithToolCapabilities(true), WithTaskHandler(handler)}, opts...)...)
	f.addTool(nil)
	return f
}

// addTool registers, or registers again, the tool "slow", declaring
// outputSchema when it is not nil.
func (f *taskFixture) addTool(outputSchema json.RawMessage) {
	opts := []mcp.ToolOption{mcp.WithString("job")}
	if outputSchema != nil {
		opts = append(opts, mcp.WithRawOutputSchema(outputSchema))
	}
	f.srv.AddTool(mcp.NewTool("slow", opts...), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		job := request.GetString("job", "job-1")
		f.tasks.set(job, func(state *TaskState) { state.Origin = TaskOriginFromContext(ctx) })
		return NewTaskResult(ctx, job)
	})
}

// handOver calls "slow" for job on a declared request with ctx's values and
// returns the task id it answers.
func (f *taskFixture) handOver(t *testing.T, ctx context.Context, job string) string {
	t.Helper()
	params := map[string]any{"name": "slow", "arguments": map[string]any{"job": job}}
	result := taskResultOf(t, sendTaskMethod(t, ctx, f.srv, mcp.MethodToolsCall, params, declaringTasks()))
	taskID, ok := result["taskId"].(string)
	require.True(t, ok, "expected a taskId, got %v", result)
	return taskID
}

// modernBody is a modern request of method with params, its client declaring
// capabilities, or nothing when capabilities is nil.
func modernBody(t *testing.T, method mcp.MCPMethod, params, capabilities map[string]any) json.RawMessage {
	t.Helper()
	meta := modernMeta()
	if capabilities != nil {
		meta[mcp.MetaKeyClientCapabilities] = capabilities
	}
	params["_meta"] = meta
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	require.NoError(t, err)
	return body
}

// sendTaskMethod sends modernBody through HandleMessage with ctx's values and
// returns the answer as the client reads it.
func sendTaskMethod(t *testing.T, ctx context.Context, srv *MCPServer, method mcp.MCPMethod, params, capabilities map[string]any) map[string]any {
	t.Helper()
	message, err := answerOf(srv.HandleMessage(ctx, modernBody(t, method, params, capabilities)))
	require.NoError(t, err)
	return message
}

// answerOf is a JSON-RPC answer as the client reads it.
func answerOf(answer mcp.JSONRPCMessage) (map[string]any, error) {
	out, err := json.Marshal(answer)
	if err != nil {
		return nil, err
	}
	var message map[string]any
	err = json.Unmarshal(out, &message)
	return message, err
}

// taskMethodParams are the params of method for taskID.
func taskMethodParams(method mcp.MCPMethod, taskID any) map[string]any {
	params := map[string]any{"taskId": taskID}
	if method == mcp.MethodTasksUpdate {
		params["inputResponses"] = map[string]any{}
	}
	return params
}

// completeResult returns the result of message without its _meta, failing
// unless it is a complete result.
func completeResult(t *testing.T, message map[string]any) map[string]any {
	t.Helper()
	result, ok := message["result"].(map[string]any)
	require.True(t, ok, "expected a result, got %v", message)
	require.Equal(t, "complete", result["resultType"], "expected a complete result, got %v", result)
	delete(result, "_meta")
	return result
}

// rpcErrorOf returns the error member of message, failing unless there is
// one.
func rpcErrorOf(t *testing.T, message map[string]any) map[string]any {
	t.Helper()
	rpcErr, ok := message["error"].(map[string]any)
	require.True(t, ok, "expected an error, got %v", message)
	return rpcErr
}

// asJSON is v as a client reads it.
func asJSON(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	require.NoError(t, err)
	var out any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func TestExtensionGetTaskRendersEachStatus(t *testing.T) {
	ask := mcp.InputRequests{"ask": {
		Method:      mcp.MethodElicitationCreate,
		Elicitation: &mcp.ElicitationParams{Message: "Which branch?", RequestedSchema: map[string]any{"type": "object"}},
	}}
	declaringElicitation := declaringTasks()
	declaringElicitation["elicitation"] = map[string]any{}
	tests := []struct {
		name         string
		input        bool
		capabilities map[string]any
		edit         func(*TaskState)
		want         map[string]any
	}{
		{"working", false, nil, func(s *TaskState) { s.StatusMessage = "halfway" },
			map[string]any{"status": "working", "statusMessage": "halfway"}},
		{"working, last updated before its creation", false, nil, func(s *TaskState) { s.LastUpdatedAt = taskCreated.Add(-time.Hour) },
			map[string]any{"status": "working", "lastUpdatedAt": "2026-03-04T05:06:07.000Z"}},
		{"completed", false, nil, func(s *TaskState) {
			s.Status = mcp.TaskStatusCompleted
			s.Result = mcp.NewToolResultText("done")
		}, map[string]any{"status": "completed", "result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "done"}}, "resultType": "complete",
		}}},
		{"completed with an isError result", false, nil, func(s *TaskState) {
			s.Status = mcp.TaskStatusCompleted
			s.Result = mcp.NewToolResultError("bad input")
		}, map[string]any{"status": "completed", "result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "bad input"}}, "isError": true, "resultType": "complete",
		}}},
		{"failed with a handler error", false, nil, func(s *TaskState) {
			s.Status = mcp.TaskStatusFailed
			s.Err = errors.New("disk full at /srv/secret")
		}, map[string]any{"status": "failed", "statusMessage": "task failed", "error": map[string]any{
			"code": float64(mcp.INTERNAL_ERROR), "message": "internal error",
		}}},
		{"failed with a NewHandlerError", false, nil, func(s *TaskState) {
			s.Status = mcp.TaskStatusFailed
			s.StatusMessage = "rejected"
			s.Err = NewHandlerError(testHandlerJSONRPCError{})
		}, map[string]any{"status": "failed", "statusMessage": "rejected", "error": map[string]any{
			"code": float64(mcp.INVALID_PARAMS), "message": "invalid customer ID", "data": map[string]any{"field": "customer_id"},
		}}},
		{"cancelled", false, nil, func(s *TaskState) { s.Status = mcp.TaskStatusCancelled },
			map[string]any{"status": "cancelled"}},
		{"input_required", true, declaringElicitation, func(s *TaskState) {
			s.Status = mcp.TaskStatusInputRequired
			s.InputRequests = ask
		}, map[string]any{"status": "input_required", "inputRequests": asJSON(t, ask)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTaskFixture(t, tt.input)
			taskID := f.handOver(t, t.Context(), "job-1")
			f.tasks.set("job-1", tt.edit)
			capabilities := tt.capabilities
			if capabilities == nil {
				capabilities = declaringTasks()
			}
			got := completeResult(t, sendTaskMethod(t, t.Context(), f.srv, mcp.MethodTasksGet, map[string]any{"taskId": taskID}, capabilities))
			want := map[string]any{
				"resultType":     "complete",
				"taskId":         taskID,
				"createdAt":      "2026-03-04T05:06:07.000Z",
				"lastUpdatedAt":  "2026-03-04T05:06:08.000Z",
				"ttlMs":          float64(60000),
				"pollIntervalMs": float64(2000),
			}
			maps.Copy(want, tt.want)
			assert.Equal(t, want, got)
		})
	}
}

func TestExtensionGetTaskValidatesAgainstTheOriginalSchema(t *testing.T) {
	numberSchema := json.RawMessage(`{"type":"object","properties":{"n":{"type":"number"}},"required":["n"]}`)
	stringSchema := json.RawMessage(`{"type":"object","properties":{"s":{"type":"string"}},"required":["s"]}`)
	tests := []struct {
		name       string
		structured map[string]any
		isError    bool
	}{
		{"valid under the schema of the call", map[string]any{"n": 1}, false},
		{"valid only under the schema registered since", map[string]any{"s": "x"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTaskFixture(t, false, WithOutputSchemaValidation())
			f.addTool(numberSchema)
			taskID := f.handOver(t, t.Context(), "job-1")
			f.addTool(stringSchema)
			f.tasks.set("job-1", func(s *TaskState) {
				s.Status = mcp.TaskStatusCompleted
				s.Result = mcp.NewToolResultText("structured")
				s.Result.StructuredContent = tt.structured
			})
			got := completeResult(t, sendTaskMethod(t, t.Context(), f.srv, mcp.MethodTasksGet, map[string]any{"taskId": taskID}, declaringTasks()))
			assert.Equal(t, "completed", got["status"])
			inner, ok := got["result"].(map[string]any)
			require.True(t, ok, "expected a result, got %v", got)
			assert.Equal(t, "complete", inner["resultType"])
			if tt.isError {
				assert.Equal(t, true, inner["isError"])
				return
			}
			assert.NotContains(t, inner, "isError")
			assert.Equal(t, asJSON(t, tt.structured), inner["structuredContent"])
		})
	}
}

// A completed result is limited to the size limit of the call it was handed
// over from: the server's limit is gone by the time of the get, so the limit
// can only come from the origin.
func TestExtensionGetTaskLimitsToTheOriginsSizeLimit(t *testing.T) {
	f := newTaskFixture(t, false, WithToolResultSizeLimit(600))
	taskID := f.handOver(t, t.Context(), "job-1")
	f.srv.toolResultSizeLimit = 0
	f.tasks.set("job-1", func(s *TaskState) {
		s.Status = mcp.TaskStatusCompleted
		s.Result = mcp.NewToolResultText(strings.Repeat("x", 2000))
	})
	got := completeResult(t, sendTaskMethod(t, t.Context(), f.srv, mcp.MethodTasksGet, map[string]any{"taskId": taskID}, declaringTasks()))
	assert.Equal(t, "completed", got["status"])
	inner, ok := got["result"].(map[string]any)
	require.True(t, ok, "expected a result, got %v", got)
	assert.Equal(t, true, inner["isError"])
	assert.Equal(t, asJSON(t, []mcp.Content{mcp.NewTextContent("tool result exceeds 600 bytes")}), inner["content"])
}

func TestExtensionTaskIdChecks(t *testing.T) {
	type callerKey struct{}
	byCaller := WithTaskCaller(func(ctx context.Context) string {
		caller, _ := ctx.Value(callerKey{}).(string)
		return caller
	})
	messages := map[string]string{
		"unknown": "unknown: no such task",
		"gone":    "gone: the task ended or the server restarted; call the tool again",
	}
	tests := []struct {
		name   string
		taskID func(t *testing.T, f *taskFixture) string
		caller string
		reason string
	}{
		{"unknown", func(*testing.T, *taskFixture) string { return "not-a-task" }, "", "unknown"},
		{"another epoch", func(t *testing.T, _ *taskFixture) string {
			taskID, err := newTaskSealer(nil, "tasks").seal("job-1", "")
			require.NoError(t, err)
			return taskID
		}, "", "gone"},
		{"another caller", func(t *testing.T, f *taskFixture) string {
			return f.handOver(t, context.WithValue(t.Context(), callerKey{}, "alice"), "job-1")
		}, "bob", "unknown"},
		{"ErrTaskGone from the handler", func(t *testing.T, f *taskFixture) string {
			taskID := f.handOver(t, context.WithValue(t.Context(), callerKey{}, "alice"), "job-1")
			f.tasks.forget("job-1")
			return taskID
		}, "alice", "gone"},
	}
	for _, tt := range tests {
		for _, method := range extensionTaskMethods {
			t.Run(tt.name+"/"+string(method), func(t *testing.T) {
				f := newTaskFixture(t, false, byCaller)
				taskID := tt.taskID(t, f)
				ctx := context.WithValue(t.Context(), callerKey{}, tt.caller)
				rpcErr := rpcErrorOf(t, sendTaskMethod(t, ctx, f.srv, method, taskMethodParams(method, taskID), declaringTasks()))
				assert.InDelta(t, mcp.INVALID_PARAMS, rpcErr["code"], 0)
				assert.Equal(t, map[string]any{"reason": tt.reason}, rpcErr["data"])
				assert.Equal(t, messages[tt.reason], rpcErr["message"])
				assert.NotContains(t, rpcErr["message"], taskID)
			})
		}
	}
}

func TestExtensionTaskMethodsNeedTheExtension(t *testing.T) {
	for _, method := range extensionTaskMethods {
		t.Run(string(method), func(t *testing.T) {
			f := newTaskFixture(t, true)
			taskID := f.handOver(t, t.Context(), "job-1")
			rpcErr := rpcErrorOf(t, sendTaskMethod(t, t.Context(), f.srv, method, taskMethodParams(method, taskID), nil))
			assert.InDelta(t, mcp.MISSING_REQUIRED_CLIENT_CAPABILITY, rpcErr["code"], 0)
			data, ok := rpcErr["data"].(map[string]any)
			require.True(t, ok, "expected data, got %v", rpcErr)
			required, ok := data["requiredCapabilities"].(map[string]any)
			require.True(t, ok, "expected requiredCapabilities, got %v", data)
			assert.Equal(t, map[string]any{mcp.ExtensionTasks: map[string]any{}}, required["extensions"])
			assert.Empty(t, f.tasks.stateRefs)
			assert.Empty(t, f.tasks.cancelled)
			assert.Empty(t, f.input.updated)
		})
	}
}

func TestExtensionTaskMethodsWithoutAHandlerAreNotFound(t *testing.T) {
	srv := NewMCPServer("tasks", "1.0.0", WithToolCapabilities(true), WithTaskCapabilities(true, true, true))
	for _, method := range extensionTaskMethods {
		t.Run(string(method), func(t *testing.T) {
			rpcErr := rpcErrorOf(t, sendTaskMethod(t, t.Context(), srv, method, taskMethodParams(method, "abc"), declaringTasks()))
			assert.InDelta(t, mcp.METHOD_NOT_FOUND, rpcErr["code"], 0)
			assert.Equal(t, `"`+string(method)+`" not supported`, rpcErr["message"])
		})
	}
}

// A taskId that is not a string is -32602. A number or an object fails to
// decode. null decodes to "", which names no task, so it is the unknown
// refusal: the schema's taskId is a string, and -32602 is what the mismatch
// calls for either way.
func TestExtensionTaskIdMustBeAString(t *testing.T) {
	tests := []struct {
		name   string
		taskID any
		data   any
	}{
		{"a number", 1, nil},
		{"an object", map[string]any{}, nil},
		{"null", nil, map[string]any{"reason": "unknown"}},
	}
	for _, method := range extensionTaskMethods {
		for _, tt := range tests {
			t.Run(string(method)+"/"+tt.name, func(t *testing.T) {
				f := newTaskFixture(t, true)
				rpcErr := rpcErrorOf(t, sendTaskMethod(t, t.Context(), f.srv, method, taskMethodParams(method, tt.taskID), declaringTasks()))
				assert.InDelta(t, mcp.INVALID_PARAMS, rpcErr["code"], 0)
				assert.Equal(t, tt.data, rpcErr["data"])
				assert.Empty(t, f.tasks.stateRefs)
				assert.Empty(t, f.tasks.cancelled)
				assert.Empty(t, f.input.updated)
			})
		}
	}
}

func TestExtensionGetTaskRefusesBadStates(t *testing.T) {
	completed := func(result *mcp.CallToolResult) func(*TaskState) {
		return func(s *TaskState) {
			s.Status = mcp.TaskStatusCompleted
			s.Result = result
		}
	}
	askingForInput := mcp.NewToolResultText("more")
	askingForInput.ResultType = mcp.ResultTypeInputRequired
	typedTask := mcp.NewToolResultText("task")
	typedTask.ResultType = mcp.ResultTypeTask
	tests := []struct {
		name  string
		input bool
		edit  func(*TaskState)
	}{
		{"a completed result that is a task", false, completed(&mcp.CallToolResult{Task: &mcp.ExtensionTask{TaskId: "nested", Status: mcp.TaskStatusWorking}})},
		{"a completed result of resultType task", false, completed(typedTask)},
		{"a completed result asking for input", false, completed(askingForInput)},
		{"a completed task without a result", false, completed(nil)},
		{"a completed task with a zero origin", false, func(s *TaskState) {
			completed(mcp.NewToolResultText("done"))(s)
			s.Origin = TaskOrigin{}
		}},
		{"a failed task without an error", false, func(s *TaskState) { s.Status = mcp.TaskStatusFailed }},
		{"input_required without a TaskInputHandler", false, func(s *TaskState) { s.Status = mcp.TaskStatusInputRequired }},
		{"an unknown status", true, func(s *TaskState) { s.Status = "paused" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newTaskFixture(t, tt.input)
			taskID := f.handOver(t, t.Context(), "job-1")
			f.tasks.set("job-1", tt.edit)
			rpcErr := rpcErrorOf(t, sendTaskMethod(t, t.Context(), f.srv, mcp.MethodTasksGet, map[string]any{"taskId": taskID}, declaringTasks()))
			assert.InDelta(t, mcp.INTERNAL_ERROR, rpcErr["code"], 0)
			assert.NotContains(t, rpcErr["message"], taskID)
		})
	}
}

func TestExtensionGetTaskAsksOnlyForDeclaredInput(t *testing.T) {
	f := newTaskFixture(t, true)
	taskID := f.handOver(t, t.Context(), "job-1")
	f.tasks.set("job-1", func(s *TaskState) {
		s.Status = mcp.TaskStatusInputRequired
		s.InputRequests = mcp.InputRequests{"ask": {Method: mcp.MethodElicitationCreate, Elicitation: &mcp.ElicitationParams{Message: "?"}}}
	})
	rpcErr := rpcErrorOf(t, sendTaskMethod(t, t.Context(), f.srv, mcp.MethodTasksGet, map[string]any{"taskId": taskID}, declaringTasks()))
	assert.InDelta(t, mcp.MISSING_REQUIRED_CLIENT_CAPABILITY, rpcErr["code"], 0)
	data, ok := rpcErr["data"].(map[string]any)
	require.True(t, ok, "expected data, got %v", rpcErr)
	assert.Equal(t, map[string]any{"elicitation": map[string]any{"form": map[string]any{}}}, data["requiredCapabilities"])
}

func TestExtensionCancelAcknowledges(t *testing.T) {
	for _, status := range []mcp.TaskStatus{mcp.TaskStatusWorking, mcp.TaskStatusCompleted} {
		t.Run(string(status), func(t *testing.T) {
			f := newTaskFixture(t, false)
			taskID := f.handOver(t, t.Context(), "job-1")
			f.tasks.set("job-1", func(s *TaskState) {
				s.Status = status
				s.Result = mcp.NewToolResultText("done")
			})
			got := completeResult(t, sendTaskMethod(t, t.Context(), f.srv, mcp.MethodTasksCancel, map[string]any{"taskId": taskID}, declaringTasks()))
			assert.Equal(t, map[string]any{"resultType": "complete"}, got)
			assert.Equal(t, []TaskRef{{TaskID: taskID, HandlerID: "job-1"}}, f.tasks.cancelled)
		})
	}
}

func TestExtensionUpdateTask(t *testing.T) {
	update := func(taskID string) map[string]any {
		return map[string]any{"taskId": taskID, "inputResponses": map[string]any{
			"ask": map[string]any{"action": "accept", "content": map[string]any{"branch": "main"}},
		}}
	}
	t.Run("routed to a TaskInputHandler", func(t *testing.T) {
		f := newTaskFixture(t, true)
		taskID := f.handOver(t, t.Context(), "job-1")
		got := completeResult(t, sendTaskMethod(t, t.Context(), f.srv, mcp.MethodTasksUpdate, update(taskID), declaringTasks()))
		assert.Equal(t, map[string]any{"resultType": "complete"}, got)
		assert.Equal(t, []TaskRef{{TaskID: taskID, HandlerID: "job-1"}}, f.input.updated)
		require.Len(t, f.input.responses, 1)
		assert.Contains(t, f.input.responses[0], "ask")
		assert.Empty(t, f.tasks.stateRefs)
	})
	t.Run("acknowledged without one", func(t *testing.T) {
		f := newTaskFixture(t, false)
		taskID := f.handOver(t, t.Context(), "job-1")
		got := completeResult(t, sendTaskMethod(t, t.Context(), f.srv, mcp.MethodTasksUpdate, update(taskID), declaringTasks()))
		assert.Equal(t, map[string]any{"resultType": "complete"}, got)
		assert.Equal(t, []TaskRef{{TaskID: taskID, HandlerID: "job-1"}}, f.tasks.stateRefs)
	})
	t.Run("gone without one", func(t *testing.T) {
		f := newTaskFixture(t, false)
		taskID := f.handOver(t, t.Context(), "job-1")
		f.tasks.forget("job-1")
		rpcErr := rpcErrorOf(t, sendTaskMethod(t, t.Context(), f.srv, mcp.MethodTasksUpdate, update(taskID), declaringTasks()))
		assert.InDelta(t, mcp.INVALID_PARAMS, rpcErr["code"], 0)
		assert.Equal(t, map[string]any{"reason": "gone"}, rpcErr["data"])
	})
}

func TestTwoTasksOnOneJobCancelSeparately(t *testing.T) {
	f := newTaskFixture(t, false)
	first := f.handOver(t, t.Context(), "job-1")
	second := f.handOver(t, t.Context(), "job-1")
	require.NotEqual(t, first, second)
	completeResult(t, sendTaskMethod(t, t.Context(), f.srv, mcp.MethodTasksCancel, map[string]any{"taskId": first}, declaringTasks()))
	assert.Equal(t, []TaskRef{{TaskID: first, HandlerID: "job-1"}}, f.tasks.cancelled)
}

func TestExtensionTaskHandlerPanicsAreRecovered(t *testing.T) {
	for _, method := range extensionTaskMethods {
		t.Run(string(method), func(t *testing.T) {
			f := newTaskFixture(t, true)
			taskID := f.handOver(t, t.Context(), "job-1")
			f.tasks.mu.Lock()
			f.tasks.panicWith = "boom"
			f.tasks.mu.Unlock()
			rpcErr := rpcErrorOf(t, sendTaskMethod(t, t.Context(), f.srv, method, taskMethodParams(method, taskID), declaringTasks()))
			assert.InDelta(t, mcp.INTERNAL_ERROR, rpcErr["code"], 0)
			assert.Equal(t, "internal error", rpcErr["message"])
		})
	}
}

func TestExtensionTaskMethodsFireOnlyUntypedHooks(t *testing.T) {
	var typed, before, succeeded []string
	hooks := &Hooks{}
	hooks.AddBeforeGetTask(func(context.Context, any, *mcp.GetTaskRequest) { typed = append(typed, "before tasks/get") })
	hooks.AddAfterGetTask(func(context.Context, any, *mcp.GetTaskRequest, *mcp.GetTaskResult) {
		typed = append(typed, "after tasks/get")
	})
	hooks.AddBeforeCancelTask(func(context.Context, any, *mcp.CancelTaskRequest) { typed = append(typed, "before tasks/cancel") })
	hooks.AddAfterCancelTask(func(context.Context, any, *mcp.CancelTaskRequest, *mcp.CancelTaskResult) {
		typed = append(typed, "after tasks/cancel")
	})
	hooks.AddBeforeAny(func(_ context.Context, _ any, method mcp.MCPMethod, _ any) { before = append(before, string(method)) })
	hooks.AddOnSuccess(func(_ context.Context, _ any, method mcp.MCPMethod, _ any, _ any) {
		succeeded = append(succeeded, string(method))
	})
	f := newTaskFixture(t, true, WithHooks(hooks), WithTaskCapabilities(true, true, true))
	taskID := f.handOver(t, t.Context(), "job-1")
	before, succeeded = nil, nil

	for _, method := range extensionTaskMethods {
		completeResult(t, sendTaskMethod(t, t.Context(), f.srv, method, taskMethodParams(method, taskID), declaringTasks()))
	}
	assert.Empty(t, typed)
	assert.Equal(t, []string{"tasks/get", "tasks/cancel", "tasks/update"}, before)
	assert.Equal(t, []string{"tasks/get", "tasks/cancel", "tasks/update"}, succeeded)

	t.Run("a legacy request keeps its typed hooks", func(t *testing.T) {
		f.srv.HandleMessage(t.Context(), json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tasks/get","params":{"taskId":"legacy"}}`))
		assert.Equal(t, []string{"before tasks/get"}, typed)
	})
}

func TestTaskMethodsContextCarriesRequestInfo(t *testing.T) {
	type httpKey struct{}
	type seen struct {
		modern   bool
		fromHTTP any
	}
	f := newTaskFixture(t, true)
	calls := map[string]seen{}
	f.tasks.onContext = func(method string, ctx context.Context) {
		info := RequestProtocolInfoFromContext(ctx)
		calls[method] = seen{modern: info != nil && info.Modern, fromHTTP: ctx.Value(httpKey{})}
	}
	taskID, err := f.srv.taskSealer.seal("job-1", "")
	require.NoError(t, err)
	f.tasks.set("job-1", func(*TaskState) {})
	httpServer := httptest.NewServer(NewStreamableHTTPServer(f.srv, WithHTTPContextFunc(func(ctx context.Context, _ *http.Request) context.Context {
		return context.WithValue(ctx, httpKey{}, "from-http")
	})))
	t.Cleanup(httpServer.Close)

	meta := modernMeta()
	meta[mcp.MetaKeyClientCapabilities] = declaringTasks()
	for _, method := range extensionTaskMethods {
		completeResult(t, decodeJSONRPC(t, postModernWithMeta(t, httpServer.URL, method, taskMethodParams(method, taskID), meta)))
	}
	f.tasks.mu.Lock()
	defer f.tasks.mu.Unlock()
	want := seen{modern: true, fromHTTP: "from-http"}
	assert.Equal(t, map[string]seen{"TaskState": want, "CancelTask": want, "UpdateTask": want}, calls,
		"each method's context carries the modern protocol info and the HTTP context")
}

func TestConcurrentGetUpdateCancelOnOneTask(t *testing.T) {
	f := newTaskFixture(t, true)
	taskID := f.handOver(t, t.Context(), "job-1")
	// Completed, so every tasks/get renders the one result the handler holds:
	// the path that must copy it rather than write it.
	f.tasks.set("job-1", func(s *TaskState) {
		s.Status = mcp.TaskStatusCompleted
		s.Result = mcp.NewToolResultText("done")
	})
	var bodies []json.RawMessage
	for _, method := range extensionTaskMethods {
		body := modernBody(t, method, taskMethodParams(method, taskID), declaringTasks())
		for range 50 {
			bodies = append(bodies, body)
		}
	}
	var wg sync.WaitGroup
	for _, body := range bodies {
		wg.Go(func() {
			message, err := answerOf(f.srv.HandleMessage(t.Context(), body))
			assert.NoError(t, err)
			assert.Contains(t, message, "result")
		})
	}
	wg.Wait()
	assert.Len(t, f.tasks.stateRefs, 50)
	assert.Len(t, f.tasks.cancelled, 50)
	assert.Len(t, f.input.updated, 50)
}

func TestModernTasksListAndResultStayNotFound(t *testing.T) {
	f := newTaskFixture(t, false, WithTaskCapabilities(true, true, true))
	for _, method := range []mcp.MCPMethod{mcp.MethodTasksList, mcp.MethodTasksResult} {
		t.Run(string(method), func(t *testing.T) {
			rpcErr := rpcErrorOf(t, sendTaskMethod(t, t.Context(), f.srv, method, map[string]any{"taskId": "abc"}, declaringTasks()))
			assert.InDelta(t, mcp.METHOD_NOT_FOUND, rpcErr["code"], 0)
			assert.Equal(t, `"`+string(method)+`" was removed in protocol version 2026-07-28`, rpcErr["message"])
		})
	}
}

// recordingHandler is a slog.Handler that keeps every record.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, record.Clone())
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

// messagesAt returns the messages of the records at level.
func (h *recordingHandler) messagesAt(level slog.Level) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var messages []string
	for _, record := range h.records {
		if record.Level == level {
			messages = append(messages, record.Message)
		}
	}
	return messages
}

func TestUnboundAuthenticatedTaskRequestWarnsOnce(t *testing.T) {
	authorized := http.Header{"Authorization": {"Bearer token"}}
	tests := []struct {
		name   string
		opts   []ServerOption
		header http.Header
		want   int
	}{
		{"unbound, with Authorization", nil, authorized, 1},
		{"bound, with Authorization", []ServerOption{WithTaskCaller(func(context.Context) string { return "alice" })}, authorized, 0},
		{"unbound, without Authorization", nil, nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := &recordingHandler{}
			f := newTaskFixture(t, false, append(tt.opts, WithLogger(slog.New(logs)))...)
			taskID := f.handOver(t, t.Context(), "job-1")
			ctx := t.Context()
			if tt.header != nil {
				ctx = context.WithValue(ctx, requestHeader, tt.header)
			}
			for range 2 {
				completeResult(t, sendTaskMethod(t, ctx, f.srv, mcp.MethodTasksGet, map[string]any{"taskId": taskID}, declaringTasks()))
			}
			warnings := logs.messagesAt(slog.LevelWarn)
			require.Len(t, warnings, tt.want)
			for _, warning := range warnings {
				for _, power := range []string{"read", "cancel", "answer its input requests"} {
					assert.Contains(t, warning, power)
				}
			}
		})
	}
}

// fixedTask is a TaskHandler that renders every task as state.
type fixedTask struct{ state TaskState }

func (h fixedTask) TaskState(context.Context, TaskRef) (TaskState, error) { return h.state, nil }

func (fixedTask) CancelTask(context.Context, TaskRef) error { return nil }

// A tasks/get of a working task, on a declared request without HTTP headers:
// 43 allocations (a declared tools/call is 40).
func TestExtensionGetTaskAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	srv := NewMCPServer("tasks", "1.0.0", WithTaskHandler(fixedTask{TaskState{
		Status:        mcp.TaskStatusWorking,
		CreatedAt:     taskCreated,
		LastUpdatedAt: taskUpdated,
		TTL:           new(time.Minute),
		PollInterval:  new(2 * time.Second),
	}}))
	taskID, err := srv.taskSealer.seal("job-1", "")
	require.NoError(t, err)
	body := modernBody(t, mcp.MethodTasksGet, map[string]any{"taskId": taskID}, declaringTasks())
	ctx := t.Context()
	message, err := answerOf(srv.HandleMessage(ctx, body))
	require.NoError(t, err)
	assert.Equal(t, "working", completeResult(t, message)["status"])
	allocs := testing.AllocsPerRun(1000, func() { srv.HandleMessage(ctx, body) })
	assert.LessOrEqual(t, allocs, 43.0, "allocations per tasks/get of a working task")
}
