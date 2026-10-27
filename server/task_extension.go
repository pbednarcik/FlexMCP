package server

import (
	"context"
	"errors"
	"fmt"
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

// Errors [NewTaskResult] returns when a call cannot be handed over.
var (
	// ErrTasksNotDeclared means the request did not declare the Tasks
	// extension, so the client cannot take a task: answer synchronously.
	ErrTasksNotDeclared = errors.New("the request did not declare the " + mcp.ExtensionTasks + " extension")
	// ErrNotModernRequest means the request is not a 2026-07-28 one. A legacy
	// request never declares the extension, so it also matches
	// ErrTasksNotDeclared.
	ErrNotModernRequest = fmt.Errorf("a task needs a 2026-07-28 request: %w", ErrTasksNotDeclared)
	// ErrNoTaskHandler means the server has no [TaskHandler].
	ErrNoTaskHandler = errors.New("the server has no task handler: pass server.WithTaskHandler")
	// ErrEmptyHandlerID means NewTaskResult was given no handler id.
	ErrEmptyHandlerID = errors.New("a task needs a non-empty handler id")
	// ErrNotToolCall means NewTaskResult was given a context that is not a
	// tool call's, as a tool handler receives it.
	ErrNotToolCall = errors.New("a task needs the context of the tool call it hands over")
)

var (
	errTaskNotSealed   = errors.New("its id was not minted by NewTaskResult for this caller")
	errTaskWithoutTask = errors.New("its resultType is task but it carries no task")
)

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

// TaskResultOption sets a field of the task [NewTaskResult] answers.
type TaskResultOption func(*taskResultConfig)

type taskResultConfig struct {
	pollInterval  *time.Duration
	ttl           *time.Duration
	statusMessage string
	createdAt     time.Time
}

// WithTaskResultPollInterval suggests how long the client waits between polls.
// d is a [time.Duration], so two seconds is 2*time.Second, never 2000, which is
// two microseconds. It is sent in whole milliseconds, rounded up; zero or less
// sends none.
func WithTaskResultPollInterval(d time.Duration) TaskResultOption {
	return func(c *taskResultConfig) { c.pollInterval = &d }
}

// WithTaskResultTTL reports how long, from creation, the task is kept. It is
// sent in whole milliseconds, truncated, and less than zero is sent as zero,
// expired at once. Without it the TTL is unlimited, sent as null. Every
// [TaskState] of the task reports the same TTL.
func WithTaskResultTTL(d time.Duration) TaskResultOption {
	return func(c *taskResultConfig) { c.ttl = &d }
}

// WithTaskResultStatusMessage sets the task's status message.
func WithTaskResultStatusMessage(msg string) TaskResultOption {
	return func(c *taskResultConfig) { c.statusMessage = msg }
}

// WithTaskResultCreatedAt reports t as the task's creation time in place of
// now, for a handler that hands over a job already running. A t after now is
// also the task's lastUpdatedAt, so the task never changed before it was
// created. Every [TaskState] of the task reports the same CreatedAt.
func WithTaskResultCreatedAt(t time.Time) TaskResultOption {
	return func(c *taskResultConfig) { c.createdAt = t }
}

// NewTaskResult hands the tool call ctx belongs to over as a task: a tool
// handler returns its result, and the client polls the task with tasks/get.
// ctx is the context the tool handler received. The server's [TaskHandler]
// must know handlerID before NewTaskResult returns. The task id is minted
// from handlerID, which has room for 54 bytes, and bound to the caller
// [WithTaskCaller] resolved for the request.
//
// It fails, and the handler answers in another way, when:
//   - handlerID is empty: [ErrEmptyHandlerID];
//   - the server has no TaskHandler: [ErrNoTaskHandler];
//   - the request is not a 2026-07-28 one: [ErrNotModernRequest];
//   - the request did not declare the extension: [ErrTasksNotDeclared], which
//     [ErrNotModernRequest] matches too; a handler that falls back to a
//     synchronous answer tests for it;
//   - ctx is not a tool call's: [ErrNotToolCall];
//   - handlerID is longer than 54 bytes: [ErrTaskIDTooLong].
//
// The request's context ends when the handler returns, so the handed-over
// work runs under a context of its own.
func NewTaskResult(ctx context.Context, handlerID string, opts ...TaskResultOption) (*mcp.CallToolResult, error) {
	if handlerID == "" {
		return nil, ErrEmptyHandlerID
	}
	call := taskCallFromContext(ctx)
	if call == nil {
		return nil, whyNoTaskCall(ctx)
	}
	taskID, err := call.srv.taskSealer.seal(handlerID, call.srv.taskCallerOf(call.Context))
	if err != nil {
		return nil, err
	}
	var cfg taskResultConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	now := time.Now()
	if cfg.createdAt.IsZero() {
		cfg.createdAt = now
	}
	updated := now
	if cfg.createdAt.After(now) {
		updated = cfg.createdAt
	}
	return &mcp.CallToolResult{Task: &mcp.ExtensionTask{
		TaskId:         taskID,
		Status:         mcp.TaskStatusWorking,
		StatusMessage:  cfg.statusMessage,
		CreatedAt:      taskTimestamp(cfg.createdAt),
		LastUpdatedAt:  taskTimestamp(updated),
		TTLMs:          taskTTLMs(cfg.ttl),
		PollIntervalMs: taskPollIntervalMs(cfg.pollInterval),
	}}, nil
}

// whyNoTaskCall names why ctx carries no task stamp.
func whyNoTaskCall(ctx context.Context) error {
	s := ServerFromContext(ctx)
	switch {
	case s == nil:
		return ErrNotToolCall
	case s.taskHandler == nil:
		return ErrNoTaskHandler
	case !IsModernRequest(ctx):
		return ErrNotModernRequest
	case !ClientDeclaresExtension(ctx, mcp.ExtensionTasks):
		return ErrTasksNotDeclared
	default:
		return ErrNotToolCall
	}
}

// taskTimestamp spells t in RFC 3339, in UTC, to the millisecond.
func taskTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// taskTTLMs is a task's TTL in whole milliseconds, truncated and at least
// zero; nil, unlimited, for nil.
func taskTTLMs(d *time.Duration) *int64 {
	if d == nil {
		return nil
	}
	ms := max(d.Milliseconds(), 0)
	return &ms
}

// taskPollIntervalMs is a poll interval in whole milliseconds, rounded up;
// nil for nil or for zero or less.
func taskPollIntervalMs(d *time.Duration) *int64 {
	if d == nil || *d <= 0 {
		return nil
	}
	ms := int64((*d + time.Millisecond - 1) / time.Millisecond)
	return &ms
}

// taskCallerOf is the caller a task id is bound to for ctx's request: what
// [WithTaskCaller]'s function returns, "" without one. ctx is the context the
// dispatcher handles the request in, before any tool middleware.
func (s *MCPServer) taskCallerOf(ctx context.Context) string {
	if s.taskCaller == nil {
		return ""
	}
	return s.taskCaller(ctx)
}

// taskCallKey finds a [taskCall] stamp in a context.
type taskCallKey struct{}

// taskCall is the stamp of a tool call that could become a task, made in one
// allocation: the call's server and its origin. Its parent is the
// dispatcher's context, before any tool middleware ran, which the caller is
// resolved on. Every other value, and the cancellation, are its parent's.
type taskCall struct {
	context.Context
	srv    *MCPServer
	origin TaskOrigin
}

func (c *taskCall) Value(key any) any {
	if _, ok := key.(taskCallKey); ok {
		return c
	}
	return c.Context.Value(key)
}

// taskCallFromContext returns ctx's task stamp, nil without one.
func taskCallFromContext(ctx context.Context) *taskCall {
	call, _ := ctx.Value(taskCallKey{}).(*taskCall)
	return call
}

// TaskOriginFromContext returns the origin of the tool call ctx belongs to.
// It is the zero origin unless the call could become a task: the server has a
// [TaskHandler], and the request is a 2026-07-28 one that declared the Tasks
// extension.
func TaskOriginFromContext(ctx context.Context) TaskOrigin {
	if call := taskCallFromContext(ctx); call != nil {
		return call.origin
	}
	return TaskOrigin{}
}

// withTaskOrigin stamps a call to tool into ctx, the dispatcher's context,
// when the call could become a task, and returns ctx untouched otherwise.
func (s *MCPServer) withTaskOrigin(ctx context.Context, tool mcp.Tool, outputSchema *jsonschema.Schema, outputSchemaKept bool) context.Context {
	if s.taskHandler == nil || !ClientDeclaresExtension(ctx, mcp.ExtensionTasks) {
		return ctx
	}
	return &taskCall{Context: ctx, srv: s, origin: TaskOrigin{
		tool:             tool,
		outputSchema:     outputSchema,
		outputSchemaKept: outputSchemaKept,
		sizeLimit:        s.toolResultSizeLimit,
	}}
}

// guardToolCallResult is the last check on a tools/call answer, after the
// middleware and every hook: a task, or a resultType of task, reaches the wire
// only when this server minted its id, for the caller resolved on ctx, on a
// 2026-07-28 request that declared the Tasks extension. Anything else is an
// internal error, answered after the OnSuccess hooks already fired; OnError
// does not fire for it.
func (s *MCPServer) guardToolCallResult(ctx context.Context, id any, result any) *requestError {
	res, ok := result.(*mcp.CallToolResult)
	if !ok || res == nil || (res.Task == nil && res.ResultType != mcp.ResultTypeTask) {
		return nil
	}
	info := RequestProtocolInfoFromContext(ctx)
	declared := false
	if info != nil && info.ClientCapabilities != nil {
		_, declared = info.ClientCapabilities.Extensions[mcp.ExtensionTasks]
	}
	var reason error
	switch {
	case res.Task == nil:
		reason = errTaskWithoutTask
	case info == nil || !info.Modern:
		reason = ErrNotModernRequest
	case !declared:
		reason = ErrTasksNotDeclared
	default:
		if _, err := s.taskSealer.open(res.Task.TaskId, s.taskCallerOf(ctx)); err != nil {
			reason = errTaskNotSealed
		}
	}
	if reason == nil {
		return nil
	}
	return &requestError{id: id, code: mcp.INTERNAL_ERROR, err: fmt.Errorf("tools/call refused a task answer: %w", reason)}
}
