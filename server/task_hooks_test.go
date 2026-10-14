package server

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskHooks_TaskCreated(t *testing.T) {
	var mu sync.Mutex
	var capturedMetrics []TaskMetrics
	done := make(chan struct{})

	hooks := &TaskHooks{}
	hooks.AddOnTaskCreated(func(ctx context.Context, metrics TaskMetrics) {
		mu.Lock()
		defer mu.Unlock()
		capturedMetrics = append(capturedMetrics, metrics)
		close(done)
	})

	server := NewMCPServer("test-server", "1.0.0", WithTaskHooks(hooks))

	// Create a task
	ctx := t.Context()
	_, err := server.createTask(ctx, "test-task-1", "test-tool", nil, nil)
	require.NoError(t, err)

	// Wait for hook to execute with timeout
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for OnTaskCreated hook")
	}

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, capturedMetrics, 1)
	assert.Equal(t, "test-task-1", capturedMetrics[0].TaskID)
	assert.Equal(t, "test-tool", capturedMetrics[0].ToolName)
	assert.Equal(t, mcp.TaskStatusWorking, capturedMetrics[0].Status)
	assert.NotZero(t, capturedMetrics[0].CreatedAt)
}

func TestTaskHooks_TaskCompleted(t *testing.T) {
	var mu sync.Mutex
	var completedMetrics []TaskMetrics
	done := make(chan struct{})

	hooks := &TaskHooks{}
	hooks.AddOnTaskCompleted(func(ctx context.Context, metrics TaskMetrics) {
		mu.Lock()
		defer mu.Unlock()
		completedMetrics = append(completedMetrics, metrics)
		close(done)
	})

	server := NewMCPServer("test-server", "1.0.0", WithTaskHooks(hooks))

	// Create and complete a task
	ctx := t.Context()
	entry, err := server.createTask(ctx, "test-task-2", "test-tool", nil, nil)
	require.NoError(t, err)

	// Complete the task successfully
	server.completeTask(entry, "result", nil)

	// Wait for hook to execute with timeout
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for OnTaskCompleted hook")
	}

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, completedMetrics, 1)
	assert.Equal(t, "test-task-2", completedMetrics[0].TaskID)
	assert.Equal(t, "test-tool", completedMetrics[0].ToolName)
	assert.Equal(t, mcp.TaskStatusCompleted, completedMetrics[0].Status)
	require.NotNil(t, completedMetrics[0].CompletedAt)
	assert.Equal(t, completedMetrics[0].CompletedAt.Sub(completedMetrics[0].CreatedAt), completedMetrics[0].Duration)
	assert.Nil(t, completedMetrics[0].Error)
}

func TestTaskHooks_TaskFailed(t *testing.T) {
	var mu sync.Mutex
	var failedMetrics []TaskMetrics
	done := make(chan struct{})

	hooks := &TaskHooks{}
	hooks.AddOnTaskFailed(func(ctx context.Context, metrics TaskMetrics) {
		mu.Lock()
		defer mu.Unlock()
		failedMetrics = append(failedMetrics, metrics)
		close(done)
	})

	server := NewMCPServer("test-server", "1.0.0", WithTaskHooks(hooks))

	// Create and fail a task
	ctx := t.Context()
	entry, err := server.createTask(ctx, "test-task-3", "test-tool", nil, nil)
	require.NoError(t, err)

	// Fail the task
	testErr := errors.New("task failed")
	server.completeTask(entry, nil, testErr)

	// Wait for hook to execute with timeout
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for OnTaskFailed hook")
	}

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, failedMetrics, 1)
	assert.Equal(t, "test-task-3", failedMetrics[0].TaskID)
	assert.Equal(t, "test-tool", failedMetrics[0].ToolName)
	assert.Equal(t, mcp.TaskStatusFailed, failedMetrics[0].Status)
	assert.Equal(t, "task failed", failedMetrics[0].StatusMessage)
	require.NotNil(t, failedMetrics[0].CompletedAt)
	assert.Equal(t, failedMetrics[0].CompletedAt.Sub(failedMetrics[0].CreatedAt), failedMetrics[0].Duration)
	assert.Equal(t, testErr, failedMetrics[0].Error)
}

func TestTaskHooks_TaskCancelled(t *testing.T) {
	var mu sync.Mutex
	var cancelledMetrics []TaskMetrics
	done := make(chan struct{})

	hooks := &TaskHooks{}
	hooks.AddOnTaskCancelled(func(ctx context.Context, metrics TaskMetrics) {
		mu.Lock()
		defer mu.Unlock()
		cancelledMetrics = append(cancelledMetrics, metrics)
		close(done)
	})

	server := NewMCPServer("test-server", "1.0.0", WithTaskHooks(hooks))

	// Create a task
	ctx := t.Context()
	entry, err := server.createTask(ctx, "test-task-4", "test-tool", nil, nil)
	require.NoError(t, err)

	// Add cancel function
	cancelCtx, cancel := context.WithCancel(ctx)
	server.tasksMu.Lock()
	entry.cancelFunc = cancel
	server.tasksMu.Unlock()

	// Cancel the task
	err = server.cancelTask(cancelCtx, "test-task-4")
	require.NoError(t, err)

	// Wait for hook to execute with timeout
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for OnTaskCancelled hook")
	}

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, cancelledMetrics, 1)
	assert.Equal(t, "test-task-4", cancelledMetrics[0].TaskID)
	assert.Equal(t, "test-tool", cancelledMetrics[0].ToolName)
	assert.Equal(t, mcp.TaskStatusCancelled, cancelledMetrics[0].Status)
	require.NotNil(t, cancelledMetrics[0].CompletedAt)
	assert.Equal(t, cancelledMetrics[0].CompletedAt.Sub(cancelledMetrics[0].CreatedAt), cancelledMetrics[0].Duration)
}

func TestTaskHooks_TaskStatusChanged(t *testing.T) {
	var mu sync.Mutex
	var statusChanges []TaskMetrics
	done := make(chan struct{})

	hooks := &TaskHooks{}
	hooks.AddOnTaskStatusChanged(func(ctx context.Context, metrics TaskMetrics) {
		mu.Lock()
		defer mu.Unlock()
		statusChanges = append(statusChanges, metrics)
		if len(statusChanges) == 2 {
			close(done)
		}
	})

	server := NewMCPServer("test-server", "1.0.0", WithTaskHooks(hooks))

	// Create a task (status change 1: working)
	ctx := t.Context()
	entry, err := server.createTask(ctx, "test-task-5", "test-tool", nil, nil)
	require.NoError(t, err)

	// Complete the task (status change 2: completed)
	server.completeTask(entry, "result", nil)

	// Wait for both hooks to execute with timeout
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for OnTaskStatusChanged hooks")
	}

	mu.Lock()
	defer mu.Unlock()

	// Should have 2 status changes: working -> completed
	require.Len(t, statusChanges, 2)
	assert.Equal(t, mcp.TaskStatusWorking, statusChanges[0].Status)
	assert.Equal(t, mcp.TaskStatusCompleted, statusChanges[1].Status)
}

func TestTaskHooks_MultipleHooks(t *testing.T) {
	var mu sync.Mutex
	createdCount := 0
	completedCount := 0
	statusChangeCount := 0
	var wg sync.WaitGroup
	wg.Add(4) // 1 created + 1 completed + 2 status changes

	hooks := &TaskHooks{}
	hooks.AddOnTaskCreated(func(ctx context.Context, metrics TaskMetrics) {
		mu.Lock()
		defer mu.Unlock()
		createdCount++
		wg.Done()
	})
	hooks.AddOnTaskCompleted(func(ctx context.Context, metrics TaskMetrics) {
		mu.Lock()
		defer mu.Unlock()
		completedCount++
		wg.Done()
	})
	hooks.AddOnTaskStatusChanged(func(ctx context.Context, metrics TaskMetrics) {
		mu.Lock()
		defer mu.Unlock()
		statusChangeCount++
		wg.Done()
	})

	server := NewMCPServer("test-server", "1.0.0", WithTaskHooks(hooks))

	// Create and complete a task
	ctx := t.Context()
	entry, err := server.createTask(ctx, "test-task-6", "test-tool", nil, nil)
	require.NoError(t, err)
	server.completeTask(entry, "result", nil)

	// Wait for all hooks to execute with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for hooks")
	}

	mu.Lock()
	defer mu.Unlock()

	assert.Equal(t, 1, createdCount, "Should call created hook once")
	assert.Equal(t, 1, completedCount, "Should call completed hook once")
	assert.Equal(t, 2, statusChangeCount, "Should call status changed hook twice (working + completed)")
}

func TestTaskHooks_NilHooks(t *testing.T) {
	// Test that nil hooks don't cause panics
	server := NewMCPServer("test-server", "1.0.0")

	ctx := t.Context()
	entry, err := server.createTask(ctx, "test-task-7", "test-tool", nil, nil)
	require.NoError(t, err)

	// These should not panic
	assert.NotPanics(t, func() {
		server.completeTask(entry, "result", nil)
	})
}

func TestTaskHooks_IntegrationWithTaskTool(t *testing.T) {
	var mu sync.Mutex
	var allMetrics []TaskMetrics
	done := make(chan struct{})

	hooks := &TaskHooks{}
	// Capture all events
	hooks.AddOnTaskCreated(func(ctx context.Context, metrics TaskMetrics) {
		mu.Lock()
		defer mu.Unlock()
		allMetrics = append(allMetrics, metrics)
	})
	hooks.AddOnTaskCompleted(func(ctx context.Context, metrics TaskMetrics) {
		mu.Lock()
		defer mu.Unlock()
		allMetrics = append(allMetrics, metrics)
		close(done)
	})

	server := NewMCPServer("test-server", "1.0.0",
		WithTaskHooks(hooks),
		WithTaskCapabilities(true, false, false),
	)

	// Register a task tool
	tool := mcp.Tool{
		Name:        "async-tool",
		Description: "Test async tool",
		Execution: &mcp.ToolExecution{
			TaskSupport: mcp.TaskSupportRequired,
		},
	}
	handler := func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
		return &mcp.CreateTaskResult{}, nil
	}
	server.AddTaskTool(tool, handler)

	// Call the tool
	ctx := t.Context()
	request := mcp.CallToolRequest{
		Params: mcp.CallToolParams{
			Name: "async-tool",
			Task: &mcp.TaskParams{},
		},
	}

	result, reqErr := server.handleTaskAugmentedToolCall(ctx, "test-id", request)
	require.Nil(t, reqErr)
	require.NotNil(t, result)

	// Wait for async execution to complete with timeout
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timeout waiting for task completion")
	}

	mu.Lock()
	defer mu.Unlock()

	// Should have at least 2 events: created and completed
	assert.GreaterOrEqual(t, len(allMetrics), 2)
	assert.Equal(t, "async-tool", allMetrics[0].ToolName)
}

// Task hooks run outside the server's task lock: a hook may take as long
// as it likes, or call back into the server for task state, without
// blocking every other task request.
func TestTaskHooks_RunOutsideTheTaskLock(t *testing.T) {
	var server *MCPServer
	var blocked sync.Map
	// readsTaskState is what a hook that calls back into the server does:
	// take the task lock for reading. Under the write lock it never returns.
	readsTaskState := func(event string) func(context.Context, TaskMetrics) {
		return func(context.Context, TaskMetrics) {
			read := make(chan struct{})
			go func() {
				server.tasksMu.RLock()
				_ = len(server.tasks)
				server.tasksMu.RUnlock()
				close(read)
			}()
			select {
			case <-read:
			case <-time.After(time.Second):
				blocked.Store(event, true)
			}
		}
	}
	completed, cancelled := make(chan struct{}), make(chan struct{}, 2)
	hooks := &TaskHooks{}
	hooks.AddOnTaskCreated(readsTaskState("created"))
	hooks.AddOnTaskCompleted(func(ctx context.Context, m TaskMetrics) {
		readsTaskState("completed")(ctx, m)
		close(completed)
	})
	hooks.AddOnTaskCancelled(func(ctx context.Context, m TaskMetrics) {
		readsTaskState("cancelled")(ctx, m)
		cancelled <- struct{}{}
	})
	server = NewMCPServer("test-server", "1.0.0", WithTaskHooks(hooks), WithTaskCapabilities(true, true, false))
	server.AddTaskTool(mcp.Tool{Name: "quick", Execution: &mcp.ToolExecution{TaskSupport: mcp.TaskSupportRequired}},
		func(context.Context, mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
			return &mcp.CreateTaskResult{}, nil
		})
	server.AddTaskTool(mcp.Tool{Name: "slow", Execution: &mcp.ToolExecution{TaskSupport: mcp.TaskSupportRequired}},
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
	// A handler that reports cancellation nobody asked for takes the
	// handler-detected cancellation path.
	server.AddTaskTool(mcp.Tool{Name: "self-cancel", Execution: &mcp.ToolExecution{TaskSupport: mcp.TaskSupportRequired}},
		func(context.Context, mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
			return nil, context.Canceled
		})

	ctx := t.Context()
	_, reqErr := server.handleTaskAugmentedToolCall(ctx, "quick", mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "quick", Task: &mcp.TaskParams{}}})
	require.Nil(t, reqErr)
	select {
	case <-completed:
	case <-time.After(5 * time.Second):
		t.Fatal("the quick task never completed")
	}

	result, reqErr := server.handleTaskAugmentedToolCall(ctx, "slow", mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "slow", Task: &mcp.TaskParams{}}})
	require.Nil(t, reqErr)
	require.NoError(t, server.cancelTask(ctx, result.Task.TaskId))
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow task was never cancelled")
	}

	_, reqErr = server.handleTaskAugmentedToolCall(ctx, "self-cancel", mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "self-cancel", Task: &mcp.TaskParams{}}})
	require.Nil(t, reqErr)
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the self-cancelling task was never cancelled")
	}

	for _, event := range []string{"created", "completed", "cancelled"} {
		_, wasBlocked := blocked.Load(event)
		assert.False(t, wasBlocked, "the %s hook ran under the task lock", event)
	}
}

// A task's hooks come in order: a cancellation that lands while the created
// hook is still running fires its own hook after it.
func TestTaskHooks_CreatedHookPrecedesCancellation(t *testing.T) {
	entered, release := make(chan string, 1), make(chan struct{})
	var mu sync.Mutex
	var events []string
	record := func(event string) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	}
	hooks := &TaskHooks{}
	hooks.AddOnTaskCreated(func(_ context.Context, m TaskMetrics) {
		entered <- m.TaskID
		<-release
		record("created")
	})
	cancelled := make(chan struct{})
	hooks.AddOnTaskCancelled(func(context.Context, TaskMetrics) {
		record("cancelled")
		close(cancelled)
	})
	server := NewMCPServer("test-server", "1.0.0", WithTaskHooks(hooks), WithTaskCapabilities(true, true, false))
	handlerDone := make(chan struct{})
	server.AddTaskTool(mcp.Tool{Name: "slow", Execution: &mcp.ToolExecution{TaskSupport: mcp.TaskSupportRequired}},
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
			defer close(handlerDone)
			<-ctx.Done()
			return nil, ctx.Err()
		})

	ctx := t.Context()
	go func() {
		_, _ = server.handleTaskAugmentedToolCall(ctx, "slow", mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "slow", Task: &mcp.TaskParams{}}})
	}()
	taskID := <-entered
	cancelErr := make(chan error, 1)
	go func() { cancelErr <- server.cancelTask(ctx, taskID) }()
	// Give the cancellation time to reach its hook, which must wait.
	time.Sleep(100 * time.Millisecond)
	close(release)
	require.NoError(t, <-cancelErr)
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the task was never cancelled")
	}
	// The cancellation reached the handler, which started after it.
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler of a task cancelled during its created hook kept running")
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"created", "cancelled"}, events)
}

// A created hook that panics does not leave the task's later hooks waiting:
// the task can still be cancelled.
func TestTaskHooks_APanickingCreatedHookDoesNotBlockCancellation(t *testing.T) {
	hooks := &TaskHooks{}
	hooks.AddOnTaskCreated(func(context.Context, TaskMetrics) { panic("hook bug") })
	server := NewMCPServer("test-server", "1.0.0", WithTaskHooks(hooks), WithTaskCapabilities(true, true, false))

	ctx := t.Context()
	var entry *taskEntry
	func() {
		defer func() { require.Equal(t, "hook bug", recover()) }()
		entry, _ = server.createTask(ctx, "task-1", "tool", nil, nil)
	}()
	server.tasksMu.RLock()
	entry = server.tasks["task-1"]
	server.tasksMu.RUnlock()
	require.NotNil(t, entry, "the task was not registered")

	cancelled := make(chan error, 1)
	go func() { cancelled <- server.cancelTask(ctx, "task-1") }()
	select {
	case err := <-cancelled:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("cancelling a task whose created hook panicked hung")
	}
}
