package transport

import (
	"fmt"
	"io"
	"log/slog"
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
// A method named "" is a method all the same, and an id is matched as a
// number or as a string alike.
func TestSSE_AServerRequestIsNotTakenForAResponse(t *testing.T) {
	ids := []struct {
		name    string
		encoded string
		id      mcp.RequestId
	}{
		{"numeric id", `1`, mcp.NewRequestId(int64(1))},
		{"string id", `"req-1"`, mcp.NewRequestId("req-1")},
	}
	for _, tc := range ids {
		for _, method := range []string{"ping", ""} {
			t.Run(tc.name+", method "+method, func(t *testing.T) {
				posted := make(chan struct{}, 4)
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
						_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"method\":%q}\n\n", tc.encoded, method)
						_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"answer\":42}}\n\n", tc.encoded)
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

				response, err := trans.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: tc.id, Method: "tools/list"})
				require.NoError(t, err)
				assert.JSONEq(t, `{"answer":42}`, string(response.Result))
			})
		}
	}
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
// found; a request whose id cannot be read is invalid, answered with a null id,
// and one whose jsonrpc member is not "2.0" is invalid, answered with its id.
func TestSSE_AServerRequestIsAnswered(t *testing.T) {
	posted := make(chan string, 10)
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
			`{"jsonrpc":"2.0","id":9,"method":""}`,
			`{"jsonrpc":"2.0","id":10,"method":null,"result":{}}`,
			`{"jsonrpc":"1.0","id":11,"method":"ping"}`,
			`{"id":12,"method":"ping"}`,
			`{"jsonrpc":2,"id":13,"method":"ping"}`,
			`{"jsonrpc":"2\u002e0","id":14,"method":"ping"}`,
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
		`{"jsonrpc":"2.0","id":9,"error":{"code":-32601,"message":"Method not found: the SSE transport serves no server requests"}}`,
		`{"jsonrpc":"2.0","id":10,"error":{"code":-32600,"message":"Invalid request: method must be a string"}}`,
		`{"jsonrpc":"2.0","id":11,"error":{"code":-32600,"message":"Invalid request: jsonrpc must be \"2.0\""}}`,
		`{"jsonrpc":"2.0","id":12,"error":{"code":-32600,"message":"Invalid request: jsonrpc must be \"2.0\""}}`,
		`{"jsonrpc":"2.0","id":13,"error":{"code":-32600,"message":"Invalid request: jsonrpc must be \"2.0\""}}`,
		`{"jsonrpc":"2.0","id":14,"result":{}}`,
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

// An answer to a server request is posted on the stream's reader, so Close
// abandons one still in flight rather than leave it posting after the
// transport is shut down.
func TestSSE_AnAnswerDoesNotOutliveClose(t *testing.T) {
	arrived := make(chan struct{})
	abandoned := make(chan struct{})
	release := make(chan struct{})
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/message" {
			// The server sees the client go only once the body is read.
			_, _ = io.Copy(io.Discard, r.Body)
			close(arrived)
			select {
			case <-r.Context().Done():
				close(abandoned)
			case <-release:
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: endpoint\ndata: %s/message\n\n", server.URL)
		_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":7,\"method\":\"ping\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	trans, err := NewSSE(server.URL + "/sse")
	require.NoError(t, err)
	require.NoError(t, trans.Start(t.Context()))

	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("the ping was not answered")
	}
	require.NoError(t, trans.Close())
	select {
	case <-abandoned:
	case <-time.After(2 * time.Second):
		t.Fatal("the answer was still being posted after Close")
	}
}

// An answer the server refuses is logged and the stream goes on: a later
// message still reaches the client.
func TestSSE_AFailedAnswerLeavesTheStreamReading(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/message" {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: endpoint\ndata: %s/message\n\n", server.URL)
		_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":7,\"method\":\"ping\"}\n\n")
		_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	trans, err := NewSSE(server.URL+"/sse", WithSSELogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	require.NoError(t, err)
	notifications := make(chan string, 1)
	trans.SetNotificationHandler(func(notification mcp.JSONRPCNotification) {
		notifications <- notification.Method
	})
	require.NoError(t, trans.Start(t.Context()))
	t.Cleanup(func() { _ = trans.Close() })

	select {
	case method := <-notifications:
		assert.Equal(t, "notifications/message", method)
	case <-time.After(2 * time.Second):
		t.Fatal("the stream stopped after a failed answer")
	}
}
