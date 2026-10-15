package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// bufferingWrapper stands for a compressing or buffering middleware: its
// Write buffers, its Flush pushes its own buffer on and then flushes the
// writer it wraps, and it offers Unwrap as http.ResponseController asks.
type bufferingWrapper struct {
	http.ResponseWriter
	buf *bufio.Writer
}

func (w *bufferingWrapper) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *bufferingWrapper) Flush() {
	_ = w.buf.Flush()
	w.ResponseWriter.(http.Flusher).Flush()
}
func (w *bufferingWrapper) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// A flush goes through a middleware's own Flush, not past it to net/http's:
// an SSE event reaches the client when it is sent, not with the response,
// through a buffering wrapper that offers Unwrap (as http.ResponseController
// flushes: the first layer with FlushError or Flush).
func TestStreamableHTTP_FlushGoesThroughAWrappersOwnFlush(t *testing.T) {
	seen := make(chan struct{})
	mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	mcpServer.AddTool(mcp.NewTool("notify"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		_ = mcpServer.SendNotificationToClient(ctx, "notifications/progress", map[string]any{"progress": 1})
		select {
		case <-seen:
			return mcp.NewToolResultText("done"), nil
		case <-time.After(5 * time.Second):
			return mcp.NewToolResultText("the notification never reached the client"), nil
		}
	})
	h := NewStreamableHTTPServer(mcpServer)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bw := &bufferingWrapper{ResponseWriter: w, buf: bufio.NewWriterSize(w, 64<<10)}
		defer func() { _ = bw.buf.Flush() }()
		h.ServeHTTP(bw, r)
	}))
	defer ts.Close()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL+"/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"notify"}}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	var all strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		all.WriteString(line + "\n")
		if strings.Contains(line, "notifications/progress") {
			select {
			case <-seen:
			default:
				close(seen)
			}
		}
	}
	require.NotContains(t, all.String(), "never reached", "the progress event stayed in the wrapper's buffer until the end")
}
