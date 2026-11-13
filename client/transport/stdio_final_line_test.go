package transport

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/require"
)

// lastLineAnswerer is the server's stdin: the first request written to it is
// answered on the server's stdout with answer, without a newline, and the
// stdout then closes.
type lastLineAnswerer struct {
	stdout *io.PipeWriter
	answer string
	once   sync.Once
}

func (w *lastLineAnswerer) Write(p []byte) (int, error) {
	w.once.Do(func() {
		go func() {
			_, _ = io.WriteString(w.stdout, w.answer)
			_ = w.stdout.Close()
		}()
	})
	return len(p), nil
}

func (*lastLineAnswerer) Close() error { return nil }

// lastThenBlockReader returns line with io.EOF on its first read, and on
// every later read blocks until release is closed.
type lastThenBlockReader struct {
	line    string
	served  bool
	release chan struct{}
}

func (r *lastThenBlockReader) Read(p []byte) (int, error) {
	if !r.served {
		r.served = true
		return copy(p, r.line), io.EOF
	}
	<-r.release
	return 0, io.EOF
}

// brokenAfterWriteReader waits for written, then returns line together with
// err.
type brokenAfterWriteReader struct {
	line    string
	err     error
	written chan struct{}
}

func (r *brokenAfterWriteReader) Read(p []byte) (int, error) {
	<-r.written
	return copy(p, r.line), r.err
}

// signalWriter closes written on its first write.
type signalWriter struct {
	written chan struct{}
	once    sync.Once
}

func (w *signalWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.written) })
	return len(p), nil
}

func (*signalWriter) Close() error { return nil }

// The server's last line is served when it has no newline before the output
// closes, as the server serves the client's, and the end of output then ends
// the transport. A line that comes with any other read error is not served.
func TestStdio_ServesTheServersLastLineWithoutANewline(t *testing.T) {
	ping := func() JSONRPCRequest {
		return JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(1)), Method: "ping"}
	}

	t.Run("a response answers the pending request", func(t *testing.T) {
		serverOut, toClient := io.Pipe()
		t.Cleanup(func() { _ = toClient.Close() })
		stdio := NewIO(serverOut, &lastLineAnswerer{stdout: toClient, answer: `{"jsonrpc":"2.0","id":1,"result":{}}`}, nil)
		require.NoError(t, stdio.Start(t.Context()))
		t.Cleanup(func() { _ = stdio.Close() })

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		resp, err := stdio.SendRequest(ctx, ping())
		require.NoError(t, err, "the last line was dropped with the end of output")
		require.Equal(t, mcp.NewRequestId(int64(1)), resp.ID)
	})

	t.Run("a notification reaches the handler", func(t *testing.T) {
		serverOut, toClient := io.Pipe()
		t.Cleanup(func() { _ = toClient.Close() })
		stdio := NewIO(serverOut, nopWriteCloser{io.Discard}, nil)
		got := make(chan string, 1)
		stdio.SetNotificationHandler(func(n mcp.JSONRPCNotification) { got <- n.Method })
		require.NoError(t, stdio.Start(t.Context()))
		t.Cleanup(func() { _ = stdio.Close() })

		go func() {
			_, _ = io.WriteString(toClient, `{"jsonrpc":"2.0","method":"notifications/last"}`)
			_ = toClient.Close()
		}()
		select {
		case method := <-got:
			require.Equal(t, "notifications/last", method)
		case <-time.After(time.Second):
			t.Fatal("the last line was dropped with the end of output")
		}
	})

	t.Run("the end of output after the last line ends the transport", func(t *testing.T) {
		release := make(chan struct{})
		stdio := NewIO(&lastThenBlockReader{line: `{"jsonrpc":"2.0","method":"notifications/last"}`, release: release}, nopWriteCloser{io.Discard}, nil)
		got := make(chan string, 1)
		stdio.SetNotificationHandler(func(n mcp.JSONRPCNotification) { got <- n.Method })
		require.NoError(t, stdio.Start(t.Context()))
		t.Cleanup(func() { _ = stdio.Close() })
		// Registered last, so it runs first: a read loop that reads again
		// is let go before Close.
		t.Cleanup(func() { close(release) })

		select {
		case <-got:
		case <-time.After(time.Second):
			t.Fatal("the last line was dropped with the end of output")
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		_, err := stdio.SendRequest(ctx, ping())
		require.ErrorIs(t, err, ErrTransportClosed, "the transport outlived the end of output")
	})

	t.Run("a line that comes with a read error is not served", func(t *testing.T) {
		written := make(chan struct{})
		stdio := NewIO(
			&brokenAfterWriteReader{line: `{"jsonrpc":"2.0","id":1,"result":{}}`, err: errors.New("broken pipe"), written: written},
			&signalWriter{written: written}, nil)
		require.NoError(t, stdio.Start(t.Context()))
		t.Cleanup(func() { _ = stdio.Close() })

		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		resp, err := stdio.SendRequest(ctx, ping())
		require.ErrorIs(t, err, ErrTransportClosed, "a line read with a broken pipe was served: %v", resp)
	})
}
