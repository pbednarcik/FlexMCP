package transport

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// Close ends the handlers still serving requests the server sent: their
// context is cancelled, as the SSE and Streamable HTTP transports do, rather
// than left running for a transport that will never send their answer.
func TestStdio_CloseStopsTheHandlersOfServerRequests(t *testing.T) {
	serverOut, toClient := io.Pipe()
	stdio := NewIO(serverOut, nopWriteCloser{Writer: io.Discard}, nil)
	entered, stopped := make(chan struct{}), make(chan struct{})
	stdio.SetRequestHandler(func(ctx context.Context, _ JSONRPCRequest) (*JSONRPCResponse, error) {
		close(entered)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	})
	require.NoError(t, stdio.Start(t.Context()))
	t.Cleanup(func() { _ = toClient.Close() })

	go func() {
		_, _ = fmt.Fprintln(toClient, `{"jsonrpc":"2.0","id":1,"method":"roots/list"}`)
	}()
	<-entered
	require.NoError(t, stdio.Close())

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler was still running after Close")
	}
}

// A send that finds stdin busy, behind a write the child stops reading, waits
// no longer than its context allows: the client's cancellation notification
// and later requests are not held past their deadlines. The write already in
// the pipe cannot be interrupted, and Close releases it as transport closed.
func TestStdio_SendsQueuedBehindABlockedWriteHonorTheirContext(t *testing.T) {
	serverOut, _ := io.Pipe()
	stdinRead, stdinWrite := io.Pipe() // nobody reads: every write blocks
	t.Cleanup(func() { _ = stdinRead.Close() })
	stdio := NewIO(serverOut, stdinWrite, nil)
	require.NoError(t, stdio.Start(t.Context()))

	stuck := make(chan error, 1)
	go func() {
		_, err := stdio.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(1)), Method: "ping"})
		stuck <- err
	}()
	require.Eventually(t, func() bool {
		if stdio.stdinMu.TryLock() {
			stdio.stdinMu.Unlock()
			return false
		}
		return true
	}, 2*time.Second, time.Millisecond, "the first send never reached the pipe")

	for name, send := range map[string]func(context.Context) error{
		"request": func(ctx context.Context) error {
			_, err := stdio.SendRequest(ctx, JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(2)), Method: "ping"})
			return err
		},
		"cancellation": func(ctx context.Context) error {
			return stdio.SendNotification(ctx, mcp.JSONRPCNotification{JSONRPC: "2.0", Method: "notifications/cancelled"})
		},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		queued := make(chan error, 1)
		go func() { queued <- send(ctx) }()
		select {
		case err := <-queued:
			require.ErrorIs(t, err, context.DeadlineExceeded, name)
		case <-time.After(2 * time.Second):
			t.Fatalf("a %s queued behind the blocked write waited past its deadline", name)
		}
		cancel()
	}

	require.NoError(t, stdio.Close())
	select {
	case err := <-stuck:
		require.ErrorIs(t, err, ErrTransportClosed)
	case <-time.After(2 * time.Second):
		t.Fatal("the blocked write outlived Close")
	}
}

// answeringWriter is the child's stdin: each line written to it is answered
// on the child's stdout with answer.
type answeringWriter struct {
	stdout *io.PipeWriter
	answer string
}

func (w answeringWriter) Write(p []byte) (int, error) {
	go func() { _, _ = fmt.Fprintln(w.stdout, w.answer) }()
	return len(p), nil
}

func (answeringWriter) Close() error { return nil }

// An answer to a pending request that names its id but cannot be decoded
// fails that request at once, as on Streamable HTTP, rather than leave it
// waiting for its deadline with nothing said.
func TestStdio_AnUndecodableAnswerFailsItsRequest(t *testing.T) {
	serverOut, toClient := io.Pipe()
	t.Cleanup(func() { _ = toClient.Close() })
	stdio := NewIO(serverOut, answeringWriter{stdout: toClient, answer: `{"jsonrpc":"2.0","id":3,"error":{"code":"oops"}}`}, nil)
	require.NoError(t, stdio.Start(t.Context()))
	t.Cleanup(func() { _ = stdio.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err := stdio.SendRequest(ctx, JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(3)), Method: "ping"})
	require.Error(t, err)
	require.NotErrorIs(t, err, context.DeadlineExceeded, "the request waited for its deadline")
	require.ErrorContains(t, err, "failed to decode response")
}

// The read loop reuses one buffer from line to line, so nothing decoded from
// a line may keep its bytes: an answer still reads as it arrived after the
// next line, of the same length, has overwritten the buffer.
func TestStdio_AnAnswerOutlivesTheNextLine(t *testing.T) {
	serverOut, toClient := io.Pipe()
	t.Cleanup(func() { _ = toClient.Close() })
	stdio := NewIO(serverOut, answeringWriter{stdout: toClient, answer: `{"jsonrpc":"2.0","id":1,"result":{"text":"aaaaaaaa"}}`}, nil)
	next := make(chan struct{})
	stdio.SetNotificationHandler(func(mcp.JSONRPCNotification) { close(next) })
	require.NoError(t, stdio.Start(t.Context()))
	t.Cleanup(func() { _ = stdio.Close() })

	response, err := stdio.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(1)), Method: "ping"})
	require.NoError(t, err)
	_, err = fmt.Fprintln(toClient, `{"jsonrpc":"2.0","method":"zzzzzzzzzzzzzzz","params":{}}`)
	require.NoError(t, err)
	<-next
	require.JSONEq(t, `{"text":"aaaaaaaa"}`, string(response.Result))
}

// A notification handler runs outside the transport's lock, so one that
// sets the handler, as a client replacing its own does, does not deadlock
// the read loop.
func TestStdio_ANotificationHandlerMaySetTheHandler(t *testing.T) {
	serverOut, toClient := io.Pipe()
	t.Cleanup(func() { _ = toClient.Close() })
	stdio := NewIO(serverOut, nopWriteCloser{Writer: io.Discard}, nil)
	second := make(chan struct{})
	stdio.SetNotificationHandler(func(mcp.JSONRPCNotification) {
		stdio.SetNotificationHandler(func(mcp.JSONRPCNotification) { close(second) })
	})
	require.NoError(t, stdio.Start(t.Context()))
	t.Cleanup(func() { _ = stdio.Close() })

	go func() {
		_, _ = fmt.Fprintln(toClient, `{"jsonrpc":"2.0","method":"notifications/first"}`)
		_, _ = fmt.Fprintln(toClient, `{"jsonrpc":"2.0","method":"notifications/second"}`)
	}()
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("the read loop deadlocked in a handler that set the handler")
	}
}

// A command that fails to start leaves the transport as it was before Start:
// a send is refused as not started, and Close has nothing to close. Before,
// the pipes the failed start had already closed were kept, so a send failed
// writing to one and Close failed closing it.
func TestStdio_AFailedStartLeavesNothingBehind(t *testing.T) {
	stdio := NewStdio("flexmcp-no-such-command", nil)
	require.Error(t, stdio.Start(t.Context()))

	_, err := stdio.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(1)), Method: "ping"})
	require.ErrorIs(t, err, ErrTransportNotStarted)
	require.ErrorIs(t, stdio.SendNotification(t.Context(), mcp.JSONRPCNotification{JSONRPC: "2.0", Method: "notifications/initialized"}), ErrTransportNotStarted)
	require.NoError(t, stdio.Close())
}

// writeSignal is the child's stdin, signalling each line written to it.
type writeSignal chan struct{}

func (w writeSignal) Write(p []byte) (int, error) {
	w <- struct{}{}
	return len(p), nil
}

func (writeSignal) Close() error { return nil }

// Each line read is its own message: nothing of one carries into the next.
// A response after a notification is still a response, not a request with
// the notification's method, and a result after an error response has no
// error.
func TestStdio_EachLineIsReadOnItsOwn(t *testing.T) {
	serverOut, toClient := io.Pipe()
	t.Cleanup(func() { _ = toClient.Close() })
	written := make(writeSignal, 2)
	stdio := NewIO(serverOut, written, nil)
	notified := make(chan struct{}, 1)
	stdio.SetNotificationHandler(func(mcp.JSONRPCNotification) { notified <- struct{}{} })
	require.NoError(t, stdio.Start(t.Context()))
	t.Cleanup(func() { _ = stdio.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	type answer struct {
		response *JSONRPCResponse
		err      error
	}
	answers := make([]chan answer, 2)
	for i := range answers {
		answers[i] = make(chan answer, 1)
		go func() {
			response, err := stdio.SendRequest(ctx, JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(i + 1)), Method: "ping"})
			answers[i] <- answer{response, err}
		}()
		<-written
	}
	_, err := fmt.Fprint(toClient,
		`{"jsonrpc":"2.0","method":"notifications/progress","params":{}}`+"\n"+
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"nope"}}`+"\n"+
			`{"jsonrpc":"2.0","id":2,"result":{}}`+"\n")
	require.NoError(t, err)

	<-notified
	first := <-answers[0]
	require.NoError(t, first.err)
	require.NotNil(t, first.response.Error)
	assert.Equal(t, "nope", first.response.Error.Message)
	second := <-answers[1]
	require.NoError(t, second.err)
	assert.Nil(t, second.response.Error, "the result carried the error before it")
	assert.JSONEq(t, `{}`, string(second.response.Result))
}
