package server

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// bufferedStuckWriter takes every write into a buffer and stalls in the
// flush, as net/http does for an event smaller than its buffer: the write
// deadline passes in FlushError, which reports it, where Flush cannot.
type bufferedStuckWriter struct {
	stuckWriter
	flushes int // how many flushes were attempted, under mu
}

func (w *bufferedStuckWriter) Write(p []byte) (int, error) { return len(p), nil }

func (w *bufferedStuckWriter) FlushError() error {
	w.mu.Lock()
	deadline := w.deadline
	w.flushes++
	w.mu.Unlock()
	if deadline == nil {
		select {} // no deadline: the flush never returns
	}
	<-deadline
	return os.ErrDeadlineExceeded
}

func serveNotifyingTool(t *testing.T, w http.ResponseWriter, options ...StreamableHTTPOption) {
	t.Helper()
	mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	mcpServer.AddTool(mcp.NewTool("notify"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		_ = mcpServer.SendNotificationToClient(ctx, "notifications/progress", map[string]any{"progress": 1})
		return mcp.NewToolResultText("done"), nil
	})
	httpServer := NewStreamableHTTPServer(mcpServer, append([]StreamableHTTPOption{WithWriteTimeout(100 * time.Millisecond)}, options...)...)

	r := httptest.NewRequest(http.MethodPost, "/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"notify"}}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")

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
}

// A flush that fails ends the exchange as a failed write does: an SSE event
// smaller than the writer's buffer reaches the socket only in the flush, so
// a client that stopped reading is seen there, and nothing more is written
// for the request: the response after the failed event is not flushed.
func TestStreamableHTTP_AFailedFlushEndsTheExchange(t *testing.T) {
	w := &bufferedStuckWriter{header: http.Header{}}
	serveNotifyingTool(t, w)
	w.mu.Lock()
	defer w.mu.Unlock()
	require.Equal(t, 1, w.flushes, "the flush's failure was not seen: output went on after it")
}

// A client that went away is a warning, not an error: nothing is wrong on
// the server.
func TestStreamableHTTP_AClientThatWentAwayIsLoggedAsAWarning(t *testing.T) {
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	serveNotifyingTool(t, newStuckWriter(), WithStreamableHTTPLogger(logger))
	require.Contains(t, logged.String(), "level=WARN")
	require.NotContains(t, logged.String(), "level=ERROR")
}
