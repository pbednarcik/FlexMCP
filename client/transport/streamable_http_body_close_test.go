package transport

import (
	"context"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// cancelFirstBody reads at once but blocks Close until the request's context
// ends, as an HTTP/2 response body can while the connection is busy.
type cancelFirstBody struct {
	data   []byte
	offset int
	reqCtx context.Context
	closed *atomic.Bool
}

func (b *cancelFirstBody) Read(p []byte) (int, error) {
	if b.offset >= len(b.data) {
		return 0, io.EOF
	}
	n := copy(p, b.data[b.offset:])
	b.offset += n
	return n, nil
}

func (b *cancelFirstBody) Close() error {
	<-b.reqCtx.Done()
	b.closed.Store(true)
	return nil
}

// cancelFirstTransport answers every request with status and a body whose
// Close waits for the request's context.
type cancelFirstTransport struct {
	status int
	closed *atomic.Bool
}

func (m cancelFirstTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: m.status,
		Header:     http.Header{"Content-Type": {"text/plain"}},
		Body:       &cancelFirstBody{data: []byte("error"), reqCtx: req.Context(), closed: m.closed},
	}, nil
}

// Every request the transport makes ends its context before it closes the
// response body, and closes it: an error answer, a 404 included, neither hangs
// on the body's Close nor leaves the body open.
func TestStreamableHTTPCancelsBeforeClosingAnErrorBody(t *testing.T) {
	request := JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(1)), Method: "tools/list"}
	// send runs on a goroutine of its own, so it only returns what the call
	// answered; terminated says the answer must be ErrSessionTerminated.
	tests := []struct {
		name       string
		status     int
		terminated bool
		send       func(ctx context.Context, tr *StreamableHTTP) error
	}{
		{"the GET stream, an error status", http.StatusInternalServerError, false, func(ctx context.Context, tr *StreamableHTTP) error {
			_, err := tr.createGETConnectionToServer(ctx)
			return err
		}},
		{"the GET stream, 404", http.StatusNotFound, true, func(ctx context.Context, tr *StreamableHTTP) error {
			_, err := tr.createGETConnectionToServer(ctx)
			return err
		}},
		{"a resumed stream, 404", http.StatusNotFound, true, func(ctx context.Context, tr *StreamableHTTP) error {
			_, err := tr.resumeSSE(ctx, &sseCursor{lastEventID: "1", hasRetry: true}, func(string, []byte) {})
			return err
		}},
		{"a request, 404", http.StatusNotFound, true, func(ctx context.Context, tr *StreamableHTTP) error {
			_, err := tr.SendRequest(ctx, request)
			return err
		}},
		{"a notification, 404", http.StatusNotFound, true, func(ctx context.Context, tr *StreamableHTTP) error {
			return tr.SendNotification(ctx, mcp.JSONRPCNotification{JSONRPC: "2.0", Method: "notifications/initialized"})
		}},
		{"a response to the server, 404", http.StatusNotFound, false, func(ctx context.Context, tr *StreamableHTTP) error {
			tr.sendResponseToServer(ctx, NewJSONRPCResultResponse(mcp.NewRequestId(int64(1)), []byte(`{}`)))
			return nil
		}},
		// Close's DELETE has a 5 s timeout of its own, so the wait below is
		// shorter than it.
		{"Close's DELETE", http.StatusOK, false, func(_ context.Context, tr *StreamableHTTP) error {
			tr.sessionID.Store("session-1")
			return tr.Close()
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var closed atomic.Bool
			tr, err := NewStreamableHTTP("http://mock-server/mcp",
				WithHTTPBasicClient(&http.Client{Transport: cancelFirstTransport{status: tt.status, closed: &closed}}))
			require.NoError(t, err)
			t.Cleanup(func() { _ = tr.Close() })

			done := make(chan error, 1)
			go func() { done <- tt.send(t.Context(), tr) }()
			select {
			case err := <-done:
				if tt.terminated {
					assert.ErrorIs(t, err, ErrSessionTerminated)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the body's Close ran before the request's context ended")
			}
			assert.True(t, closed.Load(), "the response body was left open")
		})
	}
}
