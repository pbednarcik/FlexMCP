package server_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// hidingSession wraps an in-process session and reports no streams.
type hidingSession struct{ *server.InProcessSession }

func (*hidingSession) SubscriptionStreams() ([]server.SubscriptionStream, bool) {
	return nil, true
}

var _ server.SessionWithSubscriptionStreams = (*hidingSession)(nil)

// A broadcast reads a session's streams through its own SubscriptionStreams,
// whatever session it embeds: a wrapper that reports none receives nothing,
// though the session it wraps holds a stream.
func TestABroadcastReadsAWrappersOwnStreams(t *testing.T) {
	srv := server.NewMCPServer("test", "1.0.0", server.WithToolCapabilities(true))
	inner := server.NewInProcessSession("wrapped")
	inner.Initialize()
	inner.MarkModern()
	inner.OpenSubscriptionStream("private", mcp.SubscriptionFilter{ToolsListChanged: true})
	require.NoError(t, srv.RegisterSession(t.Context(), &hidingSession{inner}))
	srv.SendNotificationToAllClients(mcp.MethodNotificationToolsListChanged, nil)
	assert.Empty(t, inner.ClientNotifications(), "a wrapper that reports no streams receives nothing")
}
