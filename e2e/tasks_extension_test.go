package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pbednarcik/flexmcp/client"
	"github.com/pbednarcik/flexmcp/client/transport"
	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// awaitLimit bounds every wait on a channel, so a test that fails to make
// progress fails instead of hanging; a passing wait never reaches it.
const awaitLimit = 10 * time.Second

// await receives from ch, failing the test when nothing arrives in time.
func await[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(awaitLimit):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// declaresTasks makes a client declare the Tasks extension on every request.
func declaresTasks() client.ClientOption {
	return client.WithClientCapabilities(mcp.ClientCapabilities{
		Extensions: map[string]any{mcp.ExtensionTasks: map[string]any{}},
	})
}

// connector connects an initialized client to srv over one transport and
// registers the teardown of the connection.
type connector func(t *testing.T, srv *server.MCPServer, opts ...client.ClientOption) *client.Client

func initialized(t *testing.T, c *client.Client) *client.Client {
	t.Helper()
	var request mcp.InitializeRequest
	request.Params.ClientInfo = mcp.Implementation{Name: "e2e-client", Version: "1.0.0"}
	result, err := c.Initialize(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, mcp.ProtocolVersion20260728, result.ProtocolVersion)
	return c
}

func connectInProcess(t *testing.T, srv *server.MCPServer, opts ...client.ClientOption) *client.Client {
	t.Helper()
	c := client.NewClient(transport.NewInProcessTransport(srv), opts...)
	require.NoError(t, c.Start(t.Context()))
	t.Cleanup(func() { _ = c.Close() })
	return initialized(t, c)
}

// connectStdio connects over in-memory pipes to a real StdioServer.
func connectStdio(t *testing.T, srv *server.MCPServer, opts ...client.ClientOption) *client.Client {
	t.Helper()
	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()
	listened := make(chan struct{})
	go func() {
		defer close(listened)
		_ = server.NewStdioServer(srv).Listen(t.Context(), serverRead, serverWrite)
	}()
	t.Cleanup(func() {
		_ = serverWrite.Close()
		_ = serverRead.Close()
		select {
		case <-listened:
		case <-time.After(awaitLimit):
			t.Error("the stdio server did not stop")
		}
	})

	c := client.NewClient(transport.NewIO(clientRead, clientWrite, io.NopCloser(strings.NewReader(""))), opts...)
	require.NoError(t, c.Start(t.Context()))
	t.Cleanup(func() { _ = c.Close() })
	return initialized(t, c)
}

// serveHTTP serves srv over Streamable HTTP and returns its URL. around, when
// given, wraps the handler.
func serveHTTP(t *testing.T, srv *server.MCPServer, around ...func(http.Handler) http.Handler) string {
	t.Helper()
	streamable := server.NewStreamableHTTPServer(srv)
	var handler http.Handler = streamable
	for _, wrap := range around {
		handler = wrap(handler)
	}
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	t.Cleanup(func() { _ = streamable.Shutdown(t.Context()) })
	return httpServer.URL
}

func connectHTTP(t *testing.T, srv *server.MCPServer, opts ...client.ClientOption) *client.Client {
	t.Helper()
	c, result := startHTTPClient(t, serveHTTP(t, srv), opts...)
	require.Equal(t, mcp.ProtocolVersion20260728, result.ProtocolVersion)
	return c
}

// transports are the three ways a client reaches a server.
var transports = []struct {
	name    string
	connect connector
}{
	{"in-process", connectInProcess},
	{"stdio", connectStdio},
	{"streamable HTTP", connectHTTP},
}

// taskFixture is a server whose tools run through a MemoryTasks, and the
// channels the tests steer their work with.
type taskFixture struct {
	mem *server.MemoryTasks
	// started receives one value each time work starts.
	started chan struct{}
	// release lets the work of "slow" finish; open closes it.
	release chan struct{}
	// stopped receives the context error of work that was stopped.
	stopped chan error
	// origins receives the origin each "slow" call saw.
	origins     chan server.TaskOrigin
	releaseOnce sync.Once
}

func newTaskFixture(t *testing.T) *taskFixture {
	t.Helper()
	f := &taskFixture{
		mem:     server.NewMemoryTasks(),
		started: make(chan struct{}, 8),
		release: make(chan struct{}),
		stopped: make(chan error, 8),
		origins: make(chan server.TaskOrigin, 8),
	}
	// Cleanups run last in, first out: work is released before the store waits
	// for it.
	t.Cleanup(f.mem.Close)
	t.Cleanup(f.open)
	return f
}

// open lets all work finish.
func (f *taskFixture) open() {
	f.releaseOnce.Do(func() { close(f.release) })
}

// server builds the fixture's server, named alike on every call so that servers
// sharing a key open each other's task ids.
func (f *taskFixture) server(opts ...server.ServerOption) *server.MCPServer {
	base := []server.ServerOption{server.WithToolCapabilities(true), server.WithTaskHandler(f.mem)}
	srv := server.NewMCPServer("e2e-tasks", "1.0.0", append(base, opts...)...)
	srv.AddTool(mcp.NewTool("slow", mcp.WithString("name")), f.slow)
	srv.AddTool(mcp.NewTool("boom"), f.boom)
	srv.AddTool(mcp.NewTool("forge"), forge)
	return srv
}

// slow hands its work over as a task when the call can become one, and does it
// at once otherwise. The work waits to be released or stopped.
func (f *taskFixture) slow(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	name := request.GetString("name", "")
	select {
	case f.origins <- server.TaskOriginFromContext(ctx):
	default:
	}
	return f.mem.Run(ctx, func(ctx context.Context, status func(string)) (*mcp.CallToolResult, error) {
		select {
		case f.started <- struct{}{}:
		default:
		}
		status("waiting for the release")
		select {
		case <-f.release:
			return mcp.NewToolResultText("finished " + name), nil
		case <-ctx.Done():
			select {
			case f.stopped <- ctx.Err():
			default:
			}
			return nil, ctx.Err()
		}
	})
}

// boom is a task whose work fails.
func (f *taskFixture) boom(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return f.mem.Run(ctx, func(context.Context, func(string)) (*mcp.CallToolResult, error) {
		return nil, errors.New("disk on fire")
	})
}

// forge answers with a task whose id the server never minted.
func forge(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	now := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	return &mcp.CallToolResult{Task: &mcp.ExtensionTask{
		TaskId:        "forged-by-the-handler",
		Status:        mcp.TaskStatusWorking,
		CreatedAt:     now,
		LastUpdatedAt: now,
	}}, nil
}

func callSlow(t *testing.T, ctx context.Context, c *client.Client, name string) *mcp.CallToolResult {
	t.Helper()
	var call mcp.CallToolRequest
	call.Params.Name = "slow"
	call.Params.Arguments = map[string]any{"name": name}
	result, err := c.CallTool(ctx, call)
	require.NoError(t, err)
	return result
}

func textOf(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	require.Len(t, result.Content, 1)
	text, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected text content, got %T", result.Content[0])
	return text.Text
}

func TestTasksEndToEnd(t *testing.T) {
	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			f := newTaskFixture(t)
			c := tr.connect(t, f.server(), declaresTasks())
			ctx, cancel := context.WithTimeout(t.Context(), awaitLimit)
			defer cancel()

			handedOver := callSlow(t, ctx, c, "report")
			require.NotNil(t, handedOver.Task, "the call must come back as a task")
			assert.Equal(t, mcp.ResultTypeTask, handedOver.ResultType)
			assert.Equal(t, mcp.TaskStatusWorking, handedOver.Task.Status)
			assert.Empty(t, handedOver.Content)
			taskID := handedOver.Task.TaskId
			require.NotEmpty(t, taskID)
			assert.LessOrEqual(t, len(taskID), 128)

			await(t, f.started, "the work to start")
			working, err := c.GetExtensionTask(ctx, taskID)
			require.NoError(t, err)
			assert.Equal(t, taskID, working.TaskId)
			assert.Equal(t, mcp.TaskStatusWorking, working.Status)
			assert.Equal(t, "waiting for the release", working.StatusMessage)
			assert.Nil(t, working.TaskResult)

			type outcome struct {
				state *mcp.ExtensionTaskStateResult
				err   error
			}
			waited := make(chan outcome, 1)
			go func() {
				state, err := c.WaitForExtensionTask(ctx, taskID, 5*time.Millisecond)
				waited <- outcome{state, err}
			}()
			f.open()

			done := await(t, waited, "the wait to end")
			require.NoError(t, done.err)
			assert.Equal(t, mcp.TaskStatusCompleted, done.state.Status)
			require.NotNil(t, done.state.TaskResult)
			assert.True(t, done.state.TaskResult.ResultType.IsComplete())
			assert.Equal(t, "finished report", textOf(t, done.state.TaskResult))
			assert.Nil(t, done.state.Error)

			again, err := c.GetExtensionTask(ctx, taskID)
			require.NoError(t, err)
			assert.Equal(t, mcp.TaskStatusCompleted, again.Status, "a terminal status does not change")
		})
	}
}

func TestTasksEndToEndCancel(t *testing.T) {
	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			f := newTaskFixture(t)
			c := tr.connect(t, f.server(), declaresTasks())
			ctx, cancel := context.WithTimeout(t.Context(), awaitLimit)
			defer cancel()

			handedOver := callSlow(t, ctx, c, "doomed")
			require.NotNil(t, handedOver.Task)
			await(t, f.started, "the work to start")

			require.NoError(t, c.CancelExtensionTask(ctx, handedOver.Task.TaskId))

			require.Eventually(t, func() bool {
				state, err := c.GetExtensionTask(ctx, handedOver.Task.TaskId)
				return err == nil && state.Status == mcp.TaskStatusCancelled
			}, awaitLimit, 5*time.Millisecond)
			assert.ErrorIs(t, await(t, f.stopped, "the work to be stopped"), context.Canceled)
		})
	}
}

func TestTasksEndToEndFailure(t *testing.T) {
	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			f := newTaskFixture(t)
			c := tr.connect(t, f.server(), declaresTasks())
			ctx, cancel := context.WithTimeout(t.Context(), awaitLimit)
			defer cancel()

			var call mcp.CallToolRequest
			call.Params.Name = "boom"
			handedOver, err := c.CallTool(ctx, call)
			require.NoError(t, err)
			require.NotNil(t, handedOver.Task)

			state, err := c.WaitForExtensionTask(ctx, handedOver.Task.TaskId, 5*time.Millisecond)
			require.NoError(t, err)
			assert.Equal(t, mcp.TaskStatusFailed, state.Status)
			require.NotNil(t, state.Error)
			assert.NotContains(t, state.Error.Message, "disk on fire", "the work's error stays on the server")
			assert.Nil(t, state.TaskResult)
		})
	}
}

// A client that does not declare the extension is answered at once, whatever
// the handler would have handed over, and sees no origin on its call.
func TestAClientThatDoesNotDeclareTasksIsAnsweredSynchronously(t *testing.T) {
	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			f := newTaskFixture(t)
			f.open()
			c := tr.connect(t, f.server())

			result := callSlow(t, t.Context(), c, "plain")

			assert.Nil(t, result.Task)
			assert.True(t, result.ResultType.IsComplete())
			assert.Equal(t, "finished plain", textOf(t, result))
			origin := await(t, f.origins, "the handler's origin")
			assert.True(t, origin.IsZero(), "a call that cannot become a task carries no origin, got %q", origin.Tool())

			_, err := c.GetExtensionTask(t.Context(), "any")
			assert.ErrorIs(t, err, client.ErrExtensionTasksNotDeclared)
		})
	}
}

// A handler that returns a task id it did not mint through NewTaskResult is
// refused on the way out, on every transport, and no client sees the task.
func TestAForgedTaskAnswerNeverReachesTheClient(t *testing.T) {
	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			f := newTaskFixture(t)
			c := tr.connect(t, f.server(), declaresTasks())

			var call mcp.CallToolRequest
			call.Params.Name = "forge"
			result, err := c.CallTool(t.Context(), call)

			require.Error(t, err)
			assert.ErrorIs(t, err, mcp.ErrInternalError)
			assert.Nil(t, result)
		})
	}
}

// Two servers built with one key and name model a restart: the second opens
// the first's ids but does not hold their tasks.
func TestTaskIdsAcrossARestartOverStreamableHTTP(t *testing.T) {
	key := []byte("0123456789abcdefghijklmnopqrstuv")
	first := newTaskFixture(t)
	c := connectHTTP(t, first.server(server.WithTaskKey(key)), declaresTasks())
	ctx, cancel := context.WithTimeout(t.Context(), awaitLimit)
	defer cancel()

	handedOver := callSlow(t, ctx, c, "survivor")
	require.NotNil(t, handedOver.Task)
	taskID := handedOver.Task.TaskId
	await(t, first.started, "the work to start")

	requireRefused := func(t *testing.T, err error, reason string) {
		t.Helper()
		require.Error(t, err)
		refused, ok := errors.AsType[mcp.TaskRefusedError](err)
		require.True(t, ok, "expected a TaskRefusedError, got %T: %v", err, err)
		assert.Equal(t, reason, refused.Reason)
		assert.ErrorIs(t, err, mcp.ErrInvalidParams)
		assert.Equal(t, reason == "gone", mcp.IsTaskGone(err))
	}

	t.Run("the server that made the task still has it", func(t *testing.T) {
		state, err := c.GetExtensionTask(ctx, taskID)
		require.NoError(t, err)
		assert.Equal(t, mcp.TaskStatusWorking, state.Status)
	})

	t.Run("a restarted server answers gone", func(t *testing.T) {
		second := newTaskFixture(t)
		restarted := connectHTTP(t, second.server(server.WithTaskKey(key)), declaresTasks())

		_, err := restarted.GetExtensionTask(ctx, taskID)
		requireRefused(t, err, "gone")
		requireRefused(t, restarted.CancelExtensionTask(ctx, taskID), "gone")
		_, err = restarted.WaitForExtensionTask(ctx, taskID, 5*time.Millisecond)
		requireRefused(t, err, "gone")
	})

	t.Run("a rebuilt server with a key of its own answers gone", func(t *testing.T) {
		third := newTaskFixture(t)
		rebuilt := connectHTTP(t, third.server(), declaresTasks())

		_, err := rebuilt.GetExtensionTask(ctx, taskID)
		requireRefused(t, err, "gone")
	})

	t.Run("an id the server never minted answers unknown", func(t *testing.T) {
		// A middle character: the last one carries padding bits, so flipping it
		// can trip the canonical-encoding check instead of the MAC.
		tampered := []byte(taskID)
		mid := len(tampered) / 2
		if tampered[mid] == 'A' {
			tampered[mid] = 'B'
		} else {
			tampered[mid] = 'A'
		}
		for name, id := range map[string]string{
			"a tampered id": string(tampered),
			"a made-up id":  "bm90LWEtdGFzay1pZA",
		} {
			_, err := c.GetExtensionTask(ctx, id)
			requireRefused(t, err, "unknown")
			assert.False(t, mcp.IsTaskGone(err), name)
		}
	})
}

// postTasksGet sends a raw tasks/get for taskID with the given Mcp-Name.
func postTasksGet(t *testing.T, url, taskID, name string) *http.Response {
	t.Helper()
	params := map[string]any{
		"taskId": taskID,
		"_meta": map[string]any{
			mcp.MetaKeyProtocolVersion: mcp.ProtocolVersion20260728,
			mcp.MetaKeyClientInfo:      map[string]any{"name": "raw", "version": "1.0.0"},
			mcp.MetaKeyClientCapabilities: map[string]any{
				"extensions": map[string]any{mcp.ExtensionTasks: map[string]any{}},
			},
		},
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": mcp.MethodTasksGet, "params": params})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(mcp.HeaderProtocolVersion, mcp.ProtocolVersion20260728)
	req.Header.Set(mcp.HeaderMethod, string(mcp.MethodTasksGet))
	req.Header.Set(mcp.HeaderName, name)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// The client writes the task id into Mcp-Name and the server checks it against
// the body: one real request proves the two agree, and a request whose header
// differs proves the server looks.
func TestTheMcpNameHeaderAgreesOnTheRealHTTPPath(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	record := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen[r.Header.Get(mcp.HeaderMethod)] = r.Header.Get(mcp.HeaderName)
			mu.Unlock()
			next.ServeHTTP(w, r)
		})
	}
	f := newTaskFixture(t)
	url := serveHTTP(t, f.server(), record)
	c, _ := startHTTPClient(t, url, declaresTasks())

	handedOver := callSlow(t, t.Context(), c, "named")
	require.NotNil(t, handedOver.Task)
	taskID := handedOver.Task.TaskId

	state, err := c.GetExtensionTask(t.Context(), taskID)
	require.NoError(t, err, "the server accepted the client's Mcp-Name")
	assert.Equal(t, taskID, state.TaskId)

	mu.Lock()
	assert.Equal(t, "slow", seen[string(mcp.MethodToolsCall)])
	assert.Equal(t, taskID, seen[string(mcp.MethodTasksGet)])
	mu.Unlock()

	agreeing := postTasksGet(t, url, taskID, taskID)
	assert.Equal(t, http.StatusOK, agreeing.StatusCode)
	mismatched := postTasksGet(t, url, taskID, "some-other-task")
	assert.Equal(t, http.StatusBadRequest, mismatched.StatusCode)
}

// idleTasks is a TaskHandler for a server whose tasks are never polled.
type idleTasks struct{}

func (idleTasks) TaskState(context.Context, server.TaskRef) (server.TaskState, error) {
	return server.TaskState{Status: mcp.TaskStatusWorking, CreatedAt: time.Now()}, nil
}

func (idleTasks) CancelTask(context.Context, server.TaskRef) error { return nil }

// handoverProbe starts two pieces of work in a tool handler that hands over:
// one under the request's own context, one under a context detached from it.
type handoverProbe struct {
	wg      sync.WaitGroup
	stop    chan struct{}
	release chan struct{}
	// underRequest receives the error the first work saw on its context.
	underRequest chan error
	// detached receives what the second work saw once released.
	detached chan detachedView
}

type detachedView struct {
	ctxErr error
	tool   string
}

func newHandoverProbe(t *testing.T) *handoverProbe {
	t.Helper()
	p := &handoverProbe{
		stop:         make(chan struct{}),
		release:      make(chan struct{}),
		underRequest: make(chan error, 1),
		detached:     make(chan detachedView, 1),
	}
	t.Cleanup(func() {
		close(p.stop)
		p.wg.Wait()
	})
	return p
}

func (p *handoverProbe) handle(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	detachedCtx := context.WithoutCancel(ctx)
	p.wg.Add(2)
	go func() {
		defer p.wg.Done()
		select {
		case <-ctx.Done():
			p.underRequest <- ctx.Err()
		case <-p.stop:
		}
	}()
	go func() {
		defer p.wg.Done()
		select {
		case <-p.release:
			p.detached <- detachedView{ctxErr: detachedCtx.Err(), tool: server.TaskOriginFromContext(detachedCtx).Tool()}
		case <-p.stop:
		}
	}()
	return server.NewTaskResult(ctx, "job-1")
}

// The request's context ends once its response is out, whatever the transport.
// Work the handler leaves on it is stopped, which is why NewTaskResult warns;
// work under a WithoutCancel of it, with the origin it carries, goes on.
func TestHandedOverWorkOutlivesTheRequest(t *testing.T) {
	for _, tr := range transports {
		t.Run(tr.name, func(t *testing.T) {
			p := newHandoverProbe(t)
			srv := server.NewMCPServer("e2e-handover", "1.0.0", server.WithToolCapabilities(true), server.WithTaskHandler(idleTasks{}))
			srv.AddTool(mcp.NewTool("work"), p.handle)
			c := tr.connect(t, srv, declaresTasks())

			var call mcp.CallToolRequest
			call.Params.Name = "work"
			handedOver, err := c.CallTool(t.Context(), call)
			require.NoError(t, err)
			require.NotNil(t, handedOver.Task)

			assert.ErrorIs(t, await(t, p.underRequest, "the request's context to end"), context.Canceled,
				"work under the request's context is stopped once the response is out")
			select {
			case view := <-p.detached:
				t.Fatalf("the detached work ended before it was released: %+v", view)
			default:
			}

			close(p.release)
			view := await(t, p.detached, "the detached work to run on")
			assert.NoError(t, view.ctxErr, "work under WithoutCancel is not stopped")
			assert.Equal(t, "work", view.tool, "and keeps the origin of its call")

			state, err := c.GetExtensionTask(t.Context(), handedOver.Task.TaskId)
			require.NoError(t, err)
			assert.Equal(t, mcp.TaskStatusWorking, state.Status)
		})
	}
}
