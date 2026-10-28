package main

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"time"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
)

// registry is a server.TaskHandler over a table of jobs of its own. A server
// that already runs jobs, with ids and a state to report, does the same: the
// task id FlexMCP mints carries the job's id and nothing else.
//
// This one keeps every job for good and recovers no panic in the work; a real
// registry evicts the jobs it reports as gone, and server.MemoryTasks does both.
type registry struct {
	mu   sync.Mutex
	jobs map[string]*job
}

// pollEvery is how long a client waits between polls of a job. The hand-over
// suggests it, and every state reports it again.
const pollEvery = 2 * time.Second

// job is one handed-over call.
type job struct {
	origin  server.TaskOrigin
	created time.Time
	cancel  context.CancelFunc
	status  mcp.TaskStatus
	result  *mcp.CallToolResult
	err     error
}

var _ server.TaskHandler = (*registry)(nil)

func newRegistry() *registry {
	return &registry{jobs: make(map[string]*job)}
}

// TaskState renders the job as it is now. A job the registry does not hold is
// gone.
func (r *registry) TaskState(_ context.Context, ref server.TaskRef) (server.TaskState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	job, ok := r.jobs[ref.HandlerID]
	if !ok {
		return server.TaskState{}, server.ErrTaskGone
	}
	poll := pollEvery
	return server.TaskState{
		Origin:       job.origin,
		CreatedAt:    job.created,
		Status:       job.status,
		PollInterval: &poll,
		Result:       job.result,
		Err:          job.err,
	}, nil
}

// CancelTask stops a job that is still working. Cancelling one that has ended
// changes nothing.
func (r *registry) CancelTask(_ context.Context, ref server.TaskRef) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	job, ok := r.jobs[ref.HandlerID]
	if !ok {
		return server.ErrTaskGone
	}
	if job.status == mcp.TaskStatusWorking {
		job.status = mcp.TaskStatusCancelled
		job.cancel()
	}
	return nil
}

// start hands the tool call ctx belongs to over as a job that runs work, and
// returns the task that stands for it. When the client cannot take a task, it
// runs work right here instead and returns its result.
func (r *registry) start(ctx context.Context, work func(context.Context) (*mcp.CallToolResult, error)) (*mcp.CallToolResult, error) {
	id := rand.Text()
	created := time.Now()

	// The request's context ends once the response is out, so the work runs
	// under one detached from it: its values, without its cancellation or its
	// session.
	workCtx, cancel := context.WithCancel(server.DetachTaskContext(ctx))

	// The registry knows the job before NewTaskResult returns, so the client's
	// first tasks/get finds it.
	r.mu.Lock()
	r.jobs[id] = &job{
		origin:  server.TaskOriginFromContext(ctx),
		created: created,
		cancel:  cancel,
		status:  mcp.TaskStatusWorking,
	}
	r.mu.Unlock()

	handedOver, err := server.NewTaskResult(ctx, id,
		server.WithTaskResultCreatedAt(created),
		server.WithTaskResultPollInterval(pollEvery),
	)
	if err != nil {
		r.mu.Lock()
		delete(r.jobs, id)
		r.mu.Unlock()
		cancel()

		if errors.Is(err, server.ErrTasksNotDeclared) {
			return work(ctx)
		}
		return nil, err
	}

	go r.run(workCtx, id, work)
	return handedOver, nil
}

// run does the work of a job and records how it ended.
func (r *registry) run(ctx context.Context, id string, work func(context.Context) (*mcp.CallToolResult, error)) {
	result, err := work(ctx)
	if err == nil && result == nil {
		err = errors.New("the work returned no result")
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// The ended job lets go of its cancellation, which holds the request's
	// context.
	job := r.jobs[id]
	job.cancel()
	job.cancel = func() {}
	if job.status != mcp.TaskStatusWorking {
		// Cancelled meanwhile: a terminal status never changes.
		return
	}
	if err != nil {
		job.status, job.err = mcp.TaskStatusFailed, err
		return
	}
	job.status, job.result = mcp.TaskStatusCompleted, result
}

// principalKey carries the caller's identity in a request's context.
type principalKey struct{}

// principalOf names the authenticated caller of a request: what the transport
// put in its context, for example through server.WithHTTPContextFunc, from a
// verified token. It is "" for an unauthenticated request, and so for every
// request of this demo, which sets no principal.
func principalOf(ctx context.Context) string {
	principal, _ := ctx.Value(principalKey{}).(string)
	return principal
}

// newGuardedServer serves an "export" tool whose tasks live in a registry. Its
// task ids are authenticated with key, which comes from configuration and is
// the same on every instance and after every restart, so each opens the ids the
// others minted. The registry lives in memory, so a restarted server still
// answers "gone": ids survive a restart only if the jobs do. The ids are bound
// to the caller, so only the caller that made a task can poll it.
func newGuardedServer(key []byte) *server.MCPServer {
	reg := newRegistry()

	srv := server.NewMCPServer("export-server", "1.0.0",
		server.WithToolCapabilities(true),
		server.WithTaskHandler(reg),
		server.WithTaskKey(key),
		server.WithTaskNamespace("export"),
		server.WithTaskCaller(principalOf),
	)

	srv.AddTool(
		mcp.NewTool("export", mcp.WithDescription("exports the data set")),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return reg.start(ctx, export)
		},
	)

	return srv
}

// export is the work of the "export" tool. It must return when ctx ends.
func export(ctx context.Context) (*mcp.CallToolResult, error) {
	select {
	case <-time.After(5 * time.Second):
		return mcp.NewToolResultText("exported"), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
