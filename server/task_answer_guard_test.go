package server

import (
	"context"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// legacyTask creates a 2025-11-25 task on srv, a taskKinds server, and
// returns the answer to the tools/call that created it.
func legacyTask(t *testing.T, srv *MCPServer) map[string]any {
	t.Helper()
	return sendIn(t, srv, mcp.MethodToolsCall, map[string]any{"name": "only", "task": map[string]any{"ttl": 1000}}, legacyCall)
}

// A 2025-11-25 task answer carries no resultType, which that revision does
// not have: an after-hook that gives it one, "task" or any other, is refused
// by the outgoing check. The task itself is still created.
func TestAnAfterHookCannotGiveALegacyTaskAResultType(t *testing.T) {
	for _, resultType := range []mcp.ResultType{mcp.ResultTypeTask, mcp.ResultTypeComplete, mcp.ResultTypeInputRequired} {
		t.Run(string(resultType), func(t *testing.T) {
			hooks := &Hooks{}
			hooks.AddAfterCallTool(func(_ context.Context, _ any, _ *mcp.CallToolRequest, result any) {
				if created, ok := result.(*mcp.CreateTaskResult); ok {
					created.ResultType = resultType
				}
			})
			var ran int
			srv := taskKinds(t, &ran, WithHooks(hooks))
			assert.Equal(t, mcp.INTERNAL_ERROR, errorCode(t, legacyTask(t, srv)))
		})
	}

	t.Run("without the hook the legacy task is answered", func(t *testing.T) {
		var ran int
		answer := legacyTask(t, taskKinds(t, &ran))
		result, ok := answer["result"].(map[string]any)
		require.True(t, ok, "expected a result, got %v", answer)
		assert.NotContains(t, result, "resultType")
		assert.Contains(t, result, "task")
	})
}

// A 2025-11-25 task never answers a 2026-07-28 request: the dispatcher never
// makes one there, and the outgoing check refuses one all the same.
func TestALegacyTaskIsNeverAnsweredToAModernRequest(t *testing.T) {
	srv := taskServer(false, returningTask(&mcp.ExtensionTask{}))
	ctx := WithRequestProtocolInfo(t.Context(), &RequestProtocolInfo{Modern: true})
	reqErr := srv.guardToolCallResult(ctx, 1, &mcp.CreateTaskResult{Task: mcp.NewTask("legacy")})
	require.NotNil(t, reqErr)
	assert.Equal(t, mcp.INTERNAL_ERROR, reqErr.code)
	assert.Nil(t, srv.guardToolCallResult(t.Context(), 1, &mcp.CreateTaskResult{Task: mcp.NewTask("legacy")}), "a legacy request")
}

// Only tools/call is answered with a task (Tasks.md rows 2 and 7): a success
// hook that makes the answer of tasks/get, tasks/cancel or tasks/update a task,
// or anything but complete, is refused after it, with -32603.
func TestATaskMethodHookCannotAnswerATask(t *testing.T) {
	for _, method := range extensionTaskMethods {
		for _, resultType := range []mcp.ResultType{mcp.ResultTypeTask, mcp.ResultTypeInputRequired} {
			t.Run(string(method)+"/"+string(resultType), func(t *testing.T) {
				hooks := &Hooks{}
				hooks.AddOnSuccess(func(_ context.Context, _ any, got mcp.MCPMethod, _ any, result any) {
					if got != method {
						return
					}
					switch r := result.(type) {
					case *mcp.ExtensionTaskStateResult:
						r.ResultType = resultType
						r.TaskId = "forged-by-hook"
					case *mcp.Result:
						r.ResultType = resultType
					}
				})
				f := newTaskFixture(t, true, WithHooks(hooks))
				id := f.handOver(t, t.Context(), "job-1")
				answer := sendTaskMethod(t, t.Context(), f.srv, method, taskMethodParams(method, id), declaringTasks())
				assert.Equal(t, mcp.INTERNAL_ERROR, errorCode(t, answer))
			})
		}
	}

	t.Run("a hook that sets complete itself is answered", func(t *testing.T) {
		hooks := &Hooks{}
		hooks.AddOnSuccess(func(_ context.Context, _ any, _ mcp.MCPMethod, _ any, result any) {
			switch r := result.(type) {
			case *mcp.ExtensionTaskStateResult:
				r.ResultType = mcp.ResultTypeComplete
			case *mcp.Result:
				r.ResultType = mcp.ResultTypeComplete
			}
		})
		f := newTaskFixture(t, true, WithHooks(hooks))
		id := f.handOver(t, t.Context(), "job-1")
		for _, method := range extensionTaskMethods {
			completeResult(t, sendTaskMethod(t, t.Context(), f.srv, method, taskMethodParams(method, id), declaringTasks()))
		}
	})
}

// The result a tasks/get answer carries is never a task, nor a request for
// input, whatever a success hook puts there, and on any status.
func TestATaskGetHookCannotNestATask(t *testing.T) {
	nested := map[string]func(*mcp.CallToolResult){
		"a task": func(r *mcp.CallToolResult) {
			r.Task = &mcp.ExtensionTask{TaskId: "forged-nested-id", Status: mcp.TaskStatusWorking}
		},
		"a task result type":    func(r *mcp.CallToolResult) { r.ResultType = mcp.ResultTypeTask },
		"an input_required one": func(r *mcp.CallToolResult) { r.ResultType = mcp.ResultTypeInputRequired },
	}
	for _, status := range []mcp.TaskStatus{mcp.TaskStatusCompleted, mcp.TaskStatusWorking} {
		for name, edit := range nested {
			t.Run(string(status)+"/"+name, func(t *testing.T) {
				hooks := &Hooks{}
				hooks.AddOnSuccess(func(_ context.Context, _ any, method mcp.MCPMethod, _ any, result any) {
					if method != mcp.MethodTasksGet {
						return
					}
					state := result.(*mcp.ExtensionTaskStateResult)
					if state.TaskResult == nil {
						state.TaskResult = mcp.NewToolResultText("injected")
					}
					edit(state.TaskResult)
				})
				f := newTaskFixture(t, false, WithHooks(hooks))
				id := f.handOver(t, t.Context(), "job-1")
				if status == mcp.TaskStatusCompleted {
					f.tasks.set("job-1", func(s *TaskState) { s.Status = mcp.TaskStatusCompleted; s.Result = mcp.NewToolResultText("done") })
				}
				answer := sendTaskMethod(t, t.Context(), f.srv, mcp.MethodTasksGet, taskMethodParams(mcp.MethodTasksGet, id), declaringTasks())
				assert.Equal(t, mcp.INTERNAL_ERROR, errorCode(t, answer))
			})
		}
	}
}

// The typed hooks of the 2025-11-25 tasks/get and tasks/cancel run before the
// same check: their answers are not tasks either.
func TestALegacyTaskMethodHookCannotAnswerATask(t *testing.T) {
	for _, method := range []mcp.MCPMethod{mcp.MethodTasksGet, mcp.MethodTasksCancel} {
		t.Run(string(method), func(t *testing.T) {
			hooks := &Hooks{}
			hooks.AddAfterGetTask(func(_ context.Context, _ any, _ *mcp.GetTaskRequest, result *mcp.GetTaskResult) {
				result.ResultType = mcp.ResultTypeTask
			})
			hooks.AddAfterCancelTask(func(_ context.Context, _ any, _ *mcp.CancelTaskRequest, result *mcp.CancelTaskResult) {
				result.ResultType = mcp.ResultTypeTask
			})
			srv := NewMCPServer("legacy-era", "1.0.0", WithTaskCapabilities(true, true, true), WithHooks(hooks))
			// The task works until it is cancelled, so tasks/cancel finds it
			// still working.
			srv.AddTaskTool(mcp.NewTool("only", mcp.WithTaskSupport(mcp.TaskSupportRequired)),
				func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
					<-ctx.Done()
					return nil, ctx.Err()
				})
			created, ok := legacyTask(t, srv)["result"].(map[string]any)
			require.True(t, ok)
			taskID, ok := created["task"].(map[string]any)["taskId"].(string)
			require.True(t, ok)
			// The task's work ends with the test, past the hooks; the test's
			// context is cancelled by the time a cleanup runs.
			t.Cleanup(func() {
				_, _ = srv.handleCancelTask(context.WithoutCancel(t.Context()), 2, mcp.CancelTaskRequest{Params: mcp.CancelTaskParams{TaskId: taskID}})
			})
			answer := sendIn(t, srv, method, map[string]any{"taskId": taskID}, legacyCall)
			assert.Equal(t, mcp.INTERNAL_ERROR, errorCode(t, answer))
		})
	}
}

// The check lets every answer through that is complete, or will be stamped
// complete, in both eras, and a nil one.
func TestTheTaskMethodCheckAllowsEveryCompleteAnswer(t *testing.T) {
	srv := NewMCPServer("guard", "1.0.0")
	for _, result := range []any{
		&mcp.Result{},
		&mcp.Result{ResultType: mcp.ResultTypeComplete},
		&mcp.ExtensionTaskStateResult{TaskResult: &mcp.CallToolResult{}},
		&mcp.GetTaskResult{},
		&mcp.CancelTaskResult{},
		(*mcp.ExtensionTaskStateResult)(nil),
	} {
		assert.Nil(t, srv.guardTaskMethodResult(t.Context(), 1, result), "%#v", result)
	}
}
