package server

import (
	"context"
	"errors"
	"maps"
	"time"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ErrTaskGone is what a [TaskHandler] returns for an authenticated task id it
// no longer knows: evicted, past its TTL, or ended long ago. FlexMCP answers
// "gone" to the client. The expiry check is the handler's, since the id
// carries no TTL.
var ErrTaskGone = errors.New("task gone")

// TaskHandler owns a server's tasks under the Tasks extension
// (io.modelcontextprotocol/tasks, protocol version 2026-07-28). FlexMCP calls
// it after authenticating the task id; it never stores what the handler
// returns. A server has one handler, set with [WithTaskHandler]; a handler
// that serves several tools tells them apart by its own ids, or by
// [TaskOrigin.Tool].
//
// FlexMCP cannot check the contracts below, so a handler must keep them:
//
//   - Terminal statuses never change.
//   - The handler's registry knows the handler id before NewTaskResult
//     returns, so an immediate tasks/get succeeds.
//   - A multi-round-trip input exchange is settled before the handler hands
//     the call over.
//   - An [mcp.InputRequests] key is unique over the whole task and never
//     reused.
//   - A handler id is never reused for another job, or another incarnation of
//     a job, while a key that can verify it is in use. That includes across
//     restarts and instances under a configured [WithTaskKey]. Several task
//     ids may name one live job.
//   - Every [TaskState] of a task reports the same CreatedAt and TTL.
//   - The methods are safe for concurrent use, for one task as for many.
//
// The server's default key and epoch are random and made when the server is
// built. Rebuilding the server, or building a second one, turns the live ids
// of the first into "gone", so a handler that survives a rebuild must expect
// to be asked about ids no client can still use, and a client's task does not
// survive the rebuild unless [WithTaskKey] fixes the key.
//
// A method returns [ErrTaskGone] for an id it no longer knows. Any other
// error answers an internal error whose message stays on the server. A panic
// in a method is recovered into the same internal error on every transport.
// The context of each method carries the request information a tool call's
// does.
type TaskHandler interface {
	// TaskState renders the task as it is now. A completed state carries the
	// Origin it was handed over with; a zero origin there is an internal
	// error.
	TaskState(ctx context.Context, ref TaskRef) (TaskState, error)
	// CancelTask asks the task to stop. Cancelling a task already terminal
	// changes nothing.
	CancelTask(ctx context.Context, ref TaskRef) error
}

// TaskInputHandler is implemented by a [TaskHandler] that takes input in the
// middle of a task.
//
// UpdateTask trusts nothing, since the responses come from the client:
//
//   - It accepts answers only for input requests still open on the task, and
//     decodes each with [mcp.InputResponses.DecodeFor] against the request it
//     answers.
//   - It ignores keys never issued, already answered or superseded, so the
//     same answer arriving twice takes effect once.
type TaskInputHandler interface {
	UpdateTask(ctx context.Context, ref TaskRef, responses mcp.InputResponses) error
}

// TaskRef is an authenticated task id: what the seal carried.
type TaskRef struct {
	// TaskID is the canonical id the client holds; it is distinct per task.
	TaskID    string
	HandlerID string
	// Caller is the caller the id was sealed for, "" when no [WithTaskCaller]
	// is set.
	Caller string
}

// TaskState is a task as its [TaskHandler] renders it now.
type TaskState struct {
	// Origin is the origin the handler was handed at the call.
	Origin TaskOrigin
	// CreatedAt is the time NewTaskResult reported.
	CreatedAt time.Time
	// TTL is how long the task is kept, nil for unlimited. It is the one TTL
	// reported at creation.
	TTL           *time.Duration
	Status        mcp.TaskStatus
	StatusMessage string
	LastUpdatedAt time.Time
	PollInterval  *time.Duration
	// Result is the completed result. FlexMCP only reads it.
	Result        *mcp.CallToolResult
	Err           error
	InputRequests mcp.InputRequests
}

// TaskOrigin is an immutable, opaque snapshot of a tool call: the tool, the
// output schema and the result-size limit in force for it. A handler keeps the
// origin with its job and returns it in every [TaskState], so a completed
// result is validated and size-limited against the call that made it.
type TaskOrigin struct {
	tool             mcp.Tool
	outputSchema     *jsonschema.Schema
	outputSchemaKept bool
	sizeLimit        int
}

// Tool returns the name of the tool the origin snapshots, "" for a zero
// origin.
func (o TaskOrigin) Tool() string {
	return o.tool.Name
}

// IsZero reports whether the origin is the zero value, which names no call.
func (o TaskOrigin) IsZero() bool {
	return o.tool.Name == "" && o.outputSchema == nil && !o.outputSchemaKept && o.sizeLimit == 0
}

// WithTaskHandler sets the server's [TaskHandler] and, with it, advertises the
// Tasks extension (io.modelcontextprotocol/tasks) on server/discover. The
// entry is derived after every option has run, so option order cannot drop
// it. The legacy initialize answer never carries it.
//
// Without a handler, a Tasks entry given through [WithExtensions] panics at
// construction.
func WithTaskHandler(h TaskHandler) ServerOption {
	return func(s *MCPServer) {
		s.taskHandler = h
	}
}

// deriveTaskAdvertisement settles the Tasks entry of capabilities.extensions
// once every option has run: with a handler, server/discover advertises a
// private copy of the map plus the entry, and the map initialize reads loses
// any entry the caller gave; without one, an entry the caller gave panics.
func (s *MCPServer) deriveTaskAdvertisement() {
	_, given := s.capabilities.extensions[mcp.ExtensionTasks]
	if s.taskHandler == nil {
		if given {
			panic("server.WithExtensions: the " + mcp.ExtensionTasks + " entry needs a handler; pass server.WithTaskHandler instead")
		}
		return
	}
	advertised := maps.Clone(s.capabilities.extensions)
	if advertised == nil {
		advertised = make(map[string]any, 1)
	}
	advertised[mcp.ExtensionTasks] = map[string]any{}
	s.discoverExtensions = advertised
	if given {
		delete(s.capabilities.extensions, mcp.ExtensionTasks)
	}
}
