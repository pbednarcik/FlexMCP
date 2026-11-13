package client

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
)

// targetedInProcess is an in-process client of a server that names its one
// session, for the sends that take a session id.
type targetedInProcess struct {
	server        *server.MCPServer
	client        *Client
	notifications <-chan mcp.JSONRPCNotification
	session       server.ClientSession
	sessionID     string
}

func newTargetedInProcess(t *testing.T, version string) *targetedInProcess {
	t.Helper()
	hooks := &server.Hooks{}
	capture := captureSession(hooks)
	s := server.NewMCPServer("test", "1.0.0",
		server.WithToolCapabilities(true),
		server.WithPromptCapabilities(true),
		server.WithHooks(hooks),
	)
	c := connectInProcess(t, s, version)
	require.Equal(t, version, c.ProtocolVersion())
	session := capture.get(t)
	return &targetedInProcess{
		server:        s,
		client:        c,
		notifications: collectNotifications(c),
		session:       session,
		sessionID:     session.SessionID(),
	}
}

// listen opens a tools-only listen stream for the life of the test and
// returns its id from the acknowledgement, once the session holds the
// stream: the acknowledgement is queued before the stream is recorded. It
// sends the request itself, because Client.Listen ends the stream of an
// earlier call, and a session may hold several.
func (tp *targetedInProcess) listen(t *testing.T) any {
	t.Helper()
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		params := mcp.SubscriptionsListenParams{Notifications: mcp.SubscriptionFilter{ToolsListChanged: true}}
		_, _ = tp.client.sendRequest(ctx, string(mcp.MethodSubscriptionsListen), params, nil)
	}()
	t.Cleanup(func() { stop(); <-done })
	ack := receive(t, tp.notifications)
	require.Equal(t, mcp.MethodNotificationSubscriptionsAcknowledged, ack.Method)
	id := ack.Params.Meta[mcp.MetaKeySubscriptionID]
	require.NotNil(t, id)
	streams, ok := tp.session.(server.SessionWithSubscriptionStreams)
	require.True(t, ok, "the in-process session holds listen streams")
	require.Eventually(t, func() bool {
		open, _ := streams.SubscriptionStreams()
		return slices.ContainsFunc(open, func(s server.SubscriptionStream) bool { return s.ID == id })
	}, 5*time.Second, time.Millisecond, "the session records stream %v", id)
	return id
}

// quiet fails the test if a notification arrives within 200 ms.
func (tp *targetedInProcess) quiet(t *testing.T) {
	t.Helper()
	select {
	case n := <-tp.notifications:
		t.Errorf("unexpected notification %s %v", n.Method, n.Params.Meta)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestInProcessTargetedNotificationHonoursTheStream pins that a notification
// sent to one in-process session outside a request passes the session's
// subscriptions/listen streams as a broadcast does (Patterns.md rows 14 and
// 17): a modern session receives only the types a stream asked for, one copy
// per such stream tagged with its id, and nothing without one; a
// handshake-era session receives it as before, untagged.
func TestInProcessTargetedNotificationHonoursTheStream(t *testing.T) {
	tools := mcp.MethodNotificationToolsListChanged
	errorLog := mcp.NewLoggingMessageNotification(mcp.LoggingLevelError, "test-logger", "boom")
	unrequested := map[string]func(s *server.MCPServer, id string) error{
		"a list change": func(s *server.MCPServer, id string) error {
			return s.SendNotificationToSpecificClient(id, mcp.MethodNotificationPromptsListChanged, nil)
		},
		"a custom method": func(s *server.MCPServer, id string) error {
			return s.SendNotificationToSpecificClient(id, "custom/x", nil)
		},
		"a log message": func(s *server.MCPServer, id string) error {
			return s.SendLogMessageToSpecificClient(id, errorLog)
		},
	}
	for name, sendUnrequested := range unrequested {
		t.Run(name+" the stream did not ask for is not delivered", func(t *testing.T) {
			tp := newTargetedInProcess(t, mcp.ProtocolVersion20260728)
			id := tp.listen(t)
			require.NoError(t, sendUnrequested(tp.server, tp.sessionID))
			require.NoError(t, tp.server.SendNotificationToSpecificClient(tp.sessionID, tools, nil))
			first := receive(t, tp.notifications)
			assert.Equal(t, tools, first.Method, "only the requested type is delivered")
			assert.Equal(t, id, first.Params.Meta[mcp.MetaKeySubscriptionID], "the delivery carries its stream's id")
		})
	}

	t.Run("two streams receive a tagged copy each", func(t *testing.T) {
		tp := newTargetedInProcess(t, mcp.ProtocolVersion20260728)
		one, two := tp.listen(t), tp.listen(t)
		require.NoError(t, tp.server.SendNotificationToSpecificClient(tp.sessionID, tools, nil))
		first, second := receive(t, tp.notifications), receive(t, tp.notifications)
		assert.Equal(t, tools, first.Method)
		assert.Equal(t, tools, second.Method)
		assert.ElementsMatch(t, []any{one, two}, []any{
			first.Params.Meta[mcp.MetaKeySubscriptionID],
			second.Params.Meta[mcp.MetaKeySubscriptionID],
		})
		tp.quiet(t)
	})

	t.Run("a modern session without a stream receives nothing", func(t *testing.T) {
		tp := newTargetedInProcess(t, mcp.ProtocolVersion20260728)
		_, err := tp.client.ListTools(t.Context(), mcp.ListToolsRequest{})
		require.NoError(t, err)
		require.NoError(t, tp.server.SendNotificationToSpecificClient(tp.sessionID, tools, nil))
		require.NoError(t, tp.server.SendLogMessageToSpecificClient(tp.sessionID, errorLog))
		tp.quiet(t)
	})

	t.Run("a handshake-era session receives it untagged", func(t *testing.T) {
		tp := newTargetedInProcess(t, mcp.ProtocolVersion20250618)
		prompts := mcp.MethodNotificationPromptsListChanged
		require.NoError(t, tp.server.SendNotificationToSpecificClient(tp.sessionID, prompts, nil))
		first := receive(t, tp.notifications)
		assert.Equal(t, prompts, first.Method)
		assert.Nil(t, first.Params.Meta[mcp.MetaKeySubscriptionID], "a handshake-era notification is not tagged")
	})
}
