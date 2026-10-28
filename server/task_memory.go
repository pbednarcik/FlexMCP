package server

import (
	"container/list"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/pbednarcik/flexmcp/mcp"
)

const (
	defaultMemoryTasksMaxRunning = 100
	defaultMemoryTasksMaxEnded   = 1000
	defaultMemoryTasksTTL        = time.Hour
)

var (
	// ErrTooManyRunningTasks is what [MemoryTasks.Run] returns when it already
	// runs as many tasks as [WithMemoryTasksMaxRunning] allows.
	ErrTooManyRunningTasks = errors.New("too many running tasks: the limit set by WithMemoryTasksMaxRunning is reached")
	// ErrMemoryTasksClosed is what [MemoryTasks.Run] returns once the store is
	// closed.
	ErrMemoryTasksClosed = errors.New("the task store is closed")

	errNoTaskResult = errors.New("the task's work returned neither a result nor an error")
)

// MemoryTasksOption configures [NewMemoryTasks].
type MemoryTasksOption func(*MemoryTasks)

// WithMemoryTasksMaxRunning limits how many tasks run at once, 100 by default.
// A task counts until its work has returned, not until it is cancelled. It
// panics for n below 1.
func WithMemoryTasksMaxRunning(n int) MemoryTasksOption {
	if n < 1 {
		panic("server.WithMemoryTasksMaxRunning: the limit must be at least 1")
	}
	return func(m *MemoryTasks) { m.maxRunning = n }
}

// WithMemoryTasksMaxEnded limits how many ended tasks are kept for polling,
// 1,000 by default; the one that ended first is evicted first. It panics for n
// below 1.
func WithMemoryTasksMaxEnded(n int) MemoryTasksOption {
	if n < 1 {
		panic("server.WithMemoryTasksMaxEnded: the limit must be at least 1")
	}
	return func(m *MemoryTasks) { m.maxEnded = n }
}

// MemoryTasks is a ready-made, in-memory [TaskHandler] under the Tasks
// extension. Pass it to [WithTaskHandler] and run long work through
// [MemoryTasks.Run] in the tool handlers of that server:
//
//	return mem.Run(ctx, func(ctx context.Context, status func(string)) (*mcp.CallToolResult, error) {
//		...
//	}, server.WithTaskResultTTL(ttl))
//
// Tasks live in this process only. Behind a load balancer, a poll that reaches
// another instance, or a restarted one, answers "gone" even under a shared
// [WithTaskKey]; use a store the instances share for that.
//
// A task is kept for its TTL, one hour unless [WithTaskResultTTL] says
// otherwise, counted from the creation time it reports, [WithTaskResultCreatedAt]
// included. The same TTL is reported to the client and enforced: when it
// passes, the task is forgotten, and its work, if still running, is cancelled.
// A creation time older than the TTL therefore hands over a task that is
// already gone. At most [WithMemoryTasksMaxEnded] ended tasks are
// kept, and [WithMemoryTasksMaxRunning] tasks run at once.
//
// MemoryTasks does not implement [TaskInputHandler]: UpdateTask is not
// available in this release and no task is ever input_required.
//
// It is safe for concurrent use. Whoever builds it owns it and closes it with
// [MemoryTasks.Close]; a server does not. Make it with [NewMemoryTasks].
type MemoryTasks struct {
	maxRunning int
	maxEnded   int

	mu      sync.Mutex
	jobs    map[string]*memoryJob
	ended   list.List // *memoryJob, the one that ended first at the front
	running int       // work functions started and not yet returned
	closed  bool
	workers sync.WaitGroup
}

// memoryJob is one task. cancel and the fields after it are guarded by
// [MemoryTasks.mu]; cancel is dropped once the work returns.
type memoryJob struct {
	id           string
	origin       TaskOrigin
	createdAt    time.Time
	ttl          time.Duration
	pollInterval *time.Duration

	cancel    context.CancelFunc
	status    mcp.TaskStatus
	message   string
	updatedAt time.Time
	result    *mcp.CallToolResult
	err       error
	expiry    *time.Timer
	endedAt   *list.Element
}

// touch records a change of job at now, never before its creation.
func (j *memoryJob) touch(now time.Time) {
	if now.Before(j.createdAt) {
		now = j.createdAt
	}
	j.updatedAt = now
}

var _ TaskHandler = (*MemoryTasks)(nil)

// NewMemoryTasks makes an empty store.
func NewMemoryTasks(opts ...MemoryTasksOption) *MemoryTasks {
	m := &MemoryTasks{
		maxRunning: defaultMemoryTasksMaxRunning,
		maxEnded:   defaultMemoryTasksMaxEnded,
		jobs:       make(map[string]*memoryJob),
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Run does the work of the tool call ctx belongs to. When the call can become
// a task, Run starts fn on a goroutine of its own and returns the answer that
// hands the call over as a task, which the tool handler returns as it is.
// Otherwise Run calls fn at once, on the caller's goroutine and with ctx, and
// returns its result: the request did not declare the Tasks extension, is not a
// 2026-07-28 one, or the server has no [TaskHandler]. A ctx that is not a tool
// call's is an error, and fn does not run.
//
// A handed-over fn runs under [DetachTaskContext] of ctx, which keeps ctx's
// values but not its cancellation or its session: it ends through
// tasks/cancel, the task's TTL or [MemoryTasks.Close], and fn must return
// when it does. Nothing fn sends reaches the client, whose request is
// answered: a notification fails with [ErrNotificationNotInitialized]. status
// sets the task's message while it works; it does nothing when fn runs
// synchronously. A Run called from inside fn runs its function synchronously
// too, and [TaskOriginFromContext] is zero there. fn's result is kept as the
// task's result and must not be changed once fn returns. An error, a panic,
// or a nil result without an error fails the task; the error stays on the
// server unless it is a [NewHandlerError].
//
// opts are those of [NewTaskResult]; without [WithTaskResultTTL] the TTL is one
// hour. Run returns [ErrTooManyRunningTasks] when the limit of running tasks is
// reached and [ErrMemoryTasksClosed] after Close; neither runs fn. The store
// must be the server's [TaskHandler], or the client's polls cannot find the
// task.
func (m *MemoryTasks) Run(ctx context.Context, fn func(ctx context.Context, status func(string)) (*mcp.CallToolResult, error), opts ...TaskResultOption) (*mcp.CallToolResult, error) {
	if taskCallFromContext(ctx) == nil {
		if ctx.Value(detachedTaskKey{}) != nil {
			return fn(ctx, func(string) {})
		}
		return runWithoutTask(ctx, fn, whyNoTaskCall(ctx))
	}
	var cfg taskResultConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	ttl := defaultMemoryTasksTTL
	if cfg.ttl != nil {
		ttl = *cfg.ttl
	}
	now := time.Now()
	createdAt := cfg.createdAt
	if createdAt.IsZero() {
		createdAt = now
	}
	updatedAt := now
	if createdAt.After(now) {
		updatedAt = createdAt
	}
	// One source for the TTL and the creation time: what is reported at the
	// hand-over is what every state reports.
	reported := cfg
	reported.ttl, reported.createdAt = &ttl, createdAt

	var raw [16]byte
	_, _ = rand.Read(raw[:])
	job := &memoryJob{
		id:        hex.EncodeToString(raw[:]),
		origin:    TaskOriginFromContext(ctx),
		createdAt: createdAt,
		ttl:       ttl,
		status:    mcp.TaskStatusWorking,
		message:   cfg.statusMessage,
		updatedAt: updatedAt,
	}
	if cfg.pollInterval != nil {
		poll := *cfg.pollInterval
		job.pollInterval = &poll
	}
	workCtx, cancel := context.WithCancel(DetachTaskContext(ctx))
	job.cancel = cancel

	m.mu.Lock()
	switch {
	case m.closed:
		m.mu.Unlock()
		cancel()
		return nil, ErrMemoryTasksClosed
	case m.running >= m.maxRunning:
		m.mu.Unlock()
		cancel()
		return nil, ErrTooManyRunningTasks
	}
	m.jobs[job.id] = job
	m.running++
	m.workers.Add(1)
	m.mu.Unlock()

	res, err := newTaskResult(ctx, job.id, reported)
	if err != nil {
		m.mu.Lock()
		delete(m.jobs, job.id)
		m.running--
		m.mu.Unlock()
		cancel()
		m.workers.Done()
		return runWithoutTask(ctx, fn, err)
	}

	m.mu.Lock()
	if m.jobs[job.id] == job {
		job.expiry = time.AfterFunc(time.Until(createdAt.Add(ttl)), func() { m.expire(job) })
	}
	m.mu.Unlock()
	go m.work(workCtx, job, fn)
	return res, nil
}

// runWithoutTask answers a call that could not become a task: fn runs at once
// when why says no task is possible, and why is the error otherwise.
func runWithoutTask(ctx context.Context, fn func(ctx context.Context, status func(string)) (*mcp.CallToolResult, error), why error) (*mcp.CallToolResult, error) {
	if errors.Is(why, ErrTasksNotDeclared) || errors.Is(why, ErrNoTaskHandler) {
		return fn(ctx, func(string) {})
	}
	return nil, why
}

// work runs fn for job and records how it ended. It returns, and gives its
// slot back, only once fn has returned, panicked or exited its goroutine.
func (m *MemoryTasks) work(ctx context.Context, job *memoryJob, fn func(ctx context.Context, status func(string)) (*mcp.CallToolResult, error)) {
	defer m.workers.Done()
	var (
		result    *mcp.CallToolResult
		err       error
		returned  bool // tells runtime.Goexit, which leaves no recover value, from a nil result
		setStatus = func(message string) { m.setMessage(job, message) }
	)
	defer func() {
		if r := recover(); r != nil {
			result, err = nil, fmt.Errorf("task work panicked: %v\n%s", r, debug.Stack())
		} else if !returned {
			result, err = nil, errors.New("task work ended its goroutine without returning")
		}
		m.finish(job, result, err)
	}()
	result, err = fn(ctx, setStatus)
	returned = true
}

func (m *MemoryTasks) setMessage(job *memoryJob, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if job.status != mcp.TaskStatusWorking {
		return
	}
	job.message = message
	job.touch(time.Now())
}

// finish gives job's slot back and, when the task is still working and kept,
// ends it with fn's outcome.
func (m *MemoryTasks) finish(job *memoryJob, result *mcp.CallToolResult, err error) {
	m.mu.Lock()
	m.running--
	if job.status == mcp.TaskStatusWorking && m.jobs[job.id] == job {
		switch {
		case err != nil:
			m.endLocked(job, mcp.TaskStatusFailed, nil, err)
		case result == nil:
			m.endLocked(job, mcp.TaskStatusFailed, nil, errNoTaskResult)
		default:
			m.endLocked(job, mcp.TaskStatusCompleted, result, nil)
		}
	}
	cancel := job.cancel
	job.cancel = func() {}
	m.mu.Unlock()
	cancel()
}

// endLocked moves job to a terminal status and evicts the oldest ended tasks
// beyond the limit. m.mu is held.
func (m *MemoryTasks) endLocked(job *memoryJob, status mcp.TaskStatus, result *mcp.CallToolResult, err error) {
	job.status = status
	job.message = ""
	job.result = result
	job.err = err
	job.touch(time.Now())
	job.endedAt = m.ended.PushBack(job)
	for m.ended.Len() > m.maxEnded {
		m.dropLocked(m.ended.Front().Value.(*memoryJob))
	}
}

// dropLocked forgets job. m.mu is held.
func (m *MemoryTasks) dropLocked(job *memoryJob) {
	delete(m.jobs, job.id)
	if job.endedAt != nil {
		m.ended.Remove(job.endedAt)
		job.endedAt = nil
	}
	if job.expiry != nil {
		job.expiry.Stop()
	}
}

// expire forgets job at its TTL and cancels its work if that still runs.
func (m *MemoryTasks) expire(job *memoryJob) {
	m.mu.Lock()
	if m.jobs[job.id] == job {
		m.dropLocked(job)
	}
	cancel := job.cancel
	m.mu.Unlock()
	cancel()
}

// TaskState renders the task as it is now, or returns [ErrTaskGone] for one
// this store does not hold: evicted, expired, closed, or from before a restart.
func (m *MemoryTasks) TaskState(_ context.Context, ref TaskRef) (TaskState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	job, ok := m.jobs[ref.HandlerID]
	if !ok {
		return TaskState{}, ErrTaskGone
	}
	ttl := job.ttl
	state := TaskState{
		Origin:        job.origin,
		CreatedAt:     job.createdAt,
		TTL:           &ttl,
		Status:        job.status,
		StatusMessage: job.message,
		LastUpdatedAt: job.updatedAt,
		Result:        job.result,
		Err:           job.err,
	}
	if job.pollInterval != nil {
		poll := *job.pollInterval
		state.PollInterval = &poll
	}
	return state, nil
}

// CancelTask ends a working task as cancelled at once and cancels its work,
// which keeps its slot until it returns. A task already ended is left as it
// is. It returns [ErrTaskGone] for a task this store does not hold.
func (m *MemoryTasks) CancelTask(_ context.Context, ref TaskRef) error {
	m.mu.Lock()
	job, ok := m.jobs[ref.HandlerID]
	if !ok {
		m.mu.Unlock()
		return ErrTaskGone
	}
	if job.status != mcp.TaskStatusWorking {
		m.mu.Unlock()
		return nil
	}
	m.endLocked(job, mcp.TaskStatusCancelled, nil, nil)
	cancel := job.cancel
	m.mu.Unlock()
	cancel()
	return nil
}

// Close cancels every task's work and waits for it to return, so work must
// watch its context. It forgets every task: later polls answer "gone", and
// [MemoryTasks.Run] returns [ErrMemoryTasksClosed] for a call that could have
// become a task. Close is safe to call again.
func (m *MemoryTasks) Close() {
	m.mu.Lock()
	m.closed = true
	cancels := make([]context.CancelFunc, 0, len(m.jobs))
	for _, job := range m.jobs {
		if job.expiry != nil {
			job.expiry.Stop()
		}
		cancels = append(cancels, job.cancel)
	}
	clear(m.jobs)
	m.ended.Init()
	m.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	m.workers.Wait()
}
