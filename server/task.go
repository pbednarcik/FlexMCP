package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// taskEntry holds task state and associated data
type taskEntry struct {
	task       mcp.Task
	sessionID  string
	toolName   string             // Name of the tool that created this task
	createdAt  time.Time          // When the task was created (for metrics)
	result     any                // The actual result once completed
	resultErr  error              // Error if task failed
	cancelFunc context.CancelFunc // Function to cancel the task
	done       chan struct{}      // Channel to signal task completion
	completed  bool               // Whether the task has been completed (guards done channel closure)
	// createdHookDone, under tasksMu, says the created hook has returned;
	// until then the hook of a completion or cancellation waits in
	// pendingHooks, which createTask runs after the created hook. A task's
	// hooks so keep their order with no lock held across hook code: a hook
	// that cancels its own task does not wait for itself.
	createdHookDone bool
	pendingHooks    []func()
	// slotReleased, under tasksMu, says the concurrency slot createTask took
	// has been given back: once, when the handler has returned.
	slotReleased bool
}

//
// Task Request Handlers
//

// handleGetTask handles tasks/get requests to retrieve task status.
func (s *MCPServer) handleGetTask(
	ctx context.Context,
	id any,
	request mcp.GetTaskRequest,
) (*mcp.GetTaskResult, *requestError) {
	task, _, err := s.getTask(ctx, request.Params.TaskId)
	if err != nil {
		return nil, &requestError{
			id:   id,
			code: mcp.INVALID_PARAMS,
			err:  err,
		}
	}

	result := mcp.NewGetTaskResult(task)
	return &result, nil
}

// handleListTasks handles tasks/list requests to list all tasks.
func (s *MCPServer) handleListTasks(
	ctx context.Context,
	id any,
	request mcp.ListTasksRequest,
) (*mcp.ListTasksResult, *requestError) {
	tasks := s.listTasks(ctx)

	// Sort tasks by TaskId for consistent pagination
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].TaskId < tasks[j].TaskId
	})

	// Apply pagination
	tasksToReturn, nextCursor, err := listByPagination(
		ctx,
		s,
		request.Params.Cursor,
		tasks,
	)
	if err != nil {
		return nil, &requestError{
			id:   id,
			code: mcp.INVALID_PARAMS,
			err:  err,
		}
	}

	result := mcp.ListTasksResult{
		Tasks:      tasksToReturn,
		NextCursor: nextCursor,
	}
	return &result, nil
}

// handleTaskResult handles tasks/result requests to get task results.
func (s *MCPServer) handleTaskResult(
	ctx context.Context,
	id any,
	request mcp.TaskResultRequest,
) (*mcp.TaskResultResult, *requestError) {
	task, done, err := s.getTask(ctx, request.Params.TaskId)
	if err != nil {
		return nil, &requestError{
			id:   id,
			code: mcp.INVALID_PARAMS,
			err:  err,
		}
	}

	// Wait for task completion if not terminal
	if !task.Status.IsTerminal() {
		select {
		case <-done:
			// Task completed
		case <-ctx.Done():
			return nil, &requestError{
				id:   id,
				code: mcp.REQUEST_INTERRUPTED,
				err:  ctx.Err(),
			}
		}
	}

	// Re-fetch the task entry to get the final result/error under lock
	entry, err := s.getTaskEntry(ctx, request.Params.TaskId)
	if err != nil {
		return nil, &requestError{
			id:   id,
			code: mcp.INVALID_PARAMS,
			err:  err,
		}
	}

	// Read result and error under lock
	s.tasksMu.RLock()
	storedResult := entry.result
	resultErr := entry.resultErr
	taskID := entry.task.TaskId
	s.tasksMu.RUnlock()

	// Return error if task failed
	if resultErr != nil {
		return nil, handlerError(ctx, id, resultErr)
	}

	// Extract the CallToolResult and populate TaskResultResult
	result := &mcp.TaskResultResult{
		Meta: mcp.WithRelatedTask(taskID),
	}

	switch taskResult := storedResult.(type) {
	case *mcp.CallToolResult:
		result.Content = taskResult.Content
		result.StructuredContent = taskResult.StructuredContent
		// The bytes are copied so the reply does not share them with the
		// stored result, which later tasks/result calls read again.
		if len(taskResult.RawStructuredContent) > 0 {
			result.RawStructuredContent = append(json.RawMessage(nil), taskResult.RawStructuredContent...)
		}
		result.IsError = taskResult.IsError
		mergeTaskResultMeta(result, taskResult.Meta)
	case *mcp.CreateTaskResult:
		result.Content = taskResult.Content
		result.StructuredContent = taskResult.StructuredContent
		result.IsError = taskResult.IsError
		mergeTaskResultMeta(result, taskResult.Meta)
	}

	return result, nil
}

func mergeTaskResultMeta(result *mcp.TaskResultResult, meta *mcp.Meta) {
	if meta == nil {
		return
	}

	if result.Meta.AdditionalFields == nil {
		result.Meta.AdditionalFields = make(map[string]any)
	}

	for k, v := range meta.AdditionalFields {
		if k != mcp.RelatedTaskMetaKey {
			result.Meta.AdditionalFields[k] = v
		}
	}
}

// handleCancelTask handles tasks/cancel requests to cancel a task.
func (s *MCPServer) handleCancelTask(
	ctx context.Context,
	id any,
	request mcp.CancelTaskRequest,
) (*mcp.CancelTaskResult, *requestError) {
	err := s.cancelTask(ctx, request.Params.TaskId)
	if err != nil {
		return nil, &requestError{
			id:   id,
			code: mcp.INVALID_PARAMS,
			err:  err,
		}
	}

	// Get the updated task
	task, _, err := s.getTask(ctx, request.Params.TaskId)
	if err != nil {
		return nil, &requestError{
			id:   id,
			code: mcp.INVALID_PARAMS,
			err:  err,
		}
	}

	result := mcp.NewCancelTaskResult(task)
	return &result, nil
}

//
// Task Management Methods
//

// createTask creates a new task entry and returns it.
// Returns an error if the max concurrent tasks limit is exceeded.
func (s *MCPServer) createTask(ctx context.Context, taskID string, toolName string, ttl *int64, pollInterval *int64) (*taskEntry, error) {
	return s.newTask(ctx, taskID, toolName, ttl, pollInterval, true)
}

// newTask is createTask, with or without a concurrency slot: a task that
// runs no handler takes none, and is not refused for the limit.
func (s *MCPServer) newTask(ctx context.Context, taskID string, toolName string, ttl *int64, pollInterval *int64, withSlot bool) (*taskEntry, error) {
	ttl = s.taskTTLFor(ttl)

	// Build task entry first (no lock needed)
	opts := []mcp.TaskOption{}
	if ttl != nil {
		opts = append(opts, mcp.WithTaskTTL(*ttl))
	}
	if pollInterval != nil {
		opts = append(opts, mcp.WithTaskPollInterval(*pollInterval))
	}
	task := mcp.NewTask(taskID, opts...)
	createdAt := time.Now()

	entry := &taskEntry{
		task:      task,
		sessionID: getSessionID(ctx),
		toolName:  toolName,
		createdAt: createdAt,
		done:      make(chan struct{}),
	}

	// Single critical section for check + increment + insert
	s.tasksMu.Lock()

	if withSlot {
		// Check concurrent task limit
		if s.maxConcurrentTasks != nil && *s.maxConcurrentTasks > 0 {
			if s.activeTasks >= *s.maxConcurrentTasks {
				s.tasksMu.Unlock()
				return nil, fmt.Errorf("max concurrent tasks limit reached (%d)", *s.maxConcurrentTasks)
			}
		}
		s.activeTasks++
	} else {
		// Nothing to give back.
		entry.slotReleased = true
	}

	// Insert the task atomically with the count
	s.tasks[taskID] = entry
	// With no hooks there is nothing to wait for.
	entry.createdHookDone = s.taskHooks == nil
	s.tasksMu.Unlock()

	// The TTL runs from creation, before the hooks: a task whose creation
	// fails below still expires.
	if ttl != nil && *ttl > 0 {
		s.scheduleTaskCleanup(taskID, entry, *ttl)
	}

	// Fire task created hook, outside the lock: a hook may take time, or
	// read task state through the server.
	if s.taskHooks != nil {
		metrics := TaskMetrics{
			TaskID:    taskID,
			ToolName:  toolName,
			Status:    task.Status,
			CreatedAt: createdAt,
			SessionID: getSessionID(ctx),
		}
		// createTask owns the task until it hands the entry back: a panic
		// out of any hook here, the created one or one queued behind it,
		// means the task's goroutine never starts, so the task is failed
		// here, which gives its slot back too; one cancelled meanwhile keeps
		// that status and gives the slot back directly.
		handedOff := false
		defer func() {
			if !handedOff {
				s.completeTask(entry, nil, errTaskHookPanicked)
				s.releaseTaskSlot(entry)
			}
		}()
		func() {
			// However the hook ends: one that panics must not leave the
			// task's later hooks waiting.
			defer s.finishCreatedHook(entry)
			s.taskHooks.taskCreated(ctx, metrics)
		}()
		handedOff = true
	}

	return entry, nil
}

// taskTTLFor returns the TTL a task gets for the one its request asked: the
// request's, cut to the server's; the server's when the request asked none;
// no TTL only when the server has none and the request asked none. A TTL
// beyond what a duration holds is cut to the longest, not wrapped; a
// server TTL under a millisecond is a millisecond, since set, it is a bound.
func (s *MCPServer) taskTTLFor(requested *int64) *int64 {
	const longest = int64(math.MaxInt64 / time.Millisecond)
	serverMs := s.taskTTL.Milliseconds()
	if s.taskTTL > 0 && serverMs == 0 {
		serverMs = 1
	}
	var ms int64
	switch {
	case requested != nil && *requested > 0:
		ms = min(*requested, longest)
		if serverMs > 0 {
			ms = min(ms, serverMs)
		}
	case serverMs > 0:
		ms = serverMs
	default:
		return nil
	}
	return &ms
}

// finishCreatedHook records that entry's created hook has returned and runs
// the hooks that waited for it, in the order they arrived.
func (s *MCPServer) finishCreatedHook(entry *taskEntry) {
	s.tasksMu.Lock()
	entry.createdHookDone = true
	pending := entry.pendingHooks
	entry.pendingHooks = nil
	s.tasksMu.Unlock()
	for _, fire := range pending {
		fire()
	}
}

// releaseTaskSlot gives back the concurrency slot createTask took for entry,
// once: the task's goroutine calls it when the handler has returned, and
// completeTask when a handler's result arrives, whichever is first.
func (s *MCPServer) releaseTaskSlot(entry *taskEntry) {
	s.tasksMu.Lock()
	s.releaseTaskSlotLocked(entry)
	s.tasksMu.Unlock()
}

// releaseTaskSlotLocked is releaseTaskSlot under tasksMu.
func (s *MCPServer) releaseTaskSlotLocked(entry *taskEntry) {
	if entry.slotReleased {
		return
	}
	entry.slotReleased = true
	s.activeTasks--
}

// fireTaskHook runs fire, a hook of entry, with ctx now, or after the
// created hook when that is still running. A hook that waits runs once
// the request that raised it may have ended, so it gets ctx's values
// without its cancellation. Never under tasksMu.
func (s *MCPServer) fireTaskHook(ctx context.Context, entry *taskEntry, fire func(context.Context)) {
	s.tasksMu.Lock()
	if !entry.createdHookDone {
		later := context.WithoutCancel(ctx)
		entry.pendingHooks = append(entry.pendingHooks, func() { fire(later) })
		s.tasksMu.Unlock()
		return
	}
	s.tasksMu.Unlock()
	fire(ctx)
}

// getTask retrieves a task by ID, checking session isolation if applicable.
// Returns a copy of the task and the done channel for waiting on completion.
func (s *MCPServer) getTask(ctx context.Context, taskID string) (mcp.Task, chan struct{}, error) {
	sessionID := getSessionID(ctx)
	s.tasksMu.RLock()
	defer s.tasksMu.RUnlock()
	entry, exists := s.tasks[taskID]
	if !exists || entry.sessionID != sessionID {
		return mcp.Task{}, nil, s.taskUnreachableLocked(taskID, sessionID)
	}
	return entry.task, entry.done, nil
}

// getTaskEntry retrieves the raw task entry for internal use (requires caller to handle synchronization).
func (s *MCPServer) getTaskEntry(ctx context.Context, taskID string) (*taskEntry, error) {
	sessionID := getSessionID(ctx)
	s.tasksMu.RLock()
	defer s.tasksMu.RUnlock()
	entry, exists := s.tasks[taskID]
	if !exists || entry.sessionID != sessionID {
		return nil, s.taskUnreachableLocked(taskID, sessionID)
	}
	return entry, nil
}

// taskUnreachableLocked is the error for a task the caller's session does
// not reach. A task belongs to the session that created it, and a task
// created with no session to a caller with none, as every streamable-HTTP
// request is: there the ID alone is the bearer token (Tasks.md row 25). A
// task that expired is expired to its own session and unknown to any
// other, so the tombstone tells no one else that the ID existed.
func (s *MCPServer) taskUnreachableLocked(taskID, sessionID string) error {
	if owner, wasExpired := s.expiredTasks[taskID]; wasExpired && owner == sessionID {
		return errors.New("task has expired")
	}
	return errors.New("task not found")
}

// listTasks returns copies of the tasks of the caller's session. A caller whose
// session has no ID cannot be told from any other, so it lists nothing: a
// server that cannot identify requestors does not list tasks (Tasks.md row
// 35); its tasks stay reachable by their ID, which is the bearer token of a
// server without sessions (row 39).
func (s *MCPServer) listTasks(ctx context.Context) []mcp.Task {
	sessionID := getSessionID(ctx)
	if sessionID == "" {
		return nil
	}

	s.tasksMu.RLock()
	defer s.tasksMu.RUnlock()

	var tasks []mcp.Task
	for _, entry := range s.tasks {
		if entry.sessionID == sessionID {
			tasks = append(tasks, entry.task)
		}
	}

	return tasks
}

// completeTask marks a task as completed with the given result.
func (s *MCPServer) completeTask(entry *taskEntry, result any, err error) {
	// The message a client may see is rendered by the error, which may be
	// a marked provider's own code: outside the lock.
	var statusMessage string
	if err != nil {
		statusMessage = clientMessage(err)
	}
	s.tasksMu.Lock()

	// Guard against double completion
	if entry.completed {
		s.tasksMu.Unlock()
		return
	}

	completedAt := time.Now()
	duration := completedAt.Sub(entry.createdAt)

	if err != nil {
		entry.task.Status = mcp.TaskStatusFailed
		entry.task.StatusMessage = statusMessage
		entry.resultErr = err
	} else {
		entry.task.Status = mcp.TaskStatusCompleted
		entry.result = result
	}

	// Update the lastUpdatedAt timestamp
	entry.task.LastUpdatedAt = completedAt.UTC().Format(time.RFC3339)

	// Mark as completed and signal
	entry.completed = true
	close(entry.done)

	// The handler has returned: its slot is free.
	s.releaseTaskSlotLocked(entry)

	// The metrics are read under the lock; the hooks run outside it.
	var metrics *TaskMetrics
	if s.taskHooks != nil {
		metrics = &TaskMetrics{
			TaskID:        entry.task.TaskId,
			ToolName:      entry.toolName,
			Status:        entry.task.Status,
			StatusMessage: entry.task.StatusMessage,
			CreatedAt:     entry.createdAt,
			CompletedAt:   &completedAt,
			Duration:      duration,
			SessionID:     entry.sessionID,
			Error:         err,
		}
	}
	task := entry.task
	s.tasksMu.Unlock()

	s.sendTaskStatusNotification(entry.sessionID, task)
	if metrics == nil {
		return
	}
	s.fireTaskHook(context.Background(), entry, func(ctx context.Context) {
		if err != nil {
			s.taskHooks.taskFailed(ctx, *metrics)
		} else {
			s.taskHooks.taskCompleted(ctx, *metrics)
		}
	})
}

// cancelTask cancels a running task.
func (s *MCPServer) cancelTask(ctx context.Context, taskID string) error {
	entry, err := s.getTaskEntry(ctx, taskID)
	if err != nil {
		return err
	}

	s.tasksMu.Lock()

	// Don't allow cancelling already completed tasks
	if entry.completed {
		s.tasksMu.Unlock()
		return fmt.Errorf("cannot cancel task in terminal status: %s", entry.task.Status)
	}

	// Cancel the context if available
	if entry.cancelFunc != nil {
		entry.cancelFunc()
	}

	cancelledAt := time.Now()
	duration := cancelledAt.Sub(entry.createdAt)

	entry.task.Status = mcp.TaskStatusCancelled
	entry.task.StatusMessage = "Task cancelled by request"
	// Update the lastUpdatedAt timestamp
	entry.task.LastUpdatedAt = cancelledAt.UTC().Format(time.RFC3339)

	// Mark as completed and signal
	entry.completed = true
	close(entry.done)

	// The slot stays taken: the handler may not have noticed yet, and it
	// gives the slot back when it returns.

	// The metrics are read under the lock; the hook runs outside it.
	var metrics *TaskMetrics
	if s.taskHooks != nil {
		metrics = &TaskMetrics{
			TaskID:        entry.task.TaskId,
			ToolName:      entry.toolName,
			Status:        entry.task.Status,
			StatusMessage: entry.task.StatusMessage,
			CreatedAt:     entry.createdAt,
			CompletedAt:   &cancelledAt,
			Duration:      duration,
			SessionID:     entry.sessionID,
		}
	}
	task := entry.task
	s.tasksMu.Unlock()

	s.sendTaskStatusNotification(entry.sessionID, task)
	if metrics != nil {
		s.fireTaskHook(ctx, entry, func(ctx context.Context) { s.taskHooks.taskCancelled(ctx, *metrics) })
	}

	return nil
}

// scheduleTaskCleanup arranges for the task to be ended and removed from
// storage once its TTL passes, so clients have the full TTL window to
// retrieve results. A timer, not a goroutine: a server keeping many tasks
// for a long time would park a stack each. It only performs cleanup if the
// stored task entry still matches expected.
func (s *MCPServer) scheduleTaskCleanup(taskID string, expected *taskEntry, ttlMs int64) {
	time.AfterFunc(time.Duration(ttlMs)*time.Millisecond, func() { s.expireTask(taskID, expected) })
}

// expireTask ends and removes a task whose TTL has passed, when the stored
// entry is still expected: a task that was removed and replaced under the
// same ID is left alone.
func (s *MCPServer) expireTask(taskID string, expected *taskEntry) {
	var cancel context.CancelFunc
	s.tasksMu.Lock()
	entry, ok := s.tasks[taskID]
	if !ok || entry != expected {
		s.tasksMu.Unlock()
		return
	}
	if !entry.completed {
		cancel = entry.cancelFunc
	}
	delete(s.tasks, taskID)
	s.expiredTasks[taskID] = entry.sessionID
	s.tasksMu.Unlock()

	if cancel != nil {
		cancel()
	}

	// Remove tombstone after 5 minutes.
	time.AfterFunc(5*time.Minute, func() {
		s.tasksMu.Lock()
		delete(s.expiredTasks, taskID)
		s.tasksMu.Unlock()
	})
}

// sendTaskStatusNotification tells the session that created a task of its
// new status. That session alone: a task ID is the bearer token of its
// stored state, which no other session may learn (Tasks.md row 25). A task
// created with no session has nobody to tell; its client polls. Never under
// tasksMu: the send walks the session's streams and may call the error
// hook.
func (s *MCPServer) sendTaskStatusNotification(sessionID string, task mcp.Task) {
	if sessionID == "" {
		return
	}
	value, ok := s.sessions.Load(sessionID)
	if !ok {
		return
	}
	session, ok := value.(ClientSession)
	if !ok || !session.Initialized() {
		return
	}

	// Convert task to map[string]any for notification params
	taskMap := map[string]any{
		"taskId":        task.TaskId,
		"status":        task.Status,
		"createdAt":     task.CreatedAt,
		"lastUpdatedAt": task.LastUpdatedAt,
	}

	if task.StatusMessage != "" {
		taskMap["statusMessage"] = task.StatusMessage
	}
	if task.TTL != nil {
		taskMap["ttl"] = *task.TTL
	}
	if task.PollInterval != nil {
		taskMap["pollInterval"] = *task.PollInterval
	}

	s.broadcastToSession(session, mcp.JSONRPCNotification{
		JSONRPC: mcp.JSONRPC_VERSION,
		Method:  mcp.MethodNotificationTasksStatus,
		Params:  mcp.NotificationParams{AdditionalFields: taskMap},
	})
}

// getSessionID extracts the session ID from the context.
func getSessionID(ctx context.Context) string {
	if session := ClientSessionFromContext(ctx); session != nil {
		return session.SessionID()
	}
	return ""
}

// servesTasks reports whether the server declares tasks.requests.tools.call,
// without which no tool can be task-called (2025-11-25 Tasks.md row 28).
func (s *MCPServer) servesTasks() bool {
	s.capabilitiesMu.RLock()
	defer s.capabilitiesMu.RUnlock()
	return s.capabilities.tasks != nil && s.capabilities.tasks.toolCallTasks
}
