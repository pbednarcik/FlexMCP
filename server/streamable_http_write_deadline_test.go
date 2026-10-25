package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// stuckWriter is a response writer whose client never reads: a write blocks
// until the write deadline passes, then fails, as net/http's does.
type stuckWriter struct {
	header http.Header

	mu       sync.Mutex
	deadline chan struct{} // closed when the deadline passes; nil for none
	timer    *time.Timer
	writes   int // how many writes were attempted
}

func newStuckWriter() *stuckWriter { return &stuckWriter{header: http.Header{}} }

func (w *stuckWriter) Header() http.Header { return w.header }
func (w *stuckWriter) WriteHeader(int)     {}
func (w *stuckWriter) Flush()              {}
func (w *stuckWriter) Write([]byte) (int, error) {
	w.mu.Lock()
	deadline := w.deadline
	w.writes++
	w.mu.Unlock()
	if deadline == nil {
		select {} // no deadline: the write never returns
	}
	<-deadline
	return 0, os.ErrDeadlineExceeded
}

func (w *stuckWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer != nil {
		w.timer.Stop()
	}
	if t.IsZero() {
		w.deadline = nil
		return nil
	}
	passed := make(chan struct{})
	w.deadline = passed
	w.timer = time.AfterFunc(time.Until(t), func() { close(passed) })
	return nil
}

// A client that stops reading does not hold the request for good: each
// write to the response has a deadline, after which the exchange is given
// up and the request ends; nothing more is written for it, so the response
// after the failed notification is not attempted.
func TestStreamableHTTP_AClientThatStopsReadingDoesNotHoldTheRequest(t *testing.T) {
	mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	mcpServer.AddTool(mcp.NewTool("notify"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		_ = mcpServer.SendNotificationToClient(ctx, "notifications/progress", map[string]any{"progress": 1})
		return mcp.NewToolResultText("done"), nil
	})
	httpServer := NewStreamableHTTPServer(mcpServer, WithWriteTimeout(100*time.Millisecond))

	r := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"notify"}}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")

	w := newStuckWriter()
	served := make(chan struct{})
	go func() {
		defer close(served)
		httpServer.ServeHTTP(w, r)
	}()
	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("a client that stopped reading held the request")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	require.Equal(t, 1, w.writes, "a write was attempted after one failed")
}
