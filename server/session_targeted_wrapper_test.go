package server_test

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
)

// streamsSession is a session of the caller's own that holds
// subscriptions/listen streams, so the server reads them through
// SubscriptionStreams, on a channel of the size it was made with.
type streamsSession struct {
	id      string
	channel chan mcp.JSONRPCNotification

	mu      sync.Mutex
	streams []server.SubscriptionStream
}

func newStreamsSession(id string, capacity int) *streamsSession {
	return &streamsSession{id: id, channel: make(chan mcp.JSONRPCNotification, capacity)}
}

func (*streamsSession) Initialize()         {}
func (*streamsSession) Initialized() bool   { return true }
func (s *streamsSession) SessionID() string { return s.id }
func (*streamsSession) MarkModern()         {}

func (s *streamsSession) NotificationChannel() chan<- mcp.JSONRPCNotification {
	return s.channel
}

func (s *streamsSession) OpenSubscriptionStream(id any, filter mcp.SubscriptionFilter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streams = append(s.streams, server.SubscriptionStream{ID: id, Filter: filter})
}

func (*streamsSession) CloseSubscriptionStream(any) {}

func (s *streamsSession) SubscriptionStreams() ([]server.SubscriptionStream, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]server.SubscriptionStream(nil), s.streams...), true
}

var _ server.SessionWithSubscriptionStreams = (*streamsSession)(nil)

// queued returns the notifications waiting on the session's channel.
func (s *streamsSession) queued() []mcp.JSONRPCNotification {
	var out []mcp.JSONRPCNotification
	for {
		select {
		case n := <-s.channel:
			out = append(out, n)
		default:
			return out
		}
	}
}

// A send to one session that holds its streams itself passes them as a
// broadcast does: a type no stream asked for is not delivered, one a stream
// asked for is delivered once, tagged with the stream's id, and a copy that
// cannot be queued is reported to the caller.
func TestSendNotificationToSpecificClientHonoursAWrappersStreams(t *testing.T) {
	newServer := func(t *testing.T, session *streamsSession) *server.MCPServer {
		t.Helper()
		srv := server.NewMCPServer("test", "1.0.0", server.WithToolCapabilities(true), server.WithPromptCapabilities(true))
		session.OpenSubscriptionStream("listen-1", mcp.SubscriptionFilter{ToolsListChanged: true})
		require.NoError(t, srv.RegisterSession(t.Context(), session))
		return srv
	}

	t.Run("a type no stream asked for is not delivered", func(t *testing.T) {
		session := newStreamsSession("wrapped", 4)
		srv := newServer(t, session)

		require.NoError(t, srv.SendNotificationToSpecificClient(session.SessionID(), mcp.MethodNotificationPromptsListChanged, nil))
		assert.Empty(t, session.queued(), "the notifications delivered")
	})

	t.Run("a type a stream asked for is delivered once, tagged", func(t *testing.T) {
		session := newStreamsSession("wrapped", 4)
		srv := newServer(t, session)

		require.NoError(t, srv.SendNotificationToSpecificClient(session.SessionID(), mcp.MethodNotificationToolsListChanged, nil))
		queued := session.queued()
		require.Len(t, queued, 1, "the notifications delivered")
		assert.Equal(t, mcp.MethodNotificationToolsListChanged, queued[0].Method)
		assert.Equal(t, "listen-1", queued[0].Params.Meta[mcp.MetaKeySubscriptionID], "the stream's tag")
	})

	t.Run("a full channel is reported", func(t *testing.T) {
		session := newStreamsSession("wrapped", 0)
		srv := newServer(t, session)

		err := srv.SendNotificationToSpecificClient(session.SessionID(), mcp.MethodNotificationToolsListChanged, nil)
		assert.ErrorIs(t, err, server.ErrNotificationChannelBlocked)
	})
}
