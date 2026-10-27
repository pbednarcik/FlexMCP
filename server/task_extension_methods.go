package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/pbednarcik/flexmcp/mcp"
)

// taskRefusal is the data of a refused task id: why it was refused.
type taskRefusal struct {
	Reason string `json:"reason"`
}

var (
	taskUnknownMessage = errTaskUnknown.Error()
	taskGoneMessage    = errTaskGone.Error()
	taskUnknownData    = taskRefusal{Reason: "unknown"}
	taskGoneData       = taskRefusal{Reason: "gone"}
	// tasksDeclared is what a request declares to reach a task method; it is
	// only read.
	tasksDeclared = mcp.ClientCapabilities{Extensions: map[string]any{mcp.ExtensionTasks: map[string]any{}}}
)

var (
	errTaskHandlerPanic       = errors.New("the task handler panicked")
	errTaskNoResult           = errors.New("a completed task carries no result")
	errTaskNestedTask         = errors.New("a completed task's result is itself a task")
	errTaskResultNeedsInput   = errors.New("a completed task's result asks for input")
	errTaskZeroOrigin         = errors.New("a completed task carries the zero origin")
	errTaskFailedWithoutError = errors.New("a failed task carries no error")
	errTaskInputUnserved      = errors.New("an input_required task needs a TaskInputHandler to progress")
	errTaskUnknownStatus      = errors.New("a task status no protocol version defines")
)

// failedTaskMessage is the statusMessage of a failed task that gave none.
const failedTaskMessage = "task failed"

// unboundCallerWarning is logged once when a task request carries
// Authorization and no WithTaskCaller binds task ids.
const unboundCallerWarning = "a Tasks extension request carries Authorization, but no server.WithTaskCaller binds task ids to a caller: " +
	"anyone holding a task id can read the task, cancel it and answer its input requests"

// servesTaskExtension reports whether the task methods of modern requests are
// served: the server has a [TaskHandler].
func (s *MCPServer) servesTaskExtension() bool {
	return s.taskHandler != nil
}

// handleExtensionGetTask serves tasks/get on a modern request: the task as
// its handler renders it now.
func (s *MCPServer) handleExtensionGetTask(ctx context.Context, id any, request mcp.ExtensionTaskRequest) (*mcp.ExtensionTaskStateResult, *requestError) {
	ref, reqErr := s.openTask(ctx, id, request.Header, request.Params.TaskId)
	if reqErr != nil {
		return nil, reqErr
	}
	state, err := s.extensionTaskState(ctx, ref)
	if err != nil {
		return nil, taskHandlerError(ctx, id, err)
	}
	return s.renderTaskState(ctx, id, ref.TaskID, &state)
}

// handleExtensionCancelTask serves tasks/cancel on a modern request. Whether
// and how the task stops is its handler's business.
func (s *MCPServer) handleExtensionCancelTask(ctx context.Context, id any, request mcp.ExtensionTaskRequest) (*mcp.Result, *requestError) {
	ref, reqErr := s.openTask(ctx, id, request.Header, request.Params.TaskId)
	if reqErr != nil {
		return nil, reqErr
	}
	if err := s.cancelExtensionTask(ctx, ref); err != nil {
		return nil, taskHandlerError(ctx, id, err)
	}
	return &mcp.Result{}, nil
}

// handleExtensionUpdateTask serves tasks/update on a modern request: the
// responses go to a [TaskInputHandler]; a handler that takes no input is asked
// for the task's state, so a task it no longer knows answers gone.
func (s *MCPServer) handleExtensionUpdateTask(ctx context.Context, id any, request mcp.UpdateExtensionTaskRequest) (*mcp.Result, *requestError) {
	ref, reqErr := s.openTask(ctx, id, request.Header, request.Params.TaskId)
	if reqErr != nil {
		return nil, reqErr
	}
	var err error
	if input, ok := s.taskHandler.(TaskInputHandler); ok {
		err = updateExtensionTask(ctx, input, ref, request.Params.InputResponses)
	} else {
		_, err = s.extensionTaskState(ctx, ref)
	}
	if err != nil {
		return nil, taskHandlerError(ctx, id, err)
	}
	return &mcp.Result{}, nil
}

// openTask starts every task method: the request must declare the Tasks
// extension, and taskID must open for the request's caller.
func (s *MCPServer) openTask(ctx context.Context, id any, header http.Header, taskID string) (TaskRef, *requestError) {
	s.warnUnboundCaller(ctx, header)
	if clientLacks(RequestProtocolInfoFromContext(ctx), tasksDeclared) {
		required := mcp.ClientCapabilities{Extensions: map[string]any{mcp.ExtensionTasks: map[string]any{}}}
		return TaskRef{}, handlerError(ctx, id, mcp.MissingRequiredClientCapabilityError{Required: required})
	}
	caller := s.taskCallerOf(ctx)
	handlerID, err := s.taskSealer.open(taskID, caller)
	if err != nil {
		if errors.Is(err, errTaskGone) {
			return TaskRef{}, taskGone(id, err)
		}
		return TaskRef{}, &requestError{id: id, code: mcp.INVALID_PARAMS, err: err, message: &taskUnknownMessage, data: &taskUnknownData}
	}
	return TaskRef{TaskID: taskID, HandlerID: handlerID, Caller: caller}, nil
}

// taskGone answers a task id that is gone; err is why, kept for the hooks.
func taskGone(id any, err error) *requestError {
	return &requestError{id: id, code: mcp.INVALID_PARAMS, err: err, message: &taskGoneMessage, data: &taskGoneData}
}

// taskHandlerError answers err, from a [TaskHandler] method: [ErrTaskGone] is
// gone, and any other error follows the handler-error rules.
func taskHandlerError(ctx context.Context, id any, err error) *requestError {
	if errors.Is(err, ErrTaskGone) {
		return taskGone(id, err)
	}
	return handlerError(ctx, id, err)
}

// warnUnboundCaller logs, once per server, that a task request carries
// Authorization while no [WithTaskCaller] binds task ids. It is a diagnostic,
// not a check: it misses authentication carried in other ways.
func (s *MCPServer) warnUnboundCaller(ctx context.Context, header http.Header) {
	if s.taskCaller != nil || header.Get("Authorization") == "" {
		return
	}
	s.unboundCallerWarned.Do(func() {
		logger := s.requestLogger
		if logger == nil {
			logger = slog.Default()
		}
		logger.WarnContext(ctx, unboundCallerWarning)
	})
}

// extensionTaskState is the handler's TaskState, a panic in it recovered into
// an error.
func (s *MCPServer) extensionTaskState(ctx context.Context, ref TaskRef) (state TaskState, err error) {
	defer recoverTaskHandler(&err)
	return s.taskHandler.TaskState(ctx, ref)
}

// cancelExtensionTask is the handler's CancelTask, a panic in it recovered
// into an error.
func (s *MCPServer) cancelExtensionTask(ctx context.Context, ref TaskRef) (err error) {
	defer recoverTaskHandler(&err)
	return s.taskHandler.CancelTask(ctx, ref)
}

// updateExtensionTask is input's UpdateTask, a panic in it recovered into an
// error.
func updateExtensionTask(ctx context.Context, input TaskInputHandler, ref TaskRef, responses mcp.InputResponses) (err error) {
	defer recoverTaskHandler(&err)
	return input.UpdateTask(ctx, ref, responses)
}

// recoverTaskHandler, deferred by a call into a [TaskHandler], turns a panic
// there into err.
func recoverTaskHandler(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("%w: %v", errTaskHandlerPanic, r)
	}
}

// renderTaskState renders state, what the handler reports for taskID, as the
// answer to tasks/get, with the member its status calls for.
func (s *MCPServer) renderTaskState(ctx context.Context, id any, taskID string, state *TaskState) (*mcp.ExtensionTaskStateResult, *requestError) {
	updated := state.LastUpdatedAt
	if updated.Before(state.CreatedAt) {
		updated = state.CreatedAt
	}
	result := &mcp.ExtensionTaskStateResult{
		TaskId:         taskID,
		Status:         state.Status,
		StatusMessage:  state.StatusMessage,
		CreatedAt:      taskTimestamp(state.CreatedAt),
		LastUpdatedAt:  taskTimestamp(updated),
		TTLMs:          taskTTLMs(state.TTL),
		PollIntervalMs: taskPollIntervalMs(state.PollInterval),
	}
	switch state.Status {
	case mcp.TaskStatusWorking, mcp.TaskStatusCancelled:
	case mcp.TaskStatusCompleted:
		completed, err := s.completedTaskResult(state)
		if err != nil {
			return nil, internalError(id, err)
		}
		result.TaskResult = completed
	case mcp.TaskStatusFailed:
		if state.Err == nil {
			return nil, internalError(id, errTaskFailedWithoutError)
		}
		details := handlerError(ctx, id, state.Err).ToJSONRPCError().Error
		result.Error = &details
		if result.StatusMessage == "" {
			result.StatusMessage = failedTaskMessage
		}
	case mcp.TaskStatusInputRequired:
		if _, ok := s.taskHandler.(TaskInputHandler); !ok {
			return nil, internalError(id, errTaskInputUnserved)
		}
		required := requiredClientCapabilities(state.InputRequests)
		if clientLacks(RequestProtocolInfoFromContext(ctx), required) {
			return nil, handlerError(ctx, id, mcp.MissingRequiredClientCapabilityError{Required: required})
		}
		result.InputRequests = state.InputRequests
	default:
		return nil, internalError(id, fmt.Errorf("%w: %q", errTaskUnknownStatus, state.Status))
	}
	return result, nil
}

// completedTaskResult is the result of a completed task as tasks/get answers
// it: a copy of the handler's, validated against the output schema and
// limited to the size of the call it was handed over from, with resultType
// complete. A validation failure is an isError result, as for a synchronous
// call.
func (s *MCPServer) completedTaskResult(state *TaskState) (*mcp.CallToolResult, error) {
	switch {
	case state.Result == nil:
		return nil, errTaskNoResult
	case state.Result.Task != nil || state.Result.ResultType == mcp.ResultTypeTask:
		return nil, errTaskNestedTask
	case state.Result.NeedsInput():
		return nil, errTaskResultNeedsInput
	case state.Origin.IsZero():
		return nil, errTaskZeroOrigin
	}
	origin := &state.Origin
	copied := *state.Result
	result := &copied
	if _, err := s.outputValidator.validate(origin.tool, origin.outputSchema, origin.outputSchemaKept, result); err != nil {
		result = validationToolResult(err)
	}
	result = limitToolResultTo(result, origin.sizeLimit)
	result.ResultType = mcp.ResultTypeComplete
	return result, nil
}
