package server

import (
	"encoding/json"
	"sync"
	"sync/atomic"

	"github.com/mark3labs/mcp-go/mcp"
)

// streamableHttpSession is the session of one request: it lives for the
// request, in both eras, and has no ID. A broadcast reaches it only while it
// serves a subscriptions/listen stream.
type streamableHttpSession struct {
	clientInfoStore // provides Get/SetClientInfo and Get/SetClientCapabilities via method promotion

	// notifyMu guards the channel and the forwarder's start; it is never held
	// across a send or a write.
	notifyMu            sync.Mutex
	notificationChannel chan mcp.JSONRPCNotification // server -> client notifications, made on first use
	startForwarder      notificationForwarder        // armed by handlePost while it handles the request
	forwarding          bool                         // the forwarder was started
	upgradeToSSE        atomic.Bool

	// Whether the session serves a subscriptions/listen stream, and the
	// notification types that stream opted in to.
	subscriptionMu     sync.RWMutex
	listening          bool
	subscriptionFilter mcp.SubscriptionFilter

	// The request the session serves, and its read, when the transport
	// read it once.
	requestBody []byte
	request     *requestEnvelope
}

// readRequest is the transport's read of message, when message is the body
// it read; nil otherwise.
func (s *streamableHttpSession) readRequest(message json.RawMessage) *requestEnvelope {
	if s.request == nil || len(message) == 0 || len(message) != len(s.requestBody) || &message[0] != &s.requestBody[0] {
		return nil
	}
	return s.request
}

func newStreamableHttpSession() *streamableHttpSession {
	return &streamableHttpSession{}
}

// SessionID is empty: the session is the request's own, and no client holds
// an ID for it.
func (s *streamableHttpSession) SessionID() string {
	return ""
}

// NotificationChannel makes the channel on first use and, while the request
// is being handled, starts its forwarder with the first notification.
func (s *streamableHttpSession) NotificationChannel() chan<- mcp.JSONRPCNotification {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	if s.notificationChannel == nil {
		s.notificationChannel = make(chan mcp.JSONRPCNotification, 100)
	}
	s.startForwarding()
	return s.notificationChannel
}

// notificationForwarder forwards a request's notifications from the channel
// they are queued on.
type notificationForwarder interface {
	forward(<-chan mcp.JSONRPCNotification)
}

// forwardWith arms start to run once, with the channel, when the first
// notification is queued; at once when one is queued already.
func (s *streamableHttpSession) forwardWith(start notificationForwarder) {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	s.startForwarder = start
	if s.notificationChannel != nil {
		s.startForwarding()
	}
}

// startForwarding runs the armed start once. notifyMu is held.
func (s *streamableHttpSession) startForwarding() {
	if s.startForwarder != nil && !s.forwarding {
		s.forwarding = true
		s.startForwarder.forward(s.notificationChannel)
	}
}

// stopForwarding disarms the start, so a later notification waits on the
// channel for the drain, and reports whether a forwarder was started.
func (s *streamableHttpSession) stopForwarding() bool {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	s.startForwarder = nil
	return s.forwarding
}

// pending is the channel queued notifications wait on: nil when none was ever
// queued, which a select never receives from.
func (s *streamableHttpSession) pending() <-chan mcp.JSONRPCNotification {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	return s.notificationChannel
}

func (s *streamableHttpSession) Initialize() {
	// do nothing
	// the session is ephemeral, no real initialized action needed
}

func (s *streamableHttpSession) Initialized() bool {
	// the session is ephemeral, no real initialized action needed
	return true
}

var (
	_ ClientSession         = (*streamableHttpSession)(nil)
	_ SessionWithClientInfo = (*streamableHttpSession)(nil)
)

func (s *streamableHttpSession) UpgradeToSSEWhenReceiveNotification() {
	s.upgradeToSSE.Store(true)
}

var _ SessionWithStreamableHTTPConfig = (*streamableHttpSession)(nil)

func (s *streamableHttpSession) SetSubscriptionFilter(filter mcp.SubscriptionFilter) {
	s.subscriptionMu.Lock()
	defer s.subscriptionMu.Unlock()
	s.subscriptionFilter = filter
}

// SubscriptionFilter reports a session serving a subscriptions/listen stream as
// filtered for as long as it serves it, so that it receives nothing before its
// filter is recorded or after it is cleared. Other sessions are unfiltered.
func (s *streamableHttpSession) SubscriptionFilter() (mcp.SubscriptionFilter, bool) {
	s.subscriptionMu.RLock()
	defer s.subscriptionMu.RUnlock()
	return s.subscriptionFilter, s.listening
}

// serveSubscriptionStream marks the session as serving a subscriptions/listen
// stream.
func (s *streamableHttpSession) serveSubscriptionStream() {
	s.subscriptionMu.Lock()
	defer s.subscriptionMu.Unlock()
	s.listening = true
}

var _ SessionWithSubscriptionFilter = (*streamableHttpSession)(nil)
