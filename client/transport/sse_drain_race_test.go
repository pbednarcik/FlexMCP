package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// knownLengthStream is an SSE body that states its length and runs on past
// its first event, so the reader the client hands it to is still reading
// when the event it waited for has arrived.
func knownLengthStream(first string) string {
	return "data: " + first + "\n\n" + strings.Repeat(": padding padding padding padding padding\n", 4000)
}

// newHTTP2Server serves handler over TLS with HTTP/2, whose response bodies
// the race detector watches.
func newHTTP2Server(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// A request answered with an event stream of known length is released without
// a second read of the body: the SSE reader may still be reading it when the
// answer has arrived. Under -race a drain beside the reader is reported.
func TestSendRequestDoesNotDrainAnEventStream(t *testing.T) {
	srv := newHTTP2Server(t, func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		payload := knownLengthStream(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":{}}`)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = w.Write([]byte(payload))
	})
	tr, err := NewStreamableHTTP(srv.URL, WithHTTPBasicClient(srv.Client()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = tr.Close() })

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for i := range 200 {
		_, err := tr.SendRequest(ctx, JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(i + 1)), Method: "tools/list"})
		require.NoError(t, err)
	}
}

// The GET stream is released the same way when its context ends while the
// reader is still reading.
func TestGETStreamDoesNotDrainAnEventStream(t *testing.T) {
	payload := knownLengthStream(`{"jsonrpc":"2.0","method":"notifications/test"}`)
	srv := newHTTP2Server(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		_, _ = w.Write([]byte(payload))
	})
	tr, err := NewStreamableHTTP(srv.URL, WithHTTPBasicClient(srv.Client()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = tr.Close() })

	for range 200 {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		tr.SetNotificationHandler(func(mcp.JSONRPCNotification) { cancel() })
		_, _ = tr.createGETConnectionToServer(ctx)
		cancel()
	}
}
