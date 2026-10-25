package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pbednarcik/flexmcp/mcp"
)

var (
	// ErrInvalidFallbackPollInterval is returned when WaitForTask receives a
	// non-positive fallback polling interval.
	ErrInvalidFallbackPollInterval = errors.New("fallback poll interval must be greater than zero")
	// ErrServerPollIntervalOverflow is returned when a server-provided polling
	// interval cannot be represented by time.Duration.
	ErrServerPollIntervalOverflow = errors.New("server poll interval exceeds time.Duration")
)

// WaitForTask polls a task until it reaches a terminal state, needs input
// (input_required, which only the requestor can move on), or the context is
// done. The server-provided poll interval takes precedence over
// fallbackPollInterval.
func (c *Client) WaitForTask(
	ctx context.Context,
	request mcp.GetTaskRequest,
	fallbackPollInterval time.Duration,
) (*mcp.GetTaskResult, error) {
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
	response, err := c.sendRequest(ctx, string(mcp.MethodTasksGet), request.Params, outboundHeader(request.Header, request.Method))
	if err != nil {
		return nil, err
	}

	return mcp.ParseGetTaskResult(response)
}
