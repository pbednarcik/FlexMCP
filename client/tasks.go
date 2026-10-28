package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pbednarcik/flexmcp/mcp"
)

var (
	// ErrInvalidFallbackPollInterval is returned when WaitForTask or
	// WaitForExtensionTask receives a non-positive fallback polling interval.
	ErrInvalidFallbackPollInterval = errors.New("fallback poll interval must be greater than zero")
	// ErrServerPollIntervalOverflow is returned when a server-provided polling
	// interval cannot be represented by time.Duration.
	ErrServerPollIntervalOverflow = errors.New("server poll interval exceeds time.Duration")
	// ErrLegacyTaskMethod is returned by the 2025-11-25 task methods (GetTask,
	// CancelTask, WaitForTask, TaskResult, ListTasks and ListTasksByPage) on a
	// 2026-07-28 connection, where a server no longer answers their shapes.
	ErrLegacyTaskMethod = errors.New("the 2025-11-25 task methods are not served on a 2026-07-28 connection: " +
		"use GetExtensionTask, CancelExtensionTask, UpdateExtensionTask and WaitForExtensionTask")
	// ErrExtensionTasksNotDeclared is returned by the Tasks extension's methods
	// (GetExtensionTask, CancelExtensionTask, UpdateExtensionTask and
	// WaitForExtensionTask) on a client that does not declare the extension: a
	// client declares it by being configured with it in WithClientCapabilities,
	// on a 2026-07-28 connection.
	ErrExtensionTasksNotDeclared = errors.New("the Tasks extension " + mcp.ExtensionTasks +
		" is not declared: it needs a 2026-07-28 connection and WithClientCapabilities with the extension under Extensions")
	// ErrUnknownTaskStatus is returned, with the state, by WaitForExtensionTask
	// when a server answers a status outside the five the extension defines:
	// no later poll would make it terminal.
	ErrUnknownTaskStatus = errors.New("the server answered an unknown task status")
	// ErrInvalidTaskState is returned, wrapped with what is wrong, by
	// GetExtensionTask and WaitForExtensionTask when a server's tasks/get
	// answer breaks the extension's rules.
	ErrInvalidTaskState = errors.New("the server answered an invalid task state")
)

// refuseLegacyTaskMethod returns ErrLegacyTaskMethod on a 2026-07-28
// connection, naming the Go method and the wire method it would have sent.
func (c *Client) refuseLegacyTaskMethod(method string, wire mcp.MCPMethod) error {
	if c.isModern() {
		return fmt.Errorf("%s (%s) was removed in protocol version %s: %w", method, wire, mcp.ProtocolVersion20260728, ErrLegacyTaskMethod)
	}
	return nil
}

// minExtensionPollInterval is the shortest wait WaitForExtensionTask puts
// between polls on a server's pollIntervalMs, against busy polling.
const minExtensionPollInterval = 250 * time.Millisecond

// GetExtensionTask asks for a task of the Tasks extension (protocol version
// 2026-07-28): its status, and as that status requires the call's result, the
// error it failed with or the input it waits for.
//
// The extension is declared by configuring the client with it in
// WithClientCapabilities; without that, and on a connection that is not
// 2026-07-28, it returns ErrExtensionTasksNotDeclared and sends nothing. A
// task id the server does not know, or that is gone, comes back as an
// mcp.TaskRefusedError carrying the server's reason (mcp.IsTaskGone tells a
// gone task, whose call is to be made again), which is still an
// mcp.ErrInvalidParams.
//
// An answer that breaks the extension's rules returns an error wrapping
// ErrInvalidTaskState: the state of another task than taskID, a completed
// task without its result or with one that is a task or not complete, a
// failed task without its error. An answer without resultType is complete.
func (c *Client) GetExtensionTask(ctx context.Context, taskID string) (*mcp.ExtensionTaskStateResult, error) {
	response, err := c.sendExtensionTask(ctx, mcp.MethodTasksGet, mcp.ExtensionTaskParams{TaskId: taskID})
	if err != nil {
		return nil, err
	}
	state, err := mcp.ParseExtensionTaskStateResult(response)
	if err != nil {
		return nil, err
	}
	if err := c.checkResultType(state.ResultType); err != nil {
		return nil, err
	}
	if err := checkTaskState(state, taskID); err != nil {
		return nil, err
	}
	return state, nil
}

// checkTaskState refuses state, the tasks/get answer for taskID, when it
// breaks the extension's rules, naming the problem and neither id.
func checkTaskState(state *mcp.ExtensionTaskStateResult, taskID string) error {
	var problem string
	completed := state.Status == mcp.TaskStatusCompleted
	switch {
	case state.TaskId != taskID:
		problem = "it is the state of another task than the one asked for"
	case completed && state.TaskResult == nil:
		problem = "a completed task carries no result"
	case completed && (state.TaskResult.Task != nil || state.TaskResult.ResultType == mcp.ResultTypeTask):
		problem = "a completed task's result is itself a task"
	case completed && state.TaskResult.ResultType != "" && state.TaskResult.ResultType != mcp.ResultTypeComplete:
		// The extension's own examples leave the inner resultType out.
		problem = "a completed task's result is not complete"
	case state.Status == mcp.TaskStatusFailed && state.Error == nil:
		problem = "a failed task carries no error"
	default:
		return nil
	}
	return fmt.Errorf("%w: %s", ErrInvalidTaskState, problem)
}

// CancelExtensionTask asks the server to cancel a task of the Tasks
// extension. The server only acknowledges: whether and how the task stops is
// its handler's business, so GetExtensionTask shows the outcome. See
// GetExtensionTask for the declaration and the errors.
func (c *Client) CancelExtensionTask(ctx context.Context, taskID string) error {
	response, err := c.sendExtensionTask(ctx, mcp.MethodTasksCancel, mcp.ExtensionTaskParams{TaskId: taskID})
	if err != nil {
		return err
	}
	return c.checkTaskAcknowledgement(response)
}

// UpdateExtensionTask answers the input requests of a task of the Tasks
// extension, by their keys, as GetExtensionTask showed them while the task was
// input_required. See GetExtensionTask for the declaration and the errors.
func (c *Client) UpdateExtensionTask(ctx context.Context, taskID string, responses mcp.InputResponses) error {
	if responses == nil {
		responses = mcp.InputResponses{}
	}
	response, err := c.sendExtensionTask(ctx, mcp.MethodTasksUpdate, mcp.UpdateExtensionTaskParams{TaskId: taskID, InputResponses: responses})
	if err != nil {
		return err
	}
	return c.checkTaskAcknowledgement(response)
}

// WaitForExtensionTask polls a task of the Tasks extension until it reaches a
// terminal status or needs input (input_required, which only the caller can
// move on: it answers through UpdateExtensionTask and waits again), and
// returns that state. The caller reads the outcome from it.
//
// Between polls it waits the server's pollIntervalMs, never less than 250 ms
// and with no ceiling, so a server's minute is respected; a missing interval
// is fallback, which must be greater than zero.
//
// When ctx ends while it waits, it returns ctx.Err() and sends nothing, and
// never tasks/cancel: whether the task stops is the caller's decision. A poll
// in flight is cancelled like any request (over stdio, with
// notifications/cancelled for that request, not for the task), and its error
// wraps the context's.
//
// A status outside the five the extension defines returns the state with an
// error wrapping ErrUnknownTaskStatus that names the status, not the task.
// An answer GetExtensionTask refuses as ErrInvalidTaskState ends the wait.
// See GetExtensionTask for the declaration and the errors.
func (c *Client) WaitForExtensionTask(ctx context.Context, taskID string, fallback time.Duration) (*mcp.ExtensionTaskStateResult, error) {
	if fallback <= 0 {
		return nil, ErrInvalidFallbackPollInterval
	}

	for {
		state, err := c.GetExtensionTask(ctx, taskID)
		if err != nil {
			return nil, fmt.Errorf("get task: %w", err)
		}
		if state.Status.IsTerminal() || state.Status == mcp.TaskStatusInputRequired {
			return state, nil
		}
		if state.Status != mcp.TaskStatusWorking {
			return state, fmt.Errorf("%w: %.64q", ErrUnknownTaskStatus, string(state.Status))
		}

		interval := fallback
		if state.PollIntervalMs != nil && *state.PollIntervalMs > 0 {
			const maxPollIntervalMilliseconds = int64(1<<63-1) / int64(time.Millisecond)
			if *state.PollIntervalMs > maxPollIntervalMilliseconds {
				return nil, ErrServerPollIntervalOverflow
			}
			interval = max(time.Duration(*state.PollIntervalMs)*time.Millisecond, minExtensionPollInterval)
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// requireTasksExtension returns ErrExtensionTasksNotDeclared unless this
// connection declares the Tasks extension.
func (c *Client) requireTasksExtension() error {
	if !c.acceptsTasks() {
		return ErrExtensionTasksNotDeclared
	}
	return nil
}

// sendExtensionTask sends one of the extension's task methods, which carry no
// caller headers: Mcp-Name is written from params.taskId.
func (c *Client) sendExtensionTask(ctx context.Context, method mcp.MCPMethod, params any) (*json.RawMessage, error) {
	if err := c.requireTasksExtension(); err != nil {
		return nil, err
	}
	return c.sendRequest(ctx, string(method), params, nil)
}

// checkTaskAcknowledgement judges the empty result tasks/cancel and
// tasks/update answer with.
func (c *Client) checkTaskAcknowledgement(response *json.RawMessage) error {
	var acknowledgement struct {
		ResultType mcp.ResultType `json:"resultType"`
	}
	if err := json.Unmarshal(*response, &acknowledgement); err != nil {
		return fmt.Errorf("failed to unmarshal response: %w", err)
	}
	return c.checkResultType(acknowledgement.ResultType)
}

// WaitForTask polls a task until it reaches a terminal state, needs input
// (input_required, which only the requestor can move on), or the context is
// done. The server-provided poll interval takes precedence over
// fallbackPollInterval.
func (c *Client) WaitForTask(
	ctx context.Context,
	request mcp.GetTaskRequest,
	fallbackPollInterval time.Duration,
) (*mcp.GetTaskResult, error) {
	if err := c.refuseLegacyTaskMethod("WaitForTask", mcp.MethodTasksGet); err != nil {
		return nil, err
	}
	if fallbackPollInterval <= 0 {
		return nil, ErrInvalidFallbackPollInterval
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		result, err := c.GetTask(ctx, request)
		if err != nil {
			return nil, fmt.Errorf("get task: %w", err)
		}
		if result.Status.IsTerminal() || result.Status == mcp.TaskStatusInputRequired {
			return result, nil
		}

		interval := fallbackPollInterval
		if result.PollInterval != nil && *result.PollInterval > 0 {
			const maxPollIntervalMilliseconds = int64(1<<63-1) / int64(time.Millisecond)
			if *result.PollInterval > maxPollIntervalMilliseconds {
				return nil, ErrServerPollIntervalOverflow
			}
			interval = time.Duration(*result.PollInterval) * time.Millisecond
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// CancelTask returns canceled task result
func (c *Client) CancelTask(
	ctx context.Context,
	request mcp.CancelTaskRequest,
) (*mcp.CancelTaskResult, error) {
	if err := c.refuseLegacyTaskMethod("CancelTask", mcp.MethodTasksCancel); err != nil {
		return nil, err
	}
	response, err := c.sendRequest(ctx, string(mcp.MethodTasksCancel), request.Params, outboundHeader(request.Header, request.Method))
	if err != nil {
		return nil, err
	}

	return mcp.ParseCancelTaskResult(response)
}

// ErrRepeatedTaskListCursor is returned when tasks/list repeats a cursor that
// was already requested. Following it would request the same page again.
var ErrRepeatedTaskListCursor = errors.New("tasks/list cursor did not advance")

// ListTasksByPage lists one page of tasks.
func (c *Client) ListTasksByPage(
	ctx context.Context,
	request mcp.ListTasksRequest,
) (*mcp.ListTasksResult, error) {
	if err := c.refuseLegacyTaskMethod("ListTasksByPage", mcp.MethodTasksList); err != nil {
		return nil, err
	}
	response, err := c.sendRequest(ctx, string(mcp.MethodTasksList), request.Params, outboundHeader(request.Header, request.Method))
	if err != nil {
		return nil, err
	}

	return mcp.ParseListTasksResult(response)
}

// ListTasks lists all tasks by following paginated responses.
func (c *Client) ListTasks(
	ctx context.Context,
	request mcp.ListTasksRequest,
) (*mcp.ListTasksResult, error) {
	if err := c.refuseLegacyTaskMethod("ListTasks", mcp.MethodTasksList); err != nil {
		return nil, err
	}
	seen := seenCursors(request.Params.Cursor)
	result, err := c.ListTasksByPage(ctx, request)
	if err != nil {
		return nil, err
	}
	for result.NextCursor != nil {
		next := *result.NextCursor
		if _, dup := seen[next]; dup {
			return nil, fmt.Errorf("%w: %q", ErrRepeatedTaskListCursor, next)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			seen[next] = struct{}{}
			request.Params.Cursor = result.NextCursor
			page, err := c.ListTasksByPage(ctx, request)
			if err != nil {
				return nil, err
			}
			result.Tasks = append(result.Tasks, page.Tasks...)
			result.NextCursor = page.NextCursor
		}
	}
	return result, nil
}

// TaskResult returns finished task result
func (c *Client) TaskResult(
	ctx context.Context,
	request mcp.TaskResultRequest,
) (*mcp.TaskResultResult, error) {
	if err := c.refuseLegacyTaskMethod("TaskResult", mcp.MethodTasksResult); err != nil {
		return nil, err
	}
	response, err := c.sendRequest(ctx, string(mcp.MethodTasksResult), request.Params, outboundHeader(request.Header, request.Method))
	if err != nil {
		return nil, err
	}

	return mcp.ParseTaskResultResult(response)
}

// GetTask returns task with current status
func (c *Client) GetTask(
	ctx context.Context,
	request mcp.GetTaskRequest,
) (*mcp.GetTaskResult, error) {
	if err := c.refuseLegacyTaskMethod("GetTask", mcp.MethodTasksGet); err != nil {
		return nil, err
	}
	response, err := c.sendRequest(ctx, string(mcp.MethodTasksGet), request.Params, outboundHeader(request.Header, request.Method))
	if err != nil {
		return nil, err
	}

	return mcp.ParseGetTaskResult(response)
}
