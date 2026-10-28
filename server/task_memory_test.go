package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
	"weak"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memoryWork is the function a test hands to MemoryTasks.Run.
type memoryWork = func(ctx context.Context, status func(string)) (*mcp.CallToolResult, error)

// memoryFixture is a server whose tool "slow" runs work through a MemoryTasks
// it hands to WithTaskHandler. runErr is what the last Run returned.
type memoryFixture struct {
	srv    *MCPServer
	mem    *MemoryTasks
	work   memoryWork
	opts   []TaskResultOption
	runErr error
}

// newMemoryFixture builds a memoryFixture whose store is closed with t.
func newMemoryFixture(t *testing.T, memOpts []MemoryTasksOption, srvOpts ...ServerOption) *memoryFixture {
	t.Helper()
	f := &memoryFixture{mem: NewMemoryTasks(memOpts...)}
	t.Cleanup(f.mem.Close)
	f.srv = NewMCPServer("memory", "1.0.0", append([]ServerOption{WithToolCapabilities(true), WithTaskHandler(f.mem)}, srvOpts...)...)
	f.srv.AddTool(mcp.NewTool("slow"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		res, err := f.mem.Run(ctx, f.work, f.opts...)
		f.runErr = err
		return res, err
	})
	return f
}

// held reports how many workers have not returned and how many tasks the
// store keeps.
func (m *MemoryTasks) held() (running, kept int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running, len(m.jobs)
}

// gate is a channel opened once, and at the latest when the test ends, so a
// worker blocked on it never keeps a failed test from closing its store.
type gate struct {
	ch   chan struct{}
	once sync.Once
}

func newGate(t *testing.T) *gate {
	g := &gate{ch: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (g *gate) open() { g.once.Do(func() { close(g.ch) }) }

// blockUntil waits for ch or for ctx to end.
func blockUntil(ctx context.Context, ch <-chan struct{}) {
	select {
	case <-ch:
	case <-ctx.Done():
	}
}

// receive waits for ch, failing the test when it takes too long.
func receive(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a signal")
	}
}

func textWork(text string) memoryWork {
	return func(context.Context, func(string)) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText(text), nil
	}
}

func blockedWork(release *gate) memoryWork {
	return func(ctx context.Context, _ func(string)) (*mcp.CallToolResult, error) {
		blockUntil(ctx, release.ch)
		return mcp.NewToolResultText("done"), nil
	}
}

func (f *memoryFixture) handOver(t *testing.T) string {
	t.Helper()
	taskID, ok := taskResultOf(t, callToolIn(t, t.Context(), f.srv, "slow", declaredCall))["taskId"].(string)
	require.True(t, ok)
	return taskID
}

func (f *memoryFixture) get(t *testing.T, taskID string) map[string]any {
	t.Helper()
	return completeResult(t, sendTaskMethod(t, t.Context(), f.srv, mcp.MethodTasksGet, map[string]any{"taskId": taskID}, declaringTasks()))
}

func (f *memoryFixture) cancel(t *testing.T, taskID string) {
	t.Helper()
	completeResult(t, sendTaskMethod(t, t.Context(), f.srv, mcp.MethodTasksCancel, map[string]any{"taskId": taskID}, declaringTasks()))
}

// pollTask is tasks/get without a *testing.T, for conditions that run on
// another goroutine; nil when the answer cannot be read.
func (f *memoryFixture) pollTask(taskID string) map[string]any {
	meta := modernMeta()
	meta[mcp.MetaKeyClientCapabilities] = declaringTasks()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": mcp.MethodTasksGet, "params": map[string]any{"taskId": taskID, "_meta": meta}})
	if err != nil {
		return nil
	}
	message, err := answerOf(f.srv.HandleMessage(context.Background(), body))
	if err != nil {
		return nil
	}
	return message
}

// statusOf is the status tasks/get answers, "" for an error.
func (f *memoryFixture) statusOf(taskID string) string {
	result, _ := f.pollTask(taskID)["result"].(map[string]any)
	status, _ := result["status"].(string)
	return status
}

// isGone reports whether tasks/get answers that the task is gone.
func (f *memoryFixture) isGone(taskID string) bool {
	rpcErr, _ := f.pollTask(taskID)["error"].(map[string]any)
	data, _ := rpcErr["data"].(map[string]any)
	return data["reason"] == "gone"
}

func (f *memoryFixture) waitForStatus(t *testing.T, taskID, status string) {
	t.Helper()
	require.Eventually(t, func() bool { return f.statusOf(taskID) == status }, 5*time.Second, time.Millisecond, "status %q", status)
}

func (f *memoryFixture) waitForIdle(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		running, _ := f.mem.held()
		return running == 0
	}, 5*time.Second, time.Millisecond, "the workers did not return")
}

// taskState asks the store itself for the task's state.
func (f *memoryFixture) taskState(t *testing.T, taskID string) TaskState {
	t.Helper()
	handlerID, err := f.srv.taskSealer.open(taskID, "")
	require.NoError(t, err)
	state, err := f.mem.TaskState(t.Context(), TaskRef{TaskID: taskID, HandlerID: handlerID})
	require.NoError(t, err)
	return state
}

func TestMemoryTasksRunsSynchronouslyWhenNoTaskIsPossible(t *testing.T) {
	tests := []struct {
		name           string
		withoutHandler bool
		era            callEra
	}{
		{"an undeclared request", false, undeclaredCall},
		{"a legacy request", false, legacyCall},
		{"a server with no task handler", true, declaredCall},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mem := NewMemoryTasks()
			t.Cleanup(mem.Close)
			calls := 0
			srv := taskServer(tt.withoutHandler, func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mem.Run(ctx, func(runCtx context.Context, status func(string)) (*mcp.CallToolResult, error) {
					calls++
					status("not kept")
					assert.True(t, runCtx == ctx, "the work runs under the request's context")
					return mcp.NewToolResultText("sync"), nil
				})
			})
			result, ok := callToolIn(t, t.Context(), srv, "slow", tt.era)["result"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, []any{map[string]any{"type": "text", "text": "sync"}}, result["content"])
			assert.NotEqual(t, "task", result["resultType"])
			assert.Equal(t, 1, calls)
			running, kept := mem.held()
			assert.Zero(t, running)
			assert.Zero(t, kept, "a synchronous call leaves no task behind")
		})
	}

	t.Run("a context that is not a tool call's is an error", func(t *testing.T) {
		mem := NewMemoryTasks()
		t.Cleanup(mem.Close)
		res, err := mem.Run(t.Context(), func(context.Context, func(string)) (*mcp.CallToolResult, error) {
			t.Error("the work ran")
			return nil, nil
		})
		assert.Nil(t, res)
		assert.ErrorIs(t, err, ErrNotToolCall)
	})
}

func TestMemoryTasksHandsOverAndCompletes(t *testing.T) {
	f := newMemoryFixture(t, nil)
	release := newGate(t)
	f.work = blockedWork(release)

	answer := taskResultOf(t, callToolIn(t, t.Context(), f.srv, "slow", declaredCall))
	taskID, ok := answer["taskId"].(string)
	require.True(t, ok)
	assert.Equal(t, "working", answer["status"])
	handlerID, err := f.srv.taskSealer.open(taskID, "")
	require.NoError(t, err)
	assert.Regexp(t, `^[0-9a-f]{32}$`, handlerID, "a handler id is 16 random bytes in hex")

	working := f.get(t, taskID)
	assert.Equal(t, "working", working["status"])
	assert.NotContains(t, working, "result")
	state := f.taskState(t, taskID)
	assert.Equal(t, "slow", state.Origin.Tool())
	assert.Equal(t, mcp.TaskStatusWorking, state.Status)

	release.open()
	f.waitForStatus(t, taskID, "completed")
	done := f.get(t, taskID)
	assert.Equal(t, map[string]any{
		"content": []any{map[string]any{"type": "text", "text": "done"}}, "resultType": "complete",
	}, done["result"])
	assert.Equal(t, working["createdAt"], done["createdAt"])
	assert.Equal(t, working["ttlMs"], done["ttlMs"])
	assert.Equal(t, "slow", f.taskState(t, taskID).Origin.Tool())

	f.waitForIdle(t)
	other := f.handOver(t)
	assert.NotEqual(t, taskID, other)
	otherHandlerID, err := f.srv.taskSealer.open(other, "")
	require.NoError(t, err)
	assert.NotEqual(t, handlerID, otherHandlerID)
}

func TestMemoryTasksStatusMessage(t *testing.T) {
	f := newMemoryFixture(t, nil)
	release := newGate(t)
	reported := make(chan struct{})
	f.opts = []TaskResultOption{WithTaskResultStatusMessage("queued")}
	f.work = func(ctx context.Context, status func(string)) (*mcp.CallToolResult, error) {
		status("halfway")
		close(reported)
		blockUntil(ctx, release.ch)
		return mcp.NewToolResultText("done"), nil
	}

	answer := taskResultOf(t, callToolIn(t, t.Context(), f.srv, "slow", declaredCall))
	assert.Equal(t, "queued", answer["statusMessage"])
	taskID, ok := answer["taskId"].(string)
	require.True(t, ok)

	receive(t, reported)
	assert.Equal(t, "halfway", f.get(t, taskID)["statusMessage"])

	release.open()
	f.waitForStatus(t, taskID, "completed")
	assert.NotContains(t, f.get(t, taskID), "statusMessage", "a progress message ends with the work")
}

func TestMemoryTasksPanicFails(t *testing.T) {
	f := newMemoryFixture(t, nil)
	f.work = func(context.Context, func(string)) (*mcp.CallToolResult, error) {
		panic("boom at /srv/secret")
	}
	taskID := f.handOver(t)
	f.waitForStatus(t, taskID, "failed")

	got := f.get(t, taskID)
	assert.Equal(t, map[string]any{"code": float64(mcp.INTERNAL_ERROR), "message": "internal error"}, got["error"])
	state := f.taskState(t, taskID)
	assert.Equal(t, mcp.TaskStatusFailed, state.Status)
	require.Error(t, state.Err)
	assert.Contains(t, state.Err.Error(), "boom at /srv/secret")
	f.waitForIdle(t)
}

func TestMemoryTasksFailsWhenTheWorkFails(t *testing.T) {
	tests := []struct {
		name string
		work memoryWork
		want string
	}{
		{"an error", func(context.Context, func(string)) (*mcp.CallToolResult, error) {
			return nil, errors.New("disk full")
		}, "disk full"},
		{"neither a result nor an error", func(context.Context, func(string)) (*mcp.CallToolResult, error) {
			return nil, nil
		}, "neither"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newMemoryFixture(t, nil)
			f.work = tt.work
			taskID := f.handOver(t)
			f.waitForStatus(t, taskID, "failed")
			state := f.taskState(t, taskID)
			require.Error(t, state.Err)
			assert.Contains(t, state.Err.Error(), tt.want)
			assert.Nil(t, state.Result)
		})
	}
}

func TestMemoryTasksCancel(t *testing.T) {
	f := newMemoryFixture(t, []MemoryTasksOption{WithMemoryTasksMaxRunning(1)})
	finish := newGate(t)
	started, sawCancel := make(chan struct{}), make(chan struct{})
	f.work = func(ctx context.Context, _ func(string)) (*mcp.CallToolResult, error) {
		close(started)
		<-ctx.Done()
		close(sawCancel)
		<-finish.ch
		return nil, ctx.Err()
	}
	taskID := f.handOver(t)
	receive(t, started)

	f.cancel(t, taskID)
	receive(t, sawCancel)
	cancelled := f.get(t, taskID)
	assert.Equal(t, "cancelled", cancelled["status"])
	assert.NotContains(t, cancelled, "result")
	assert.NotContains(t, cancelled, "error")

	running, _ := f.mem.held()
	assert.Equal(t, 1, running, "the count drops when the worker returns, not when cancel is asked")
	callToolIn(t, t.Context(), f.srv, "slow", declaredCall)
	assert.ErrorIs(t, f.runErr, ErrTooManyRunningTasks, "a worker still running holds its slot")

	finish.open()
	f.waitForIdle(t)
	assert.Equal(t, "cancelled", f.get(t, taskID)["status"], "the worker's error does not change a cancelled task")
	f.work = textWork("next")
	f.waitForStatus(t, f.handOver(t), "completed")
}

func TestMemoryTasksCancelOfATerminalTaskChangesNothing(t *testing.T) {
	tests := []struct {
		name        string
		work        memoryWork
		cancelFirst bool
		status      string
	}{
		{"completed", textWork("done"), false, "completed"},
		{"failed", func(context.Context, func(string)) (*mcp.CallToolResult, error) {
			return nil, errors.New("disk full")
		}, false, "failed"},
		{"cancelled", func(ctx context.Context, _ func(string)) (*mcp.CallToolResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}, true, "cancelled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newMemoryFixture(t, nil)
			f.work = tt.work
			taskID := f.handOver(t)
			if tt.cancelFirst {
				f.cancel(t, taskID)
			}
			f.waitForStatus(t, taskID, tt.status)
			f.waitForIdle(t)
			before := f.get(t, taskID)

			f.cancel(t, taskID)
			assert.Equal(t, before, f.get(t, taskID))
		})
	}
}

func TestMemoryTasksMaxRunning(t *testing.T) {
	f := newMemoryFixture(t, []MemoryTasksOption{WithMemoryTasksMaxRunning(2)})
	release := newGate(t)
	f.work = blockedWork(release)
	f.handOver(t)
	f.handOver(t)

	calls := 0
	f.work = func(context.Context, func(string)) (*mcp.CallToolResult, error) {
		calls++
		return mcp.NewToolResultText("sync"), nil
	}
	assert.Equal(t, mcp.INTERNAL_ERROR, errorCode(t, callToolIn(t, t.Context(), f.srv, "slow", declaredCall)))
	assert.ErrorIs(t, f.runErr, ErrTooManyRunningTasks)
	assert.Zero(t, calls, "a refused call runs nothing")

	legacy, ok := callToolIn(t, t.Context(), f.srv, "slow", legacyCall)["result"].(map[string]any)
	require.True(t, ok, "a call that cannot become a task runs at the limit")
	assert.Equal(t, []any{map[string]any{"type": "text", "text": "sync"}}, legacy["content"])
	assert.Equal(t, 1, calls)

	release.open()
	f.waitForIdle(t)
	f.work = textWork("done")
	f.waitForStatus(t, f.handOver(t), "completed")
}

func TestMemoryTasksEvictsEndedByCountAndTTL(t *testing.T) {
	t.Run("by count, oldest first, never a running task", func(t *testing.T) {
		f := newMemoryFixture(t, []MemoryTasksOption{WithMemoryTasksMaxEnded(2)})
		release := newGate(t)
		f.work = blockedWork(release)
		running := f.handOver(t)

		f.work = textWork("done")
		var ended []string
		for range 3 {
			taskID := f.handOver(t)
			f.waitForStatus(t, taskID, "completed")
			ended = append(ended, taskID)
		}
		assert.True(t, f.isGone(ended[0]), "the oldest ended task is evicted")
		assert.Equal(t, "completed", f.statusOf(ended[1]))
		assert.Equal(t, "completed", f.statusOf(ended[2]))
		assert.Equal(t, "working", f.statusOf(running))
	})

	t.Run("by TTL", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newMemoryFixture(t, nil)
			f.opts = []TaskResultOption{WithTaskResultTTL(time.Minute)}
			f.work = textWork("done")
			taskID := f.handOver(t)
			synctest.Wait()
			require.Equal(t, "completed", f.statusOf(taskID))

			time.Sleep(59 * time.Second)
			assert.Equal(t, "completed", f.statusOf(taskID))
			time.Sleep(2 * time.Second)
			assert.True(t, f.isGone(taskID))
			_, kept := f.mem.held()
			assert.Zero(t, kept)
		})
	})

	t.Run("by TTL, a running task is cancelled and forgotten", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newMemoryFixture(t, nil)
			f.opts = []TaskResultOption{WithTaskResultTTL(time.Minute)}
			sawCancel := make(chan struct{})
			f.work = func(ctx context.Context, _ func(string)) (*mcp.CallToolResult, error) {
				<-ctx.Done()
				close(sawCancel)
				return nil, ctx.Err()
			}
			taskID := f.handOver(t)
			synctest.Wait()
			require.Equal(t, "working", f.statusOf(taskID))

			time.Sleep(61 * time.Second)
			synctest.Wait()
			select {
			case <-sawCancel:
			default:
				t.Error("the worker was not cancelled at its TTL")
			}
			assert.True(t, f.isGone(taskID))
			running, kept := f.mem.held()
			assert.Zero(t, running)
			assert.Zero(t, kept)
		})
	})

	t.Run("by TTL, the default is one hour", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newMemoryFixture(t, nil)
			f.work = textWork("done")
			taskID := f.handOver(t)
			synctest.Wait()

			time.Sleep(59 * time.Minute)
			assert.Equal(t, "completed", f.statusOf(taskID))
			time.Sleep(2 * time.Minute)
			assert.True(t, f.isGone(taskID))
		})
	})
}

func TestMemoryTasksReportsItsTTL(t *testing.T) {
	start := time.Now().Add(-10 * time.Minute)
	tests := []struct {
		name string
		opts []TaskResultOption
		want int64
	}{
		{"no TTL given", nil, 3_600_000},
		{"a TTL given", []TaskResultOption{WithTaskResultTTL(90 * time.Second)}, 90_000},
		{"a creation time given", []TaskResultOption{WithTaskResultCreatedAt(start)}, 3_600_000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newMemoryFixture(t, nil)
			f.opts = tt.opts
			f.work = textWork("done")
			answer := taskResultOf(t, callToolIn(t, t.Context(), f.srv, "slow", declaredCall))
			assert.InDelta(t, tt.want, answer["ttlMs"], 0)
			taskID, ok := answer["taskId"].(string)
			require.True(t, ok)

			f.waitForStatus(t, taskID, "completed")
			got := f.get(t, taskID)
			assert.InDelta(t, tt.want, got["ttlMs"], 0, "tasks/get reports the TTL the hand-over did")
			assert.Equal(t, answer["createdAt"], got["createdAt"])
			state := f.taskState(t, taskID)
			require.NotNil(t, state.TTL)
			assert.Equal(t, time.Duration(tt.want)*time.Millisecond, *state.TTL)
			assert.Equal(t, answer["createdAt"], taskTimestamp(state.CreatedAt))
		})
	}
}

// R1: the ids of a restarted store are gone, never another task's, even when
// the configured key lets the new server verify them.
func TestMemoryTasksRestartUnderAConfiguredKeyIsGone(t *testing.T) {
	key := bytes.Repeat([]byte{7}, 32)
	a := newMemoryFixture(t, nil, WithTaskKey(key))
	a.work = textWork("a")
	oldID := a.handOver(t)
	a.waitForStatus(t, oldID, "completed")

	b := newMemoryFixture(t, nil, WithTaskKey(key))
	b.work = textWork("b")
	for _, method := range extensionTaskMethods {
		rpcErr := rpcErrorOf(t, sendTaskMethod(t, t.Context(), b.srv, method, taskMethodParams(method, oldID), declaringTasks()))
		assert.InDelta(t, mcp.INVALID_PARAMS, rpcErr["code"], 0, string(method))
		assert.Equal(t, map[string]any{"reason": "gone"}, rpcErr["data"], string(method))
	}

	newID := b.handOver(t)
	assert.NotEqual(t, oldID, newID)
	b.waitForStatus(t, newID, "completed")
	assert.True(t, b.isGone(oldID))
	assert.Equal(t, "completed", a.statusOf(oldID), "the first store still knows its own task")
}

func TestMemoryTasksClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newMemoryFixture(t, nil)
		proceed := newGate(t)
		var exited atomic.Bool
		f.work = func(ctx context.Context, _ func(string)) (*mcp.CallToolResult, error) {
			<-ctx.Done()
			<-proceed.ch
			exited.Store(true)
			return nil, ctx.Err()
		}
		taskID := f.handOver(t)
		synctest.Wait()

		closed := make(chan struct{})
		go func() {
			f.mem.Close()
			close(closed)
		}()
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("Close returned before the worker did")
		default:
		}
		proceed.open()
		synctest.Wait()
		select {
		case <-closed:
		default:
			t.Fatal("Close did not return once the worker did")
		}
		assert.True(t, exited.Load())

		running, kept := f.mem.held()
		assert.Zero(t, running)
		assert.Zero(t, kept)
		assert.True(t, f.isGone(taskID))

		assert.Equal(t, mcp.INTERNAL_ERROR, errorCode(t, callToolIn(t, t.Context(), f.srv, "slow", declaredCall)))
		assert.ErrorIs(t, f.runErr, ErrMemoryTasksClosed)
		f.work = textWork("sync")
		_, ok := callToolIn(t, t.Context(), f.srv, "slow", legacyCall)["result"].(map[string]any)
		assert.True(t, ok, "a call that cannot become a task still runs")
		f.mem.Close()
	})
}

func TestMemoryTasksSurvivesTheRequestContext(t *testing.T) {
	type valueKey struct{}
	f := newMemoryFixture(t, nil)
	release := newGate(t)
	f.work = func(ctx context.Context, _ func(string)) (*mcp.CallToolResult, error) {
		blockUntil(ctx, release.ch)
		return mcp.NewToolResultText(fmt.Sprintf("%v|%v", ctx.Err(), ctx.Value(valueKey{}))), nil
	}
	reqCtx, cancel := context.WithCancel(context.WithValue(t.Context(), valueKey{}, "kept"))
	defer cancel()
	taskID, ok := taskResultOf(t, callToolIn(t, reqCtx, f.srv, "slow", declaredCall))["taskId"].(string)
	require.True(t, ok)

	cancel()
	release.open()
	f.waitForStatus(t, taskID, "completed")
	result, ok := f.get(t, taskID)["result"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []any{map[string]any{"type": "text", "text": "<nil>|kept"}}, result["content"],
		"the worker keeps the request's values, not its cancellation")
}

func TestMemoryTasksConcurrentUse(t *testing.T) {
	f := newMemoryFixture(t, nil)
	release := newGate(t)
	f.work = func(ctx context.Context, status func(string)) (*mcp.CallToolResult, error) {
		status("working")
		blockUntil(ctx, release.ch)
		return mcp.NewToolResultText("done"), nil
	}
	refs := make([]TaskRef, 16)
	for i := range refs {
		taskID := f.handOver(t)
		handlerID, err := f.srv.taskSealer.open(taskID, "")
		require.NoError(t, err)
		refs[i] = TaskRef{TaskID: taskID, HandlerID: handlerID}
	}

	var wg sync.WaitGroup
	for i, ref := range refs {
		for range 3 {
			wg.Go(func() { _, _ = f.mem.TaskState(t.Context(), ref) })
		}
		if i%2 == 0 {
			wg.Go(func() { assert.NoError(t, f.mem.CancelTask(t.Context(), ref)) })
		}
	}
	wg.Go(release.open)
	wg.Wait()
	f.waitForIdle(t)

	for i, ref := range refs {
		state, err := f.mem.TaskState(t.Context(), ref)
		require.NoError(t, err)
		if i%2 == 0 {
			assert.Contains(t, []mcp.TaskStatus{mcp.TaskStatusCompleted, mcp.TaskStatusCancelled}, state.Status)
		} else {
			assert.Equal(t, mcp.TaskStatusCompleted, state.Status)
		}
	}
}

func TestMemoryTasksOptionsRefuseANonPositiveLimit(t *testing.T) {
	assert.Panics(t, func() { WithMemoryTasksMaxRunning(0) })
	assert.Panics(t, func() { WithMemoryTasksMaxEnded(0) })
	assert.NotPanics(t, func() { WithMemoryTasksMaxRunning(1) })
	assert.NotPanics(t, func() { WithMemoryTasksMaxEnded(1) })
}

// The TTL counts from the creation time the client was told, whatever the
// hand-over time: a creation in the future or in the past moves the eviction.
func TestMemoryTasksTTLCountsFromTheReportedCreation(t *testing.T) {
	tests := []struct {
		name          string
		createdAfter  time.Duration
		ttl           time.Duration
		alive, buried time.Duration // since the hand-over
	}{
		{"a creation in the future", 10 * time.Minute, time.Minute, 10*time.Minute + 59*time.Second, 11*time.Minute + time.Second},
		{"a creation in the past", -50 * time.Minute, time.Hour, 9*time.Minute + 59*time.Second, 10*time.Minute + time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newMemoryFixture(t, nil)
				f.opts = []TaskResultOption{WithTaskResultTTL(tt.ttl), WithTaskResultCreatedAt(time.Now().Add(tt.createdAfter))}
				f.work = textWork("done")
				taskID := f.handOver(t)
				synctest.Wait()

				time.Sleep(tt.alive)
				assert.Equal(t, "completed", f.statusOf(taskID), "the task is kept until createdAt + ttl")
				time.Sleep(tt.buried - tt.alive)
				assert.True(t, f.isGone(taskID), "the task is forgotten at createdAt + ttl")
			})
		})
	}
}

func TestMemoryTasksLastUpdatedAt(t *testing.T) {
	t.Run("moves with a status message and with the end", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newMemoryFixture(t, nil)
			finish := newGate(t)
			f.work = func(ctx context.Context, status func(string)) (*mcp.CallToolResult, error) {
				time.Sleep(time.Minute)
				status("halfway")
				blockUntil(ctx, finish.ch)
				return mcp.NewToolResultText("done"), nil
			}
			taskID := f.handOver(t)
			created := f.taskState(t, taskID).CreatedAt
			synctest.Wait()
			assert.WithinDuration(t, created, f.taskState(t, taskID).LastUpdatedAt, 0)

			time.Sleep(time.Minute)
			synctest.Wait()
			assert.WithinDuration(t, created.Add(time.Minute), f.taskState(t, taskID).LastUpdatedAt, 0, "a status message is an update")

			time.Sleep(time.Minute)
			finish.open()
			synctest.Wait()
			state := f.taskState(t, taskID)
			require.Equal(t, mcp.TaskStatusCompleted, state.Status)
			assert.WithinDuration(t, created.Add(2*time.Minute), state.LastUpdatedAt, 0, "the end is an update")
		})
	})

	t.Run("never precedes a creation in the future", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newMemoryFixture(t, nil)
			finish := newGate(t)
			f.opts = []TaskResultOption{WithTaskResultCreatedAt(time.Now().Add(10 * time.Minute))}
			f.work = func(ctx context.Context, status func(string)) (*mcp.CallToolResult, error) {
				status("halfway")
				blockUntil(ctx, finish.ch)
				return mcp.NewToolResultText("done"), nil
			}
			taskID := f.handOver(t)
			synctest.Wait()
			state := f.taskState(t, taskID)
			assert.Equal(t, "halfway", state.StatusMessage)
			assert.False(t, state.LastUpdatedAt.Before(state.CreatedAt), "a status message moved lastUpdatedAt before createdAt")

			finish.open()
			synctest.Wait()
			state = f.taskState(t, taskID)
			require.Equal(t, mcp.TaskStatusCompleted, state.Status)
			assert.False(t, state.LastUpdatedAt.Before(state.CreatedAt), "the end moved lastUpdatedAt before createdAt")
		})
	})
}

// A worker that returns after its task expired is not an ended task: it must
// not push a live one out of the ended list.
func TestMemoryTasksAnExpiredWorkerLeavesNoGhost(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newMemoryFixture(t, []MemoryTasksOption{WithMemoryTasksMaxEnded(1)})
		release := newGate(t)
		f.opts = []TaskResultOption{WithTaskResultTTL(time.Minute)}
		f.work = func(context.Context, func(string)) (*mcp.CallToolResult, error) {
			<-release.ch
			return mcp.NewToolResultText("late"), nil
		}
		expired := f.handOver(t)
		f.opts = []TaskResultOption{WithTaskResultTTL(time.Hour)}
		f.work = textWork("done")
		kept := f.handOver(t)
		synctest.Wait()
		require.Equal(t, "completed", f.statusOf(kept))

		time.Sleep(time.Minute + time.Second)
		require.True(t, f.isGone(expired))
		release.open()
		synctest.Wait()

		assert.Equal(t, "completed", f.statusOf(kept), "the late worker evicted a live ended task")
		assert.True(t, f.isGone(expired))
		running, _ := f.mem.held()
		assert.Zero(t, running)
	})
}

func TestMemoryTasksACancelledWorkerCannotChangeItsTask(t *testing.T) {
	f := newMemoryFixture(t, nil)
	finish := newGate(t)
	started, reported := make(chan struct{}), make(chan struct{})
	f.work = func(ctx context.Context, status func(string)) (*mcp.CallToolResult, error) {
		close(started)
		<-ctx.Done()
		status("stopping")
		close(reported)
		<-finish.ch
		return nil, ctx.Err()
	}
	taskID := f.handOver(t)
	receive(t, started)
	f.cancel(t, taskID)
	receive(t, reported)

	got := f.get(t, taskID)
	assert.Equal(t, "cancelled", got["status"])
	assert.NotContains(t, got, "statusMessage", "a cancelled worker reported progress on an ended task")
}

func TestMemoryTasksFailsWhenTheWorkExitsItsGoroutine(t *testing.T) {
	f := newMemoryFixture(t, nil)
	f.work = func(context.Context, func(string)) (*mcp.CallToolResult, error) {
		runtime.Goexit()
		return mcp.NewToolResultText("unreachable"), nil
	}
	taskID := f.handOver(t)
	f.waitForStatus(t, taskID, "failed")
	state := f.taskState(t, taskID)
	require.Error(t, state.Err)
	assert.Contains(t, state.Err.Error(), "ended its goroutine")
	f.waitForIdle(t)
}

func TestMemoryTasksANestedRunInsideTheWorkRunsSynchronously(t *testing.T) {
	f := newMemoryFixture(t, nil)
	innerCalls := 0
	var innerOrigin TaskOrigin
	f.work = func(ctx context.Context, _ func(string)) (*mcp.CallToolResult, error) {
		return f.mem.Run(ctx, func(innerCtx context.Context, _ func(string)) (*mcp.CallToolResult, error) {
			innerCalls++
			innerOrigin = TaskOriginFromContext(innerCtx)
			return mcp.NewToolResultText("inner"), nil
		})
	}
	taskID := f.handOver(t)
	f.waitForStatus(t, taskID, "completed")

	result, ok := f.get(t, taskID)["result"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []any{map[string]any{"type": "text", "text": "inner"}}, result["content"])
	assert.Equal(t, 1, innerCalls)
	assert.True(t, innerOrigin.IsZero())
	f.waitForIdle(t)
	_, kept := f.mem.held()
	assert.Equal(t, 1, kept, "the nested call made no task of its own")
}

// An ended task is kept for polling, but nothing of the request it was handed
// over from: not its context, nor what that context holds.
func TestMemoryTasksAnEndedTaskKeepsNothingOfTheRequest(t *testing.T) {
	type valueKey struct{}
	f := newMemoryFixture(t, nil)
	f.work = textWork("done")
	handOver := func() (string, weak.Pointer[[1024]byte]) {
		held := new([1024]byte)
		ctx := context.WithValue(t.Context(), valueKey{}, held)
		taskID, ok := taskResultOf(t, callToolIn(t, ctx, f.srv, "slow", declaredCall))["taskId"].(string)
		require.True(t, ok)
		return taskID, weak.Make(held)
	}
	taskID, request := handOver()
	f.waitForStatus(t, taskID, "completed")
	f.waitForIdle(t)

	for range 3 {
		runtime.GC()
	}
	assert.True(t, request.Value() == nil, "the ended task keeps the request's context")
	assert.Equal(t, "completed", f.statusOf(taskID), "the task itself is kept")
}
