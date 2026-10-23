package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/mark3labs/mcp-go/mcp"
)

// limitCreateTaskResult measures the payload tasks/result will copy out,
// including the handler's _meta. The content fields are omitted from
// CreateTaskResult's own JSON.
func (s *MCPServer) limitCreateTaskResult(result *mcp.CreateTaskResult) *mcp.CreateTaskResult {
	if result == nil || s.toolResultSizeLimit <= 0 {
		return result
	}
	view := &mcp.CallToolResult{
		Meta:              result.Meta,
		Content:           result.Content,
		StructuredContent: result.StructuredContent,
		IsError:           result.IsError,
	}
	limited := s.limitToolResult(view)
	if limited == view {
		return result
	}
	stored := *result
	stored.Meta = nil
	stored.Content = limited.Content
	stored.StructuredContent = limited.StructuredContent
	stored.IsError = limited.IsError
	return &stored
}

// handleTaskAugmentedToolCall handles tool calls that are executed as tasks.
// It creates a task entry, starts async execution, and returns CreateTaskResult immediately.
func (s *MCPServer) handleTaskAugmentedToolCall(
	ctx context.Context,
	id any,
	request mcp.CallToolRequest,
) (*mcp.CreateTaskResult, *requestError) {
	// Look up the tool - check both taskTools and regular tools
	s.toolsMu.RLock()
	taskTool, isTaskTool := s.taskTools[request.Params.Name]
	regularTool, isRegularTool := s.tools[request.Params.Name]
	inputSchema, inputSchemaKept := s.inputSchemaOf(request.Params.Name)
	outputSchema, outputSchemaKept := s.outputSchemaOf(request.Params.Name)
	s.toolsMu.RUnlock()

	// Determine which tool to use and validate task support
	var toolToUse ServerTaskTool
	var hasTaskHandler bool

	if isTaskTool {
		// Tool is registered as a task tool
		toolToUse = taskTool
		hasTaskHandler = true
	} else if isRegularTool {
		// Tool is a regular tool with task support
		// Validate that it actually supports task augmentation
		if regularTool.Tool.Execution == nil ||
			(regularTool.Tool.Execution.TaskSupport != mcp.TaskSupportOptional &&
				regularTool.Tool.Execution.TaskSupport != mcp.TaskSupportRequired) {
			return nil, &requestError{
				id:   id,
				code: mcp.METHOD_NOT_FOUND,
				err:  fmt.Errorf("tool '%s' does not support task augmentation", request.Params.Name),
			}
		}

		hasTaskHandler = false
	} else {
		// Tool not found in either map
		return nil, &requestError{
			id:   id,
			code: mcp.INVALID_PARAMS,
			err:  fmt.Errorf("tool '%s' not found", request.Params.Name),
		}
	}

	// The call was decoded for the tool registered then, and a tool that has
	// not opted in gets the arguments as a call decoded at once carries them.
	// A task tool's name is never a regular tool's, so regularTool is then
	// the zero tool, which has not.
	onDemand := regularTool.ArgumentsOnDemand
	if !onDemand && s.mayLeaveArgumentsToTheGetters(&request.Params) {
		request.Params = argumentsAtOnce(request.Params)
	}

	// The arguments are validated as on the synchronous path (Tools.md row
	// 23): ones that fail never reach the handler. The task is still
	// created, so the client fetches the same tool error through
	// tasks/result; it takes no concurrency slot, since no handler runs for
	// it, so the limit does not hide the validation error.
	var invalid error
	if s.inputValidator != nil {
		toolDef := regularTool.Tool
		if hasTaskHandler {
			toolDef = toolToUse.Tool
		}
		if _, err := s.inputValidator.validateRegistered(toolDef, inputSchema, inputSchemaKept, s.argumentsToValidate(request, onDemand)); err != nil {
			invalid = err
		}
	}

	// Generate task ID (UUID v4)
	taskID := uuid.New().String()

	// Extract TTL from task params
	var ttl *int64
	if request.Params.Task != nil {
		ttl = request.Params.Task.TTL
	}

	// Create task entry (pollInterval is nil - server doesn't set a default)
	entry, err := s.newTask(ctx, taskID, request.Params.Name, ttl, nil, invalid == nil)
	if err != nil {
		return nil, &requestError{
			id:   id,
			code: mcp.INTERNAL_ERROR,
			err:  err,
		}
	}

	if invalid != nil {
		// No handler runs for this task: it is completed here. A hook may
		// have cancelled it meanwhile, which completeTask leaves as it is.
		s.completeTask(entry, validationToolResult(invalid), nil)
		s.tasksMu.RLock()
		taskCopy := entry.task
		s.tasksMu.RUnlock()
		return &mcp.CreateTaskResult{Task: taskCopy}, nil
	}

	// Snapshot the task as "working" before launching execution, so the
	// CreateTaskResult reflects the just-created state even if the async
	// handler completes before we read it back (a fast/synchronous handler
	// can finish before this goroutine is scheduled).
	s.tasksMu.RLock()
	taskCopy := entry.task
	s.tasksMu.RUnlock()

	// The task outlives the request that created it: the request's context
	// is cancelled once its response is written, so the task runs on one
	// that keeps the request's values (session, protocol info) but ends only
	// through tasks/cancel or the TTL.
	taskCtx := context.WithoutCancel(ctx)
	entry.outputSchema, entry.outputSchemaKept = outputSchema, outputSchemaKept
	if hasTaskHandler {
		go s.executeTaskTool(taskCtx, entry, toolToUse, request)
	} else {
		go s.executeRegularToolAsTask(taskCtx, entry, regularTool, request)
	}

	// Return CreateTaskResult immediately with task as top-level field
	return &mcp.CreateTaskResult{
		Task: taskCopy,
	}, nil
}

// executeTaskTool executes a task tool handler asynchronously.
// It creates a cancellable context, stores the cancel function for potential cancellation,
// and executes the handler in the background, storing the result when complete.
func (s *MCPServer) executeTaskTool(
	ctx context.Context,
	entry *taskEntry,
	taskTool ServerTaskTool,
	request mcp.CallToolRequest,
) {
	// The slot is given back once the handler has returned, however the
	// task ended: a cancelled task's handler may run on for a while, and a
	// panicking one is failed first.
	defer s.releaseTaskSlot(entry)
	defer func() {
		if r := recover(); r != nil {
			s.completeTask(entry, nil, fmt.Errorf("panic in task tool handler %s: %v", request.Params.Name, r))
		}
	}()

	// Create cancellable context for this task execution
	taskCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Register cancellation atomically with TTL cleanup, which may have
	// already removed the task, and with tasks/cancel, which may have ended
	// it while the created hook ran.
	s.tasksMu.Lock()
	entry.cancelFunc = cancel
	ended := s.tasks[entry.task.TaskId] != entry || entry.completed
	s.tasksMu.Unlock()
	if ended {
		cancel()
	}

	// The handler runs inside the task tool middlewares, outermost first;
	// the chain is built outside the lock, so a middleware that panics while
	// wrapping does not keep it.
	s.toolMiddlewareMu.RLock()
	middlewares := slices.Clone(s.taskToolHandlerMiddlewares)
	s.toolMiddlewareMu.RUnlock()
	finalHandler := taskTool.Handler
	for _, m := range slices.Backward(middlewares) {
		finalHandler = m(finalHandler)
	}
	result, err := finalHandler(taskCtx, request)

	if err != nil {
		// A context error is a cancellation only when the task's own context
		// ended: tasks/cancel or the TTL. One returned while that context is
		// live is the handler's or a middleware's own, a deadline of theirs,
		// and fails the task like any other error. cancelTask sets the status
		// of a cancelled task; a handler that saw the cancel first sets it here.
		if taskCtx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			// Check if task was already cancelled via tasks/cancel
			s.tasksMu.Lock()
			alreadyCancelled := entry.task.Status == mcp.TaskStatusCancelled
			s.tasksMu.Unlock()

			if !alreadyCancelled {
				// Handler detected cancellation before tasks/cancel was called
				// Mark as cancelled with the context error message
				cancelledAt := time.Now()
				duration := cancelledAt.Sub(entry.createdAt)

				// The metrics and the status are read under the lock; the
				// notification and the hook go outside it.
				var metrics *TaskMetrics
				var status *mcp.Task
				s.tasksMu.Lock()
				if !entry.completed {
					entry.task.Status = mcp.TaskStatusCancelled
					entry.task.StatusMessage = "Task cancelled"
					entry.task.LastUpdatedAt = cancelledAt.UTC().Format(time.RFC3339)
					entry.completed = true
					close(entry.done)

					s.releaseTaskSlotLocked(entry)
					status = new(entry.task)

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
				}
				s.tasksMu.Unlock()
				if status != nil {
					s.sendTaskStatusNotification(entry.sessionID, *status)
				}
				if metrics != nil {
					s.fireTaskHook(ctx, entry, func(ctx context.Context) { s.taskHooks.taskCancelled(ctx, *metrics) })
				}
			}
			return
		}

		// Task failed - complete with error
		s.completeTask(entry, nil, err)
		return
	}

	// tasks/result has nothing to report for a handler that returned neither
	// a result nor an error, so the task fails.
	if result == nil {
		s.completeTask(entry, nil, noResultError("tool", request.Params.Name))
		return
	}

	// Task succeeded - store the CreateTaskResult
	// Note: The actual result will be retrieved later via tasks/result
	//
	// Validate the StructuredContent against the tool's declared output
	// schema when WithOutputSchemaValidation is enabled. If validation fails,
	// persist a tool execution error in place of the bad result so the
	// client cannot retrieve a result that violates the schema via
	// tasks/result. handleTaskResult accepts both *CallToolResult and
	// *CreateTaskResult, so storing a *CallToolResult here is safe.
	if s.outputValidator != nil {
		if _, vErr := s.outputValidator.validateCreateTaskResult(taskTool.Tool, entry.outputSchema, entry.outputSchemaKept, result); vErr != nil {
			s.completeTask(entry, validationToolResult(vErr), nil)
			return
		}
	}
	s.completeTask(entry, s.limitCreateTaskResult(result), nil)
}

// executeRegularToolAsTask executes a regular tool handler asynchronously as a task.
// This is used for hybrid mode where a tool with TaskSupportOptional is called with task params.
func (s *MCPServer) executeRegularToolAsTask(
	ctx context.Context,
	entry *taskEntry,
	regularTool ServerTool,
	request mcp.CallToolRequest,
) {
	// The slot is given back once the handler has returned, however the
	// task ended: a cancelled task's handler may run on for a while, and a
	// panicking one is failed first.
	defer s.releaseTaskSlot(entry)
	// A panic fails the task, as it does a task tool's: this goroutine has
	// no transport above it to recover one.
	defer func() {
		if r := recover(); r != nil {
			s.completeTask(entry, nil, fmt.Errorf("panic in tool handler %s: %v", request.Params.Name, r))
		}
	}()

	// Create cancellable context for this task execution
	taskCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Register cancellation atomically with TTL cleanup, which may have
	// already removed the task, and with tasks/cancel, which may have ended
	// it while the created hook ran.
	s.tasksMu.Lock()
	entry.cancelFunc = cancel
	ended := s.tasks[entry.task.TaskId] != entry || entry.completed
	s.tasksMu.Unlock()
	if ended {
		cancel()
	}

	// The handler runs inside the tool middlewares, the chain built outside
	// the lock as in handleToolCall.
	finalHandler := regularTool.Handler
	s.toolMiddlewareMu.RLock()
	mw := s.toolHandlerMiddlewares
	s.toolMiddlewareMu.RUnlock()
	for _, m := range slices.Backward(mw) {
		finalHandler = m(finalHandler)
	}

	result, err := finalHandler(taskCtx, request)

	if err != nil {
		// A context error is a cancellation only when the task's own context
		// ended: tasks/cancel or the TTL. One returned while that context is
		// live is the handler's or a middleware's own, a deadline of theirs,
		// and fails the task like any other error. cancelTask sets the status
		// of a cancelled task; a handler that saw the cancel first sets it here.
		if taskCtx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			// Check if task was already cancelled via tasks/cancel
			s.tasksMu.Lock()
			alreadyCancelled := entry.task.Status == mcp.TaskStatusCancelled
			s.tasksMu.Unlock()

			if !alreadyCancelled {
				// Handler detected cancellation before tasks/cancel was called
				// Mark as cancelled with the context error message
				cancelledAt := time.Now()
				duration := cancelledAt.Sub(entry.createdAt)

				// The metrics and the status are read under the lock; the
				// notification and the hook go outside it.
				var metrics *TaskMetrics
				var status *mcp.Task
				s.tasksMu.Lock()
				if !entry.completed {
					entry.task.Status = mcp.TaskStatusCancelled
					entry.task.StatusMessage = "Task cancelled"
					entry.task.LastUpdatedAt = cancelledAt.UTC().Format(time.RFC3339)
					entry.completed = true
					close(entry.done)

					s.releaseTaskSlotLocked(entry)
					status = new(entry.task)

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
				}
				s.tasksMu.Unlock()
				if status != nil {
					s.sendTaskStatusNotification(entry.sessionID, *status)
				}
				if metrics != nil {
					s.fireTaskHook(ctx, entry, func(ctx context.Context) { s.taskHooks.taskCancelled(ctx, *metrics) })
				}
			}
			return
		}

		// Task failed - complete with error
		s.completeTask(entry, nil, err)
		return
	}

	// tasks/result has nothing to report for a handler that returned neither
	// a result nor an error, so the task fails.
	if result == nil {
		s.completeTask(entry, nil, noResultError("tool", request.Params.Name))
		return
	}

	// A handler may ask the client for more input (SEP-2322). Inside a task
	// that goes through tasks/update (Tasks.md row 15), which this server
	// does not serve yet, so the task fails rather than end as an empty
	// completed task; a client that could not be asked at all gets the
	// synchronous call's error.
	if _, err := resolveMultiRoundTrip(taskCtx, result, callToolResultNeedsInput); err != nil {
		s.completeTask(entry, nil, err)
		return
	}
	if result.NeedsInput() {
		err := ErrTaskInputUnsupported
		if len(result.InputRequests) == 0 {
			// Nothing was asked: the handler sheds load, as the
			// synchronous path tells a client that cannot be asked.
			err = ErrLoadShedding
		}
		s.completeTask(entry, nil, err)
		return
	}

	// Task succeeded - store the CallToolResult directly
	// When retrieved via tasks/result, this will be returned to the client
	//
	// Mirror the synchronous path: validate the result's StructuredContent
	// against the tool's declared output schema when
	// WithOutputSchemaValidation is enabled. A validation failure replaces
	// the result with a tool execution error so the bad payload never
	// reaches the client via tasks/result.
	if s.outputValidator != nil {
		if _, vErr := s.outputValidator.validate(regularTool.Tool, entry.outputSchema, entry.outputSchemaKept, result); vErr != nil {
			s.completeTask(entry, validationToolResult(vErr), nil)
			return
		}
	}
	s.completeTask(entry, s.limitToolResult(result), nil)
}
