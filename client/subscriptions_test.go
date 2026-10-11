package client

import (
	"context"
	"encoding/json"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newListenServer returns an in-process server and a count of the
// subscriptions/listen streams it has closed.
func newListenServer(options ...server.ServerOption) (*server.MCPServer, *atomic.Int32) {
	closed := &atomic.Int32{}
	hooks := &server.Hooks{}
	hooks.AddAfterSubscriptionsListen(func(context.Context, any, *mcp.SubscriptionsListenRequest, *mcp.SubscriptionsListenResult) {
		closed.Add(1)
	})
	options = append(options, server.WithHooks(hooks))
	return server.NewMCPServer("test-server", "1.0.0", options...), closed
}

// collectNotifications delivers every notification the client receives on a
// channel.
func collectNotifications(c *Client) <-chan mcp.JSONRPCNotification {
	notifications := make(chan mcp.JSONRPCNotification, 16)
	c.OnNotification(func(n mcp.JSONRPCNotification) { notifications <- n })
	return notifications
}

func TestListenStreamsTheSubscribedNotificationsUntilStopped(t *testing.T) {
	s, closed := newListenServer(server.WithToolCapabilities(true))
	c := connectInProcess(t, s, "")
	require.Equal(t, mcp.ProtocolVersion20260728, c.ProtocolVersion())
	notifications := collectNotifications(c)

	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	done := make(chan error, 1)
	go func() {
		done <- c.Listen(ctx, mcp.SubscriptionFilter{ToolsListChanged: true, PromptsListChanged: true})
	}()

	ack := receive(t, notifications)
	require.Equal(t, mcp.MethodNotificationSubscriptionsAcknowledged, ack.Method)
	assert.NotNil(t, ack.Params.Meta[mcp.MetaKeySubscriptionID])
	assert.Equal(t, mcp.SubscriptionFilter{ToolsListChanged: true}, ack.Params.AdditionalFields["notifications"],
		"the server establishes only what it can serve")

	s.AddTool(mcp.NewTool("late"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	changed := receive(t, notifications)
	assert.Equal(t, mcp.MethodNotificationToolsListChanged, changed.Method)
	assert.Equal(t, ack.Params.Meta[mcp.MetaKeySubscriptionID], changed.Params.Meta[mcp.MetaKeySubscriptionID],
		"every notification on the stream carries the stream's id")
	assert.Equal(t, int32(0), closed.Load(), "the stream stays open while the context lives")

	stop()
	require.NoError(t, receive(t, done))
	assert.Equal(t, int32(1), closed.Load())
}

func TestListenReturnsAtOnceWhenNothingWasEstablished(t *testing.T) {
	s, closed := newListenServer()
	c := connectInProcess(t, s, "")
	notifications := collectNotifications(c)

	require.NoError(t, c.Listen(t.Context(), mcp.SubscriptionFilter{ToolsListChanged: true}))

	ack := receive(t, notifications)
	assert.Equal(t, mcp.MethodNotificationSubscriptionsAcknowledged, ack.Method)
	assert.Equal(t, mcp.SubscriptionFilter{}, ack.Params.AdditionalFields["notifications"])
	assert.Equal(t, int32(1), closed.Load())
}

func TestListenAsyncStopsTheStream(t *testing.T) {
	s, closed := newListenServer(server.WithToolCapabilities(true))
	c := connectInProcess(t, s, "")
	notifications := collectNotifications(c)

	errs := make(chan error, 1)
	stop, err := c.ListenAsync(t.Context(), mcp.SubscriptionFilter{ToolsListChanged: true}, func(err error) { errs <- err })
	require.NoError(t, err)
	require.Equal(t, mcp.MethodNotificationSubscriptionsAcknowledged, receive(t, notifications).Method)

	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	receive(t, stopped)

	assert.Equal(t, int32(1), closed.Load(), "stop returns once the server has closed the stream")
	select {
	case err := <-errs:
		t.Fatalf("stopping was reported as an error: %v", err)
	default:
	}
}

func TestListenRequiresAModernConnection(t *testing.T) {
	hooks := &server.Hooks{}
	recorder := recordRequests(hooks)
	s := server.NewMCPServer("test-server", "1.0.0", server.WithToolCapabilities(true), server.WithHooks(hooks))
	c := connectInProcess(t, s, mcp.LATEST_LEGACY_PROTOCOL_VERSION)
	require.Equal(t, mcp.LATEST_LEGACY_PROTOCOL_VERSION, c.ProtocolVersion())

	err := c.Listen(t.Context(), mcp.SubscriptionFilter{ToolsListChanged: true})
	require.ErrorContains(t, err, "requires protocol version "+mcp.ProtocolVersion20260728)

	stop, err := c.ListenAsync(t.Context(), mcp.SubscriptionFilter{ToolsListChanged: true}, nil)
	require.ErrorContains(t, err, "requires protocol version "+mcp.ProtocolVersion20260728)
	assert.Nil(t, stop)

	assert.NotContains(t, recorder.received(), string(mcp.MethodSubscriptionsListen),
		"the refusal is the client's own; nothing is sent")
}

func TestListenReportsTheServersError(t *testing.T) {
	tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		return transport.NewJSONRPCErrorResponse(request.ID, mcp.INTERNAL_ERROR, "stream refused", nil), nil
	})}
	c := connectScripted(t, tr)
	require.Equal(t, mcp.ProtocolVersion20260728, c.ProtocolVersion())

	err := c.Listen(t.Context(), mcp.SubscriptionFilter{ToolsListChanged: true})
	require.ErrorContains(t, err, "stream refused")

	errs := make(chan error, 1)
	stop, err := c.ListenAsync(t.Context(), mcp.SubscriptionFilter{ToolsListChanged: true}, func(err error) { errs <- err })
	require.NoError(t, err)
	defer stop()
	assert.ErrorContains(t, receive(t, errs), "stream refused")
}

func TestListenAsyncDoesNotReportStopping(t *testing.T) {
	listening := make(chan struct{}, 1)
	tr := &scriptedTransport{respond: modernScript(func(ctx context.Context, _ transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		listening <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})}
	c := connectScripted(t, tr)

	errs := make(chan error, 1)
	stop, err := c.ListenAsync(t.Context(), mcp.SubscriptionFilter{ToolsListChanged: true}, func(err error) { errs <- err })
	require.NoError(t, err)
	receive(t, listening)
	stop()

	select {
	case err := <-errs:
		t.Fatalf("stopping was reported as an error: %v", err)
	default:
	}
	assert.Equal(t, []string{string(mcp.MethodServerDiscover), string(mcp.MethodSubscriptionsListen)}, tr.sentMethods())
}

// requestRecorder lists the methods of the requests a server received.
type requestRecorder struct {
	mu      sync.Mutex
	methods []string
}

func recordRequests(hooks *server.Hooks) *requestRecorder {
	recorder := &requestRecorder{}
	hooks.AddOnRequestInitialization(func(_ context.Context, _ any, message any) error {
		raw, _ := message.(json.RawMessage)
		var request struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(raw, &request)
		recorder.mu.Lock()
		defer recorder.mu.Unlock()
		recorder.methods = append(recorder.methods, request.Method)
		return nil
	})
	return recorder
}

func (r *requestRecorder) received() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.methods)
}

func TestSubscribeIsSentOnTheWireOnALegacyConnection(t *testing.T) {
	hooks := &server.Hooks{}
	recorder := recordRequests(hooks)
	s := server.NewMCPServer("test-server", "1.0.0", server.WithResourceCapabilities(true, true), server.WithHooks(hooks))
	c := connectInProcess(t, s, mcp.LATEST_LEGACY_PROTOCOL_VERSION)

	err := c.Subscribe(t.Context(), mcp.SubscribeRequest{Params: mcp.SubscribeParams{URI: "test://a"}})
	require.Error(t, err, "this server serves no resources/subscribe")
	assert.Contains(t, recorder.received(), string(mcp.MethodResourcesSubscribe))
	assert.Empty(t, c.PendingSubscriptionFilter().ResourceSubscriptions,
		"nothing is recorded for a Listen that a legacy connection cannot open")

	err = c.Unsubscribe(t.Context(), mcp.UnsubscribeRequest{Params: mcp.UnsubscribeParams{URI: "test://a"}})
	require.Error(t, err)
	assert.Contains(t, recorder.received(), string(mcp.MethodResourcesUnsubscribe))
}
