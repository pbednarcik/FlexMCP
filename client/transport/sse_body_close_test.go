package transport

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// endpointStream is a legacy SSE stream that sends the endpoint event and
// then stays open until its request's context ends.
type endpointStream struct {
	event  io.Reader
	reqCtx context.Context
}

func (s *endpointStream) Read(p []byte) (int, error) {
	if n, err := s.event.Read(p); err != io.EOF {
		return n, err
	}
	<-s.reqCtx.Done()
	return 0, s.reqCtx.Err()
}

func (s *endpointStream) Close() error { return nil }

// sseCancelFirstTransport opens the legacy SSE stream when getStatus is 200
// and answers the GET otherwise, and every POST, with status and a body whose
// Close waits for the request's context.
type sseCancelFirstTransport struct {
	getStatus, postStatus int
	closed                *atomic.Bool
}

func (m sseCancelFirstTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	status := m.postStatus
	if req.Method == http.MethodGet {
		if m.getStatus == http.StatusOK {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       &endpointStream{event: strings.NewReader("event: endpoint\ndata: /message\n\n"), reqCtx: req.Context()},
			}, nil
		}
		status = m.getStatus
	}
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"text/plain"}},
		Body:       &cancelFirstBody{data: []byte("error"), reqCtx: req.Context(), closed: m.closed},
	}, nil
}

// The legacy SSE transport ends a request's context before it closes an error
// answer's body, and closes it, as Streamable HTTP does.
func TestSSECancelsBeforeClosingAnErrorBody(t *testing.T) {
	// started rows open the stream on the test's goroutine first; send runs
	// on a goroutine of its own and returns the error the call must answer.
	tests := []struct {
		name    string
		started bool
		send    func(ctx context.Context, tr *SSE) error
	}{
		{"the stream, an error status", false, func(ctx context.Context, tr *SSE) error {
			return tr.Start(ctx)
		}},
		{"a request, an error status", true, func(ctx context.Context, tr *SSE) error {
			_, err := tr.SendRequest(ctx, JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(1)), Method: "tools/list"})
			return err
		}},
		{"a notification, an error status", true, func(ctx context.Context, tr *SSE) error {
			return tr.SendNotification(ctx, mcp.JSONRPCNotification{JSONRPC: "2.0", Method: "notifications/initialized"})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getStatus := http.StatusInternalServerError
			if tt.started {
				getStatus = http.StatusOK
			}
			var closed atomic.Bool
			tr, err := NewSSE("http://mock-server/sse", WithHTTPClient(&http.Client{
				Transport: sseCancelFirstTransport{getStatus: getStatus, postStatus: http.StatusInternalServerError, closed: &closed},
			}))
			require.NoError(t, err)
			t.Cleanup(func() { _ = tr.Close() })
			if tt.started {
				require.NoError(t, tr.Start(t.Context()))
			}

			done := make(chan error, 1)
			go func() { done <- tt.send(t.Context(), tr) }()
			select {
			case err := <-done:
				assert.Error(t, err)
			case <-time.After(3 * time.Second):
				t.Fatal("the body's Close ran before the request's context ended")
			}
			assert.True(t, closed.Load(), "the response body was left open")
		})
	}
}
