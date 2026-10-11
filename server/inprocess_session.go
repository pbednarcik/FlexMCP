package server

import (
	"fmt"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// InProcessSession is the session of an in-process client: notifications
// queue on a channel the transport drains; nothing crosses a wire. Every
// request and subscriptions/listen stream of the client shares it, so it
// holds the client's streams and tags what it delivers on them.
type InProcessSession struct {
	clientInfoStore     // provides Get/SetClientInfo and Get/SetClientCapabilities via method promotion
	subscriptionStreams // the client's subscriptions/listen streams

	sessionID     string
	notifications chan mcp.JSONRPCNotification
	initialized   atomic.Bool
}

// NewInProcessSession creates a session for an in-process client.
func NewInProcessSession(sessionID string) *InProcessSession {
	return &InProcessSession{
		sessionID:     sessionID,
		notifications: make(chan mcp.JSONRPCNotification, 100),
	}
}

func (s *InProcessSession) SessionID() string {
	return s.sessionID
}

func (s *InProcessSession) NotificationChannel() chan<- mcp.JSONRPCNotification {
	return s.notifications
}

// ClientNotifications returns the receive-only endpoint of the session's
// notification channel. In-process transports use this to drain
// server-to-client notifications (progress, list-changed, resource
// updates, etc.) queued via NotificationChannel and forward them to the
// client's registered notification handler.
func (s *InProcessSession) ClientNotifications() <-chan mcp.JSONRPCNotification {
	return s.notifications
}

func (s *InProcessSession) Initialize() {
	s.initialized.Store(true)
}

func (s *InProcessSession) Initialized() bool {
	return s.initialized.Load()
}

// GenerateInProcessSessionID generates a unique session ID for inprocess clients
func GenerateInProcessSessionID() string {
	return fmt.Sprintf("inprocess-%d", time.Now().UnixNano())
}

// Ensure interface compliance
var (
	_ ClientSession                  = (*InProcessSession)(nil)
	_ SessionWithClientInfo          = (*InProcessSession)(nil)
	_ SessionWithSubscriptionStreams = (*InProcessSession)(nil)
)
