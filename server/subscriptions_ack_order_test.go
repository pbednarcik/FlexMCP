package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// broadcastOnSubscribe is a listen session on which a broadcast lands the
// moment its filter is set: the narrowest window between a subscription
// being recorded and its acknowledgement being queued.
type broadcastOnSubscribe struct {
	*listenSession
	srv *MCPServer
}

func (s *broadcastOnSubscribe) SetSubscriptionFilter(filter mcp.SubscriptionFilter) {
	s.listenSession.SetSubscriptionFilter(filter)
	if !filter.IsEmpty() {
		s.srv.sendNotificationToAllClients(mcp.JSONRPCNotification{
			JSONRPC: mcp.JSONRPC_VERSION,
			Method:  mcp.MethodNotificationToolsListChanged,
		})
	}
}

// The first message on a subscription is its acknowledgement; nothing for
// the subscription precedes it (Patterns.md row 15). A broadcast that lands
// while the subscription is being set up waits behind the acknowledgement.
func TestSubscriptionsListen_NothingPrecedesTheAcknowledgement(t *testing.T) {
	srv := NewMCPServer("listen-test", "1.0.0", WithToolCapabilities(true))
	session := &broadcastOnSubscribe{listenSession: newListenSession(), srv: srv}
	require.NoError(t, srv.RegisterSession(t.Context(), session))

	ctx, cancel := context.WithCancel(srv.WithContext(t.Context(), session))
	defer cancel()
	ctx = WithRequestProtocolInfo(ctx, &RequestProtocolInfo{Modern: true, ProtocolVersion: mcp.ProtocolVersion20260728})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = srv.handleSubscriptionsListen(ctx, 7, mcp.SubscriptionsListenRequest{
			Params: mcp.SubscriptionsListenParams{Notifications: mcp.SubscriptionFilter{ToolsListChanged: true}},
		})
	}()

	select {
	case first := <-session.notify:
		require.Equal(t, mcp.MethodNotificationSubscriptionsAcknowledged, first.Method,
			"a message preceded the acknowledgement")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first message")
	}
	cancel()
	<-done
}
