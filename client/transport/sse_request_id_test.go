package transport

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

	select {
	case method := <-notifications:
		assert.Equal(t, "notifications/message", method, "an error with a null id reached the notification handler")
	case <-time.After(2 * time.Second):
		t.Fatal("no notification arrived")
	}
}

// The SSE transport serves no server requests, but a request is never left
// unanswered (JSON-RPC 2.0, section 5): ping, which the receiver MUST answer
// on the earlier revisions, gets an empty result; any other method is not
// found; a request whose id cannot be read is invalid, answered with a null id.
func TestSSE_AServerRequestIsAnswered(t *testing.T) {
	posted := make(chan string, 4)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/message" {
			body, _ := io.ReadAll(r.Body)
			posted <- string(body)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: endpoint\ndata: %s/message\n\n", server.URL)
		for _, request := range []string{
			`{"jsonrpc":"2.0","id":7,"method":"ping"}`,
			`{"jsonrpc":"2.0","id":8,"method":"sampling/createMessage","params":{}}`,
			`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
			`{"jsonrpc":"2.0","id":1.5,"method":"ping"}`,
		} {
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", request)
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	trans, err := NewSSE(server.URL + "/sse")
	require.NoError(t, err)
	trans.SetNotificationHandler(func(notification mcp.JSONRPCNotification) {
		t.Errorf("a server request reached the notification handler: %+v", notification)
	})
	require.NoError(t, trans.Start(t.Context()))
	t.Cleanup(func() { _ = trans.Close() })

	want := []string{
		`{"jsonrpc":"2.0","id":7,"result":{}}`,
		`{"jsonrpc":"2.0","id":8,"error":{"code":-32601,"message":"Method not found: the SSE transport serves no server requests"}}`,
		`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request id"}}`,
		`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request id"}}`,
	}
	for _, answer := range want {
		select {
		case body := <-posted:
			assert.JSONEq(t, answer, body)
		case <-time.After(2 * time.Second):
			t.Fatalf("no answer posted; want %s", answer)
		}
	}
}
