package transport

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

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
