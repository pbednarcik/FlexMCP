package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// scriptedSSEServer is a legacy HTTP+SSE server: GET opens the stream with
// the endpoint event, then writes what arrives on events until it is ended;
// a POST is accepted and handed to onPost with the message's id.
type scriptedSSEServer struct {
	events     chan string
	onPost     func(id json.RawMessage, s *scriptedSSEServer)
	noEndpoint bool
	endOnce    sync.Once
	posts      atomic.Int32
}

func newScriptedSSEServer() *scriptedSSEServer {
	return &scriptedSSEServer{events: make(chan string, 16)}
}

// end closes the stream from the server's side.
func (s *scriptedSSEServer) end() { s.endOnce.Do(func() { close(s.events) }) }

func (s *scriptedSSEServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		if !s.noEndpoint {
			_, _ = fmt.Fprint(w, "event: endpoint\ndata: /message\n\n")
		}
		flusher.Flush()
		for {
			select {
			case event, ok := <-s.events:
				if !ok {
					return
				}
				_, _ = fmt.Fprint(w, event)
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	}
	s.posts.Add(1)
	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &message)
	w.WriteHeader(http.StatusAccepted)
	if s.onPost != nil && message.Method != "" {
		s.onPost(message.ID, s)
	}
}

// startedSSE is an SSE transport started against server, closed at cleanup.
func startedSSE(t *testing.T, server *scriptedSSEServer, options ...ClientOption) *SSE {
	t.Helper()
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	c, err := NewSSE(httpServer.URL, options...)
	require.NoError(t, err)
	require.NoError(t, c.Start(t.Context()))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// A notification handler runs outside the transport's lock, so one that
// sets the notification handler does not stop the stream: the response
// after the notification still arrives.
func TestSSE_ANotificationHandlerMaySetTheHandler(t *testing.T) {
	server := newScriptedSSEServer()
	server.onPost = func(id json.RawMessage, s *scriptedSSEServer) {
		s.events <- "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n"
		s.events <- fmt.Sprintf("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{}}\n\n", id)
	}
	c := startedSSE(t, server, WithResponseTimeout(2*time.Second))
	c.SetNotificationHandler(func(mcp.JSONRPCNotification) { c.SetNotificationHandler(func(mcp.JSONRPCNotification) {}) })

	_, err := c.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(1)), Method: "tools/list"})
	assert.NoError(t, err)
}

// The same on Streamable HTTP, whose POST may answer with an SSE stream
// that carries a notification before the response.
func TestStreamableHTTP_ANotificationHandlerMaySetTheHandler(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var message struct {
			ID json.RawMessage `json:"id"`
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &message)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{}}\n\n", message.ID)
	}))
	t.Cleanup(httpServer.Close)
	c, err := NewStreamableHTTP(httpServer.URL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	c.SetNotificationHandler(func(mcp.JSONRPCNotification) { c.SetNotificationHandler(func(mcp.JSONRPCNotification) {}) })

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, err = c.SendRequest(ctx, JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(1)), Method: "tools/list"})
	assert.NoError(t, err)
}

// A stream the server ends, with no Close, fails the request waiting on it
// and every request after it at once: nothing is left to read their
// answers, and waiting out the response timeout serves no one.
func TestSSE_AnEndedStreamFailsPendingAndLaterRequests(t *testing.T) {
	server := newScriptedSSEServer()
	server.onPost = func(_ json.RawMessage, s *scriptedSSEServer) { s.end() }
	c := startedSSE(t, server, WithResponseTimeout(5*time.Second))

	for i := int64(1); i <= 2; i++ {
		begin := time.Now()
		_, err := c.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(i), Method: "tools/list"})
		require.Error(t, err, "request %d", i)
		assert.Less(t, time.Since(begin), time.Second, "request %d waited on an ended stream", i)
	}
	_, err := c.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(3)), Method: "tools/list"})
	assert.ErrorIs(t, err, ErrTransportClosed)
}

// A stream an earlier Start attempt opened, ending after a later attempt
// has started, ends nothing the later attempt serves.
func TestSSE_AnEarlierAttemptsStreamEndsNothing(t *testing.T) {
	c, err := NewSSE("http://127.0.0.1:1/sse")
	require.NoError(t, err)
	c.attempt.Store(2)
	c.readSSE(io.NopCloser(strings.NewReader("")), make(chan struct{}), &sync.Once{}, 1)
	assert.False(t, c.ended.Load(), "the current attempt's stream was marked ended")

	c.readSSE(io.NopCloser(strings.NewReader("")), make(chan struct{}), &sync.Once{}, 2)
	assert.True(t, c.ended.Load(), "the current attempt's own stream ended")
}

// A Close that lands while a request is being built, after its closed check,
// still ends it: the request is not sent and does not wait for an answer.
func TestSSE_ACloseDuringSendRequestEndsIt(t *testing.T) {
	server := newScriptedSSEServer()
	var armed atomic.Bool
	var c *SSE
	c = startedSSE(t, server, WithResponseTimeout(5*time.Second), WithHeaderFunc(func(context.Context) map[string]string {
		if armed.Load() {
			_ = c.Close()
		}
		return nil
	}))
	armed.Store(true)

	begin := time.Now()
	_, err := c.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(1)), Method: "tools/list"})
	require.EqualError(t, err, "transport has been closed")
	assert.ErrorIs(t, err, ErrTransportClosed)
	assert.Less(t, time.Since(begin), time.Second, "the request outlived the Close")
	assert.Zero(t, server.posts.Load(), "the request was sent after Close")
}

// A Start that fails has already told its caller so: the connection-lost
// handler, which callers answer by reconnecting, does not fire for it.
func TestSSE_AFailedStartDoesNotReportALostConnection(t *testing.T) {
	server := newScriptedSSEServer()
	server.noEndpoint = true
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	c, err := NewSSE(httpServer.URL, WithEndpointTimeout(200*time.Millisecond))
	require.NoError(t, err)
	var lost atomic.Int32
	c.SetConnectionLostHandler(func(error) { lost.Add(1) })

	require.Error(t, c.Start(t.Context()))
	time.Sleep(300 * time.Millisecond)
	assert.Zero(t, lost.Load(), "the connection-lost handler ran for a Start that failed")
}

// A stream that ends as Start completes is reported one way or the other:
// Start fails, or it succeeds and the connection-lost handler fires. It is
// never a successful Start on a stream that is gone, reported to no one.
func TestSSE_AStreamEndingAsStartCompletesIsReported(t *testing.T) {
	for range 20 {
		// The endpoint and the stream's end go out in one write, so the
		// reader can see the end before Start has published its success.
		httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: endpoint\ndata: /message\n\n")
		}))
		c, err := NewSSE(httpServer.URL)
		require.NoError(t, err)
		lost := make(chan struct{}, 1)
		c.SetConnectionLostHandler(func(error) { lost <- struct{}{} })

		if c.Start(t.Context()) == nil {
			select {
			case <-lost:
			case <-time.After(time.Second):
				t.Fatal("Start succeeded on an ended stream and no connection loss was reported")
			}
		}
		_ = c.Close()
		httpServer.Close()
	}
}

// A notification sent after Close, or after the server ended the stream, is
// refused and nothing is posted: the session it belonged to is over.
func TestSSE_ANotificationAfterTheEndIsRefused(t *testing.T) {
	notification := mcp.JSONRPCNotification{JSONRPC: mcp.JSONRPC_VERSION, Method: "notifications/initialized"}

	t.Run("closed", func(t *testing.T) {
		server := newScriptedSSEServer()
		c := startedSSE(t, server)
		require.NoError(t, c.Close())

		err := c.SendNotification(t.Context(), notification)
		assert.EqualError(t, err, "transport has been closed")
		assert.ErrorIs(t, err, ErrTransportClosed)
		assert.Zero(t, server.posts.Load(), "the notification was posted after Close")
	})

	t.Run("ended", func(t *testing.T) {
		server := newScriptedSSEServer()
		c := startedSSE(t, server)
		server.end()
		require.Eventually(t, c.ended.Load, 2*time.Second, 5*time.Millisecond)

		assert.ErrorIs(t, c.SendNotification(t.Context(), notification), ErrTransportClosed)
		assert.Zero(t, server.posts.Load(), "the notification was posted on an ended stream")
	})
}

// A stream that fails with a read error other than EOF, on a transport with
// no lost-connection handler, is logged as an ended one is: nothing else
// tells the caller why the stream stopped.
func TestSSE_AReadErrorIsLoggedWithoutAHandler(t *testing.T) {
	var logged bytes.Buffer
	c, err := NewSSE("http://127.0.0.1:1/sse", WithSSELogger(slog.New(slog.NewTextHandler(&logged, nil))))
	require.NoError(t, err)
	c.started.Store(true)

	c.readSSE(io.NopCloser(iotest.ErrReader(errors.New("connection reset by peer"))), make(chan struct{}), &sync.Once{}, 0)
	assert.Contains(t, logged.String(), "connection reset by peer")
}
