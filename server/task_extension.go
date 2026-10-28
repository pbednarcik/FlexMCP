package server

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"time"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// ErrTaskGone is what a [TaskHandler] returns for an authenticated task id it
// no longer knows, evicted or past its TTL; the client is told "gone". The
// expiry check is the handler's, since the id carries no TTL.
var ErrTaskGone = errors.New("task gone")

// Errors [NewTaskResult] returns when a call cannot be handed over.
var (
	// ErrTasksNotDeclared means the request did not declare the Tasks
	// extension: answer synchronously.
	ErrTasksNotDeclared = errors.New("the request did not declare the " + mcp.ExtensionTasks + " extension")
	// ErrNotModernRequest means the request is not a 2026-07-28 one; it
	// matches ErrTasksNotDeclared too.
	ErrNotModernRequest = fmt.Errorf("a task needs a 2026-07-28 request: %w", ErrTasksNotDeclared)
	// ErrNoTaskHandler means the server has no [TaskHandler].
	ErrNoTaskHandler = errors.New("the server has no task handler: pass server.WithTaskHandler")
	// ErrEmptyHandlerID means NewTaskResult was given no handler id.
	ErrEmptyHandlerID = errors.New("a task needs a non-empty handler id")
	// ErrNotToolCall means the context is not a tool handler's.
	ErrNotToolCall = errors.New("a task needs the context of the tool call it hands over")
)

var (
	errTaskNotSealed        = errors.New("its id was not minted by NewTaskResult for this caller")
	errTaskWithoutTask      = errors.New("its resultType is task but it carries no task")
	errLegacyTaskOnModern   = errors.New("it is a 2025-11-25 task, on a 2026-07-28 request")
	errLegacyTaskResultType = errors.New("it is a 2025-11-25 task, which carries no resultType")
	errTaskAnswerIsTask     = errors.New("only tools/call is answered with a task")
	errTaskAnswerIncomplete = errors.New("its resultType is not complete")
	errTaskResultIsTask     = errors.New("the result it carries is itself a task")
	errTaskResultAsksInput  = errors.New("the result it carries asks for input")
)

// TaskHandler owns a server's tasks under the Tasks extension
// (io.modelcontextprotocol/tasks, protocol version 2026-07-28). FlexMCP calls
// it after authenticating the task id and stores nothing it returns. A server
// has one, set with [WithTaskHandler]; it tells tools apart by its own ids or
// by [TaskOrigin.Tool].
//
// FlexMCP cannot check these contracts, so a handler keeps them:
//
//   - Terminal statuses never change.
//   - The registry knows the handler id before NewTaskResult returns, so an
//     immediate tasks/get succeeds.
//   - A multi-round-trip input exchange is settled before the hand-over.
//   - An [mcp.InputRequests] key is unique over the whole task, never reused.
//   - A handler id is never reused for another job, or another incarnation of
//     a job, while a key that can verify it is in use, across restarts and
//     instances under a configured [WithTaskKey] too. Several task ids may
//     name one live job.
//   - Every [TaskState] of a task reports the same CreatedAt and TTL.
//   - The methods are safe for concurrent use.
//
// A handler that outlives its server can be asked about ids no client can
// still use; [WithTaskKey] says when ids survive a rebuild.
//
// A method returns [ErrTaskGone] for an id it no longer knows. Any other error
// is answered as a tool handler's is: a [NewHandlerError] in its own JSON-RPC
// form, an [mcp.MissingRequiredClientCapabilityError] as -32021, and anything
// else as an internal error whose message stays on the server. A panic is
// recovered into that internal error. Each method's context carries the
// request information a tool call's does.
type TaskHandler interface {
	// TaskState renders the task as it is now. A completed state carries the
	// Origin it was handed over with; a zero one is an internal error.
	TaskState(ctx context.Context, ref TaskRef) (TaskState, error)
	// CancelTask asks the task to stop; a terminal task is left as it is.
	CancelTask(ctx context.Context, ref TaskRef) error
}

// TaskInputHandler is a [TaskHandler] that takes input in the middle of a
// task. UpdateTask trusts nothing, since the responses come from the client:
// it accepts answers only for input requests still open on the task, decodes
// each with [mcp.InputResponses.DecodeFor] against the request it answers,
// and ignores keys never issued, already answered or superseded, so an answer
// arriving twice takes effect once.
type TaskInputHandler interface {
	UpdateTask(ctx context.Context, ref TaskRef, responses mcp.InputResponses) error
}

// TaskRef is an authenticated task id: what the seal carried.
type TaskRef struct {
	// TaskID is the canonical id the client holds; it is distinct per task.
	TaskID    string
	HandlerID string
	// Caller is the caller the id was sealed for, "" without [WithTaskCaller].
	Caller string
}

// TaskState is a task as its [TaskHandler] renders it now.
type TaskState struct {
	Origin TaskOrigin
	// CreatedAt is the time NewTaskResult reported.
	CreatedAt time.Time
	// TTL is the TTL reported at creation, nil for unlimited.
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

// TaskOrigin is an immutable, opaque snapshot of a tool call: the tool, its
// output schema and its result-size limit. A handler returns it in every
// [TaskState], so a completed result is checked against the call that made it.
// A kept origin keeps its tool's registration reachable, handler included,
// after the tool is registered again or deleted.
type TaskOrigin struct {
	// tool is the tool as registered when the call ran, shared with every
	// call of that registration and never written to: registering the name
	// again stores a new one, and the origin keeps this.
	tool             *mcp.Tool
	outputSchema     *jsonschema.Schema
	outputSchemaKept bool
	sizeLimit        int
}

// Tool returns the name of the snapshotted tool, "" for a zero origin.
func (o TaskOrigin) Tool() string {
	if o.tool == nil {
		return ""
	}
	return o.tool.Name
}

// IsZero reports whether the origin is the zero value, which names no call.
func (o TaskOrigin) IsZero() bool {
	return o.tool == nil && o.outputSchema == nil && !o.outputSchemaKept && o.sizeLimit == 0
}

// WithTaskHandler sets the server's [TaskHandler] and advertises the Tasks
// extension on server/discover, never on the legacy initialize answer. The
// entry is derived after every option has run, so option order cannot drop
// it; without a handler, a Tasks entry given through [WithExtensions] panics.
func WithTaskHandler(h TaskHandler) ServerOption {
	return func(s *MCPServer) {
		s.taskHandler = h
	}
}

// deriveTaskAdvertisement settles the Tasks entry once every option has run.
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

// WithTaskResultPollInterval suggests how long the client waits between polls:
// two seconds is 2*time.Second, never 2000, which is two microseconds. It is
// sent in whole milliseconds, rounded up; zero or less sends none.
func WithTaskResultPollInterval(d time.Duration) TaskResultOption {
	return func(c *taskResultConfig) { c.pollInterval = &d }
}

// WithTaskResultTTL reports how long, from creation, the task is kept;
// without it the TTL is unlimited, sent as null. It is sent in whole
// milliseconds, truncated, and below zero as zero, expired at once.
func WithTaskResultTTL(d time.Duration) TaskResultOption {
	return func(c *taskResultConfig) { c.ttl = &d }
}

// WithTaskResultStatusMessage sets the task's status message.
func WithTaskResultStatusMessage(msg string) TaskResultOption {
	return func(c *taskResultConfig) { c.statusMessage = msg }
}

// WithTaskResultCreatedAt reports t, not now, as the creation time, for a job
// already running. A t after now is also the task's lastUpdatedAt.
func WithTaskResultCreatedAt(t time.Time) TaskResultOption {
	return func(c *taskResultConfig) { c.createdAt = t }
}

// NewTaskResult hands the tool call ctx belongs to over as a task: the tool
// handler returns its result, and the client polls the task with tasks/get.
// ctx is the context the tool handler received, and the server's
// [TaskHandler] must know handlerID before NewTaskResult returns. The task id
// is minted from handlerID, at most 54 bytes, and bound to the caller
// [WithTaskCaller] resolves. The id is authenticated, not encrypted: whoever
// holds it can read handlerID, so a handler id must not carry secrets.
//
// It fails, and the handler answers in another way, when:
//   - handlerID is empty: [ErrEmptyHandlerID];
//   - the server has no TaskHandler: [ErrNoTaskHandler];
//   - the request is not a 2026-07-28 one: [ErrNotModernRequest];
//   - the request did not declare the extension: [ErrTasksNotDeclared], which
//     [ErrNotModernRequest] matches too and a synchronous fallback tests for;
//   - ctx is not a tool call's: [ErrNotToolCall];
//   - handlerID is longer than 54 bytes: [ErrTaskIDTooLong].
//
// The request's context ends when the handler returns: run the handed-over
// work under [DetachTaskContext].
func NewTaskResult(ctx context.Context, handlerID string, opts ...TaskResultOption) (*mcp.CallToolResult, error) {
	var cfg taskResultConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	return newTaskResult(ctx, handlerID, cfg)
}

// newTaskResult is [NewTaskResult] with its options applied to cfg.
func newTaskResult(ctx context.Context, handlerID string, cfg taskResultConfig) (*mcp.CallToolResult, error) {
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

func taskTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

func taskTTLMs(d *time.Duration) *int64 {
	if d == nil {
		return nil
	}
	ms := max(d.Milliseconds(), 0)
	return &ms
}

// maxPollIntervalMs is the longest pollIntervalMs sent: the most whole
// milliseconds a time.Duration holds.
const maxPollIntervalMs = math.MaxInt64 / int64(time.Millisecond)

func taskPollIntervalMs(d *time.Duration) *int64 {
	if d == nil || *d <= 0 {
		return nil
	}
	// Divided first, so the longest duration does not overflow, and rounded
	// up no further than the longest interval a client can turn back into a
	// time.Duration.
	ms := int64(*d / time.Millisecond)
	if *d%time.Millisecond != 0 && ms < maxPollIntervalMs {
		ms++
	}
	return &ms
}

// taskCallerOf is the caller a task id is bound to for ctx, the dispatcher's
// context before any tool middleware: "" without [WithTaskCaller].
func (s *MCPServer) taskCallerOf(ctx context.Context) string {
	if s.taskCaller == nil {
		return ""
	}
	return s.taskCaller(ctx)
}

type taskCallKey struct{}

// taskCall stamps a tool call that could become a task, in one allocation. Its
// parent is the dispatcher's context, which the caller is resolved on.
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

// DetachTaskContext returns the context for work a tool call hands over: it
// keeps ctx's values, the request's protocol information among them, and
// drops only its cancellation, its client session and its task stamp. Nothing the work sends reaches the client: a notification fails
// with [ErrNotificationNotInitialized]. Under it [NewTaskResult] fails with
// [ErrNotToolCall] and [MemoryTasks.Run] runs at once, so the work hands over
// no second task. Take [TaskOriginFromContext] from ctx.
func DetachTaskContext(ctx context.Context) context.Context {
	return &detachedTaskContext{Context: context.WithoutCancel(ctx)}
}

type detachedTaskKey struct{}

type detachedTaskContext struct{ context.Context }

func (c *detachedTaskContext) Value(key any) any {
	switch key.(type) {
	case taskCallKey, clientSessionKey:
		return nil
	case detachedTaskKey:
		return c
	}
	return c.Context.Value(key)
}

func taskCallFromContext(ctx context.Context) *taskCall {
	call, _ := ctx.Value(taskCallKey{}).(*taskCall)
	return call
}

// TaskOriginFromContext returns the origin of the tool call ctx belongs to,
// zero unless the call could become a task: the server has a [TaskHandler] and
// the request is a 2026-07-28 one that declared the Tasks extension.
func TaskOriginFromContext(ctx context.Context) TaskOrigin {
	if call := taskCallFromContext(ctx); call != nil {
		return call.origin
	}
	return TaskOrigin{}
}

func (s *MCPServer) withTaskOrigin(ctx context.Context, tool *mcp.Tool, outputSchema *jsonschema.Schema, outputSchemaKept bool) context.Context {
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
// middleware and every hook, for each shape the call answers with: a task of
// the extension reaches the wire only with an id this server minted for ctx's
// caller, on a 2026-07-28 request that declared the extension; a 2025-11-25
// task only on a legacy request, without the resultType that revision does
// not have. Anything else is an internal error, after OnSuccess, not OnError.
func (s *MCPServer) guardToolCallResult(ctx context.Context, id any, result any) *requestError {
	var reason error
	switch res := result.(type) {
	case *mcp.CallToolResult:
		if res == nil || (res.Task == nil && res.ResultType != mcp.ResultTypeTask) {
			return nil
		}
		reason = s.extensionTaskRefusal(ctx, res.Task)
	case *mcp.CreateTaskResult:
		if res == nil {
			return nil
		}
		switch {
		case IsModernRequest(ctx):
			reason = errLegacyTaskOnModern
		case res.ResultType != "":
			reason = errLegacyTaskResultType
		}
	}
	if reason == nil {
		return nil
	}
	return &requestError{id: id, code: mcp.INTERNAL_ERROR, err: fmt.Errorf("tools/call refused a task answer: %w", reason)}
}

// extensionTaskRefusal is why task, a tools/call answer's, may not reach the
// wire, nil when it may.
func (s *MCPServer) extensionTaskRefusal(ctx context.Context, task *mcp.ExtensionTask) error {
	info := RequestProtocolInfoFromContext(ctx)
	declared := false
	if info != nil && info.ClientCapabilities != nil {
		_, declared = info.ClientCapabilities.Extensions[mcp.ExtensionTasks]
	}
	switch {
	case task == nil:
		return errTaskWithoutTask
	case info == nil || !info.Modern:
		return ErrNotModernRequest
	case !declared:
		return ErrTasksNotDeclared
	}
	if _, err := s.taskSealer.open(task.TaskId, s.taskCallerOf(ctx)); err != nil {
		return errTaskNotSealed
	}
	return nil
}

// guardTaskMethodResult is the last check on the answer to tasks/get,
// tasks/cancel and tasks/update, after every hook, in each era: only tools/call
// is answered with a task, so the answer is complete, or left to be stamped
// complete, and the result a tasks/get answer carries is neither a task nor a
// request for input. Anything else is an internal error, after OnSuccess, not
// OnError.
func (*MCPServer) guardTaskMethodResult(_ context.Context, id any, result any) *requestError {
	var reason error
	switch res := result.(type) {
	case *mcp.ExtensionTaskStateResult:
		if res != nil {
			reason = taskStateRefusal(res)
		}
	case *mcp.Result:
		if res != nil {
			reason = taskAnswerRefusal(res.ResultType)
		}
	case *mcp.GetTaskResult:
		if res != nil {
			reason = taskAnswerRefusal(res.ResultType)
		}
	case *mcp.CancelTaskResult:
		if res != nil {
			reason = taskAnswerRefusal(res.ResultType)
		}
	}
	if reason == nil {
		return nil
	}
	return &requestError{id: id, code: mcp.INTERNAL_ERROR, err: fmt.Errorf("a task method refused its answer: %w", reason)}
}

// taskAnswerRefusal is why a task method's answer of resultType may not reach
// the wire, nil when it may: it is complete, or unset, which a modern answer
// is stamped complete from and a legacy one leaves out.
func taskAnswerRefusal(resultType mcp.ResultType) error {
	switch resultType {
	case "", mcp.ResultTypeComplete:
		return nil
	case mcp.ResultTypeTask:
		return errTaskAnswerIsTask
	default:
		return errTaskAnswerIncomplete
	}
}

// taskStateRefusal is why state, a tasks/get answer, may not reach the wire,
// nil when it may.
func taskStateRefusal(state *mcp.ExtensionTaskStateResult) error {
	if err := taskAnswerRefusal(state.ResultType); err != nil {
		return err
	}
	switch inner := state.TaskResult; {
	case inner == nil:
		return nil
	case inner.Task != nil || inner.ResultType == mcp.ResultTypeTask:
		return errTaskResultIsTask
	case inner.NeedsInput():
		return errTaskResultAsksInput
	}
	return nil
}
