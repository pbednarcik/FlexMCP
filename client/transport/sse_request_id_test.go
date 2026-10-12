package transport

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// Request ids are per sender: a server request that happens to carry the id of
// a request the client is waiting on is not that request's response.
func TestSSE_AServerRequestIsNotTakenForAResponse(t *testing.T) {
	posted := make(chan struct{}, 1)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sse":
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			_, _ = fmt.Fprintf(w, "event: endpoint\ndata: %s/message\n\n", server.URL)
			flusher.Flush()
			select {
			case <-posted:
			case <-r.Context().Done():
				return
			}
			_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ping\"}\n\n")
			_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"answer\":42}}\n\n")
			flusher.Flush()
			<-r.Context().Done()
		case "/message":
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusAccepted)
			posted <- struct{}{}
		}
	}))
	t.Cleanup(server.Close)

	trans, err := NewSSE(server.URL + "/sse")
	require.NoError(t, err)
	require.NoError(t, trans.Start(t.Context()))
	t.Cleanup(func() { _ = trans.Close() })

	response, err := trans.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Method: "tools/list"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"answer":42}`, string(response.Result))
}

// An error with a null id is a response, not a notification: the shared SSE
// stream cannot tell which request it answers, so it reaches no handler.
func TestSSE_AnErrorWithANullIDIsNotANotification(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = fmt.Fprintf(w, "event: endpoint\ndata: %s/message\n\n", server.URL)
		_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":null,\"error\":{\"code\":-32600,\"message\":\"Invalid request id\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\"}\n\n")
		flusher.Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	trans, err := NewSSE(server.URL + "/sse")
	require.NoError(t, err)
	notifications := make(chan string, 2)
	trans.SetNotificationHandler(func(notification mcp.JSONRPCNotification) {
		notifications <- notification.Method
	})
	require.NoError(t, trans.Start(t.Context()))
	t.Cleanup(func() { _ = trans.Close() })

	assert.Equal(t, "notifications/message", <-notifications, "an error with a null id reached the notification handler")
}
