package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// forwardersRunning counts the goroutines handlePost started that are still
// running: the notification forwarders. These tests are not parallel, so the
// count is theirs (parallel tests run after every sequential one); each
// compares against the count taken just before its request, so a goroutine an
// earlier test left behind does not count.
func forwardersRunning() int {
	buf := make([]byte, 1<<22)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "created by github.com/mark3labs/mcp-go/server.(*StreamableHTTPServer).handlePost")
}

// postToolCall sends one legacy tools/call of name and returns the response.
func postToolCall(t *testing.T, h http.Handler, name string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":{}}}`
	r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// A call that sends no notification starts no forwarder goroutine: most calls
// send none, and a goroutine per request costs a thread wakeup each.
func TestStreamableHTTPCallWithoutNotificationsStartsNoForwarder(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	var before, during int
	srv.AddTool(mcp.NewTool("quiet"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		during = forwardersRunning()
		return mcp.NewToolResultText("done"), nil
	})
	h := NewStreamableHTTPServer(srv)

	before = forwardersRunning()
	w := postToolCall(t, h, "quiet")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.LessOrEqual(t, during, before, "a call that sent no notification ran a forwarder")
}

// A call that sends notifications gets them before its response, and its
// forwarder has ended when the response is written: no goroutine outlives its
// request.
func TestStreamableHTTPForwarderEndsWithItsRequest(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	srv.AddTool(mcp.NewTool("chatty"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		for range 3 {
			if err := srv.SendNotificationToClient(ctx, "notifications/message", map[string]any{"level": "info", "data": "x"}); err != nil {
				return nil, err
			}
		}
		return mcp.NewToolResultText("done"), nil
	})
	h := NewStreamableHTTPServer(srv)

	before := forwardersRunning()
	w := postToolCall(t, h, "chatty")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	body := w.Body.String()
	assert.Equal(t, 3, strings.Count(body, `"method":"notifications/message"`), body)
	assert.Less(t, strings.LastIndex(body, "notifications/message"), strings.Index(body, `"result"`),
		"every notification comes before the response")
	assert.LessOrEqual(t, forwardersRunning(), before, "the forwarder outlived its request")
}

// A notification sent after the request stopped forwarding still finds a
// channel and waits there for the drain, as before; it starts no forwarder.
// The order is forced, not raced: a clean -race run would not prove it.
func TestStreamableHTTPSessionNotificationAfterForwardingStops(t *testing.T) {
	s := newStreamableHttpSession()
	starts := 0
	s.forwardWith(forwarderFunc(func(<-chan mcp.JSONRPCNotification) { starts++ }))
	assert.False(t, s.stopForwarding(), "nothing was sent, so no forwarder ran")

	s.NotificationChannel() <- mcp.JSONRPCNotification{Method: "late"}
	assert.Equal(t, 0, starts, "a notification after forwarding stopped started a forwarder")
	select {
	case n := <-s.pending():
		assert.Equal(t, "late", n.Method)
	default:
		t.Fatal("the late notification is not waiting for the drain")
	}
}

// The first notification starts the forwarder once, on the channel the
// notification was queued on; one queued before forwarding was armed starts
// it when armed.
func TestStreamableHTTPSessionStartsItsForwarderOnce(t *testing.T) {
	s := newStreamableHttpSession()
	var forwarded []<-chan mcp.JSONRPCNotification
	s.forwardWith(forwarderFunc(func(ch <-chan mcp.JSONRPCNotification) { forwarded = append(forwarded, ch) }))
	s.NotificationChannel() <- mcp.JSONRPCNotification{Method: "first"}
	s.NotificationChannel() <- mcp.JSONRPCNotification{Method: "second"}
	require.Len(t, forwarded, 1, "the forwarder starts once")
	assert.Equal(t, "first", (<-forwarded[0]).Method)
	assert.True(t, s.stopForwarding(), "a forwarder ran")

	early := newStreamableHttpSession()
	early.NotificationChannel() <- mcp.JSONRPCNotification{Method: "early"}
	starts := 0
	early.forwardWith(forwarderFunc(func(<-chan mcp.JSONRPCNotification) { starts++ }))
	assert.Equal(t, 1, starts, "a notification queued before forwarding was armed starts the forwarder when armed")
}

// forwarderFunc forwards by calling itself.
type forwarderFunc func(<-chan mcp.JSONRPCNotification)

func (f forwarderFunc) forward(notifications <-chan mcp.JSONRPCNotification) { f(notifications) }

type parentKey struct{}

// A request's context holds the transport's values in one layer: the session,
// the versions and the header are its own, every other value and the
// cancellation its parent's, and a context made from it is canceled with the
// parent.
func TestPostContextHoldsTheTransportValues(t *testing.T) {
	parent, cancel := context.WithCancel(context.WithValue(t.Context(), parentKey{}, "parent"))
	session := newStreamableHttpSession()
	header := http.Header{"X-Test": {"1"}}
	versions := []string{mcp.ProtocolVersion20260728}
	ctx := &postContext{Context: parent, session: session, versions: any(versions), header: header}

	assert.Same(t, session, ClientSessionFromContext(ctx))
	assert.Equal(t, versions, SupportedProtocolVersionsFromContext(ctx))
	assert.Equal(t, header, ctx.Value(requestHeader))
	assert.Equal(t, "parent", ctx.Value(parentKey{}))
	assert.Nil(t, ctx.Value(struct{ unknown int }{}))

	child, stop := context.WithCancel(ctx)
	defer stop()
	cancel()
	<-child.Done()
	assert.ErrorIs(t, ctx.Err(), context.Canceled)
}
