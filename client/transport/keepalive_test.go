package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// A POST whose answer is not read keeps its HTTP/1.1 connection when the
// server answers 202 with a short body of known length: the body is read to
// its end before the request's context ends, as net/http's own drain on
// Close would have.
func TestPOSTAnswersWithABodyKeepTheConnection(t *testing.T) {
	const posts = 10
	notification := mcp.JSONRPCNotification{JSONRPC: "2.0", Method: "notifications/initialized"}
	tests := []struct {
		name  string
		conns int64 // the connections the client may open
		run   func(t *testing.T, url string, client *http.Client)
	}{
		{"legacy SSE notifications", 2, func(t *testing.T, url string, client *http.Client) {
			tr, err := NewSSE(url+"/sse", WithHTTPClient(client))
			require.NoError(t, err)
			t.Cleanup(func() { _ = tr.Close() })
			require.NoError(t, tr.Start(t.Context()))
			for range posts {
				require.NoError(t, tr.SendNotification(t.Context(), notification))
			}
		}},
		{"Streamable HTTP notifications", 1, func(t *testing.T, url string, client *http.Client) {
			tr, err := NewStreamableHTTP(url, WithHTTPBasicClient(client))
			require.NoError(t, err)
			t.Cleanup(func() { _ = tr.Close() })
			for range posts {
				require.NoError(t, tr.SendNotification(t.Context(), notification))
			}
		}},
		{"Streamable HTTP responses to the server", 1, func(t *testing.T, url string, client *http.Client) {
			tr, err := NewStreamableHTTP(url, WithHTTPBasicClient(client))
			require.NoError(t, err)
			t.Cleanup(func() { _ = tr.Close() })
			for range posts {
				tr.sendResponseToServer(t.Context(), NewJSONRPCResultResponse(mcp.NewRequestId(int64(1)), []byte(`{}`)))
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var conns atomic.Int64
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("event: endpoint\ndata: /message\n\n"))
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					return
				}
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte("Accepted"))
			}))
			srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
				if state == http.StateNew {
					conns.Add(1)
				}
			}
			srv.Start()
			t.Cleanup(srv.Close)
			client := &http.Client{Transport: &http.Transport{}}
			t.Cleanup(client.CloseIdleConnections)

			tt.run(t, srv.URL, client)
			assert.LessOrEqual(t, conns.Load(), tt.conns, "connections opened for %d posts", posts)
		})
	}
}

// stalledBody claims a length its bytes never reach: Read waits for the
// request's context.
type stalledBody struct{ reqCtx context.Context }

func (b stalledBody) Read([]byte) (int, error) {
	<-b.reqCtx.Done()
	return 0, b.reqCtx.Err()
}

func (stalledBody) Close() error { return nil }

type stalledBodyTransport struct{}

func (stalledBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusAccepted, ContentLength: 8, Body: stalledBody{reqCtx: req.Context()}}, nil
}

// The read of a body left unread is cut after maxDrainTime: a server that
// states a length and never sends the bytes does not hold the call.
func TestReleaseResponseDrainIsCut(t *testing.T) {
	tr, err := NewStreamableHTTP("http://mock-server/mcp", WithHTTPBasicClient(&http.Client{Transport: stalledBodyTransport{}}))
	require.NoError(t, err)
	t.Cleanup(func() { _ = tr.Close() })

	done := make(chan error, 1)
	go func() {
		done <- tr.SendNotification(t.Context(), mcp.JSONRPCNotification{JSONRPC: "2.0", Method: "notifications/initialized"})
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("the drain of a stalled body held the notification")
	}
}

// countConnections starts handler on a server that counts the connections
// its clients open.
func countConnections(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var conns atomic.Int64
	srv := httptest.NewUnstartedServer(handler)
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &conns
}

// A request keeps its connection whatever the size of its JSON answer. The
// decoder stops at the closing brace, before json.Encoder's newline, and at
// these sizes the body's last read ends on that brace.
func TestSendRequestKeepsTheConnectionAfterAJSONAnswer(t *testing.T) {
	for _, size := range []int{129, 257, 513, 1025} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			srv, conns := countConnections(t, func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					ID json.RawMessage `json:"id"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				head := `{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":{"pad":"`
				tail := `"}}`
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(json.RawMessage(head + strings.Repeat("x", size-1-len(head)-len(tail)) + tail))
			})
			client := &http.Client{Transport: &http.Transport{}}
			t.Cleanup(client.CloseIdleConnections)
			tr, err := NewStreamableHTTP(srv.URL, WithHTTPBasicClient(client))
			require.NoError(t, err)
			t.Cleanup(func() { _ = tr.Close() })

			for i := range 10 {
				resp, err := tr.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(i + 1)), Method: "tools/list"})
				require.NoError(t, err)
				require.Nil(t, resp.Error)
			}
			assert.Equal(t, int64(1), conns.Load(), "connections opened for 10 requests")
		})
	}
}

// A resumed stream the server refuses with a body keeps its connection.
func TestResumeSSEKeepsTheConnectionAfterARefusal(t *testing.T) {
	srv, conns := countConnections(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such stream", http.StatusBadRequest)
	})
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)
	tr, err := NewStreamableHTTP(srv.URL, WithHTTPBasicClient(client))
	require.NoError(t, err)
	t.Cleanup(func() { _ = tr.Close() })

	for range 10 {
		_, err := tr.resumeSSE(t.Context(), &sseCursor{lastEventID: "1", hasRetry: true}, func(string, string) {})
		require.Error(t, err)
	}
	assert.Equal(t, int64(1), conns.Load(), "connections opened for 10 refused resumes")
}

// countingBody counts the bytes read from it.
type countingBody struct {
	io.Reader
	read *atomic.Int64
}

func (b countingBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read.Add(int64(n))
	return n, err
}

func (countingBody) Close() error { return nil }

// releaseResponse reads a body only when its length is stated and at most
// maxDrainBytes: a longer one, or one of unknown length, is closed unread. An
// event stream is never read: its body went to an SSE reader, which may still
// be reading it.
func TestReleaseResponseDrainsOnlyAShortBodyOfKnownLength(t *testing.T) {
	tests := []struct {
		name          string
		contentType   string
		contentLength int64
		drained       bool
	}{
		{"a short body", "application/json", 8, true},
		{"a body of the limit", "application/json", maxDrainBytes, true},
		{"a body past the limit", "application/json", maxDrainBytes + 1, false},
		{"a body of unknown length", "application/json", -1, false},
		{"an event stream", "text/event-stream", 8, false},
		{"an event stream with parameters", "text/event-stream; charset=utf-8", 8, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var read atomic.Int64
			size := max(tt.contentLength, 8)
			resp := &http.Response{
				Header:        http.Header{"Content-Type": {tt.contentType}},
				ContentLength: tt.contentLength,
				Body:          countingBody{Reader: strings.NewReader(strings.Repeat("x", int(size))), read: &read},
			}
			releaseResponse(func() {}, resp)
			assert.Equal(t, tt.drained, read.Load() > 0)
		})
	}
}
