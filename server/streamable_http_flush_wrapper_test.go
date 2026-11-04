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

	"github.com/pbednarcik/flexmcp/mcp"
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

// unwrapOnlyWriter stands for a middleware that offers Unwrap, as
// http.ResponseController asks, but no Flush of its own.
type unwrapOnlyWriter struct {
	http.ResponseWriter
}

func (w *unwrapOnlyWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// flushErrorWriter stands for a middleware that flushes through FlushError
// alone, with no Flush.
type flushErrorWriter struct {
	inner http.ResponseWriter
}

func (w *flushErrorWriter) Header() http.Header         { return w.inner.Header() }
func (w *flushErrorWriter) Write(p []byte) (int, error) { return w.inner.Write(p) }
func (w *flushErrorWriter) WriteHeader(status int)      { w.inner.WriteHeader(status) }
func (w *flushErrorWriter) FlushError() error {
	w.inner.(http.Flusher).Flush()
	return nil
}

// plainWriter is a ResponseWriter that cannot flush at all.
type plainWriter struct {
	header http.Header
}

func (w *plainWriter) Header() http.Header         { return w.header }
func (w *plainWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *plainWriter) WriteHeader(int)             {}

// A writer streams when a layer under it can flush, found the way FlushError
// and http.ResponseController find it: Unwrap until a layer has FlushError or
// Flush. Flush reaches that layer too.
func TestHTTPResponseWriterAdapter_StreamsThroughUnwrap(t *testing.T) {
	tests := []struct {
		name      string
		wrap      func(http.ResponseWriter) http.ResponseWriter
		base      func() http.ResponseWriter
		canStream bool
	}{
		{"net/http's writer", func(w http.ResponseWriter) http.ResponseWriter { return w }, nil, true},
		{"Unwrap to a writer that flushes", func(w http.ResponseWriter) http.ResponseWriter { return &unwrapOnlyWriter{w} }, nil, true},
		{"two Unwraps deep", func(w http.ResponseWriter) http.ResponseWriter { return &unwrapOnlyWriter{&unwrapOnlyWriter{w}} }, nil, true},
		{"a layer with FlushError only", func(w http.ResponseWriter) http.ResponseWriter { return &unwrapOnlyWriter{&flushErrorWriter{w}} }, nil, true},
		{"a wrapper with no Unwrap", func(w http.ResponseWriter) http.ResponseWriter { return &hiddenFlusherWriter{w} }, nil, false},
		{
			"Unwrap to a writer that cannot flush",
			func(w http.ResponseWriter) http.ResponseWriter { return &unwrapOnlyWriter{w} },
			func() http.ResponseWriter { return &plainWriter{header: http.Header{}} },
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			var base http.ResponseWriter = rec
			if tt.base != nil {
				base = tt.base()
			}
			a := newHTTPResponseWriterAdapter(tt.wrap(base))
			require.Equal(t, tt.canStream, a.CanStream())
			a.Flush()
			if tt.base == nil {
				require.Equal(t, tt.canStream, rec.Flushed, "Flush reached net/http's writer")
			}
		})
	}
}

// A listen through a middleware that offers Unwrap but no Flush is a live
// stream, not a refusal.
func TestStreamableHTTP_ListenStreamsThroughAWrapperWithoutFlush(t *testing.T) {
	mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(true))
	h := NewStreamableHTTPServer(mcpServer)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(&unwrapOnlyWriter{w}, r)
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ack := nextListenMessage(t, openListenStream(ctx, t, ts.URL, 3, map[string]any{"toolsListChanged": true}))
	require.Equal(t, mcp.MethodNotificationSubscriptionsAcknowledged, ack.Method)
}
