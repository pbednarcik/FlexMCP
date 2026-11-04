package transport

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
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
