package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
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
	posts      atomic.Int32
}

func newScriptedSSEServer() *scriptedSSEServer {
	return &scriptedSSEServer{events: make(chan string, 16)}
}

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
