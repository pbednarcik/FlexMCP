package server

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
)

// subscriptionsListenKey carries the JSON-RPC ID of the subscriptions/listen
// request that opened the current notification stream.
type subscriptionsListenKey struct{}

// WithSubscriptionID returns a context tagged with the JSON-RPC ID of the
// subscriptions/listen request that opened the current stream.
func WithSubscriptionID(ctx context.Context, id any) context.Context {
	return context.WithValue(ctx, subscriptionsListenKey{}, id)
}

// SubscriptionIDFromContext returns the JSON-RPC ID of the
// subscriptions/listen request that opened the current stream, or nil when the
// request is not a subscription stream.
func SubscriptionIDFromContext(ctx context.Context) any {
	return ctx.Value(subscriptionsListenKey{})
}

// SessionWithSubscriptionFilter is implemented by sessions that can record
// which notification types a client opted in to through subscriptions/listen.
//
// Protocol version 2026-07-28 makes every server-to-client notification
// opt-in: a server MUST NOT deliver a notification type the client did not
// explicitly request (SEP-2575).
//
// Implementations must be safe for concurrent use: the filter is written by
// the goroutine serving the subscriptions/listen request and read by the
// notification fan-out running on independent goroutines.
type SessionWithSubscriptionFilter interface {
	ClientSession
	// SetSubscriptionFilter records the notification types the client opted
	// in to. Passing the zero filter clears the subscription.
	SetSubscriptionFilter(filter mcp.SubscriptionFilter)
	// SubscriptionFilter returns the notification types the client opted in
	// to, and whether a subscription is currently active.
	SubscriptionFilter() (mcp.SubscriptionFilter, bool)
}

// SubscriptionStream is one subscriptions/listen stream a session holds: the
// listen request's id, which tags every notification delivered on it, and the
// notification types it opted in to.
type SubscriptionStream struct {
	ID     any
	Filter mcp.SubscriptionFilter
}

// SessionWithSubscriptionStreams is implemented by sessions that serve every
// request of one client over one channel for the life of the client, stdio
// and in-process, and can therefore hold several subscriptions/listen streams
// at once. A broadcast reaches such a client once per open stream whose
// filter admits it, tagged with that stream's id in _meta, and never
// otherwise; a client of the handshake era, which opens no stream, receives
// broadcasts unfiltered and untagged as its revision prescribes.
//
// Implementations must be safe for concurrent use: streams are opened and
// closed by the goroutines serving subscriptions/listen and read by the
// notification fan-out.
type SessionWithSubscriptionStreams interface {
	ClientSession
	// OpenSubscriptionStream records a stream and the types it opted in to.
	OpenSubscriptionStream(id any, filter mcp.SubscriptionFilter)
	// CloseSubscriptionStream forgets a stream.
	CloseSubscriptionStream(id any)
	// SubscriptionStreams returns the open streams and whether the session
	// has served a request of protocol version 2026-07-28 or later, from
	// which point broadcasts reach the client only through its streams.
	SubscriptionStreams() (streams []SubscriptionStream, modern bool)
	// MarkModern records that the session served a modern request.
	MarkModern()
}

// subscriptionStreams implements SessionWithSubscriptionStreams for the
// sessions that embed it; its zero value is ready to use.
type subscriptionStreams struct {
	mu      sync.RWMutex
	modern  bool
	streams map[any]mcp.SubscriptionFilter
}

func (s *subscriptionStreams) OpenSubscriptionStream(id any, filter mcp.SubscriptionFilter) {
	// A JSON-RPC id is a number or a string; a malformed one that is not
	// comparable cannot key the map and opens no stream.
	if id == nil || !reflect.TypeOf(id).Comparable() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams == nil {
		s.streams = make(map[any]mcp.SubscriptionFilter)
	}
	s.streams[id] = filter
}

func (s *subscriptionStreams) CloseSubscriptionStream(id any) {
	if id == nil || !reflect.TypeOf(id).Comparable() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.streams, id)
}

func (s *subscriptionStreams) SubscriptionStreams() ([]SubscriptionStream, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.streams) == 0 {
		return nil, s.modern
	}
	streams := make([]SubscriptionStream, 0, len(s.streams))
	for id, filter := range s.streams {
		streams = append(streams, SubscriptionStream{ID: id, Filter: filter})
	}
	return streams, s.modern
}

func (s *subscriptionStreams) MarkModern() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modern = true
}

// tagSubscription tags a notification with the id of the subscriptions/listen
// stream that carries it, as every notification on a stream must be. The
// _meta map is copied, because a broadcast shares it with the copies sent to
// every other session.
func tagSubscription(notification mcp.JSONRPCNotification, id any) mcp.JSONRPCNotification {
	meta := make(map[string]any, len(notification.Params.Meta)+1)
	maps.Copy(meta, notification.Params.Meta)
	meta[mcp.MetaKeySubscriptionID] = id
	notification.Params.Meta = meta
	return notification
}

// handleSubscriptionsListen serves the subscriptions/listen RPC introduced in
// protocol version 2026-07-28 (SEP-2575).
//
// It replaces the standalone HTTP GET stream and the
// resources/subscribe and resources/unsubscribe RPCs with a single long-lived
// response stream. The server intersects the client's requested filter with
// its own capabilities, acknowledges the result, and then blocks, delivering
// notifications on the stream until the client cancels the request or
// disconnects.
func (s *MCPServer) handleSubscriptionsListen(
	ctx context.Context,
	id any,
	request mcp.SubscriptionsListenRequest,
) (*mcp.SubscriptionsListenResult, *requestError) {
	if id == nil {
		return nil, &requestError{
			id:   id,
			code: mcp.INVALID_REQUEST,
			err:  errors.New("subscriptions/listen requires a request ID"),
		}
	}

	session := ClientSessionFromContext(ctx)
	if session == nil {
		return nil, &requestError{
			id:   id,
			code: mcp.INTERNAL_ERROR,
			err:  errors.New("subscriptions/listen requires an active session"),
		}
	}

	allowed := s.allowedSubscriptions(request.Params.Notifications)

	// Record the filter so notification fan-out can honour the opt-in: a
	// request's own session holds one filter, a long-lived session one
	// stream per listen request.
	if filtered, ok := session.(SessionWithSubscriptionFilter); ok {
		filtered.SetSubscriptionFilter(allowed)
		defer filtered.SetSubscriptionFilter(mcp.SubscriptionFilter{})
	}
	if streams, ok := session.(SessionWithSubscriptionStreams); ok {
		streams.OpenSubscriptionStream(id, allowed)
		defer streams.CloseSubscriptionStream(id)
	}

	// Acknowledge the subscription so the client learns which of its requested
	// notification types were actually established. Every message on the
	// stream, including this one, is tagged with the subscription ID so the
	// client can correlate it.
	ack := mcp.JSONRPCNotification{
		JSONRPC: mcp.JSONRPC_VERSION,
		Method:  mcp.MethodNotificationSubscriptionsAcknowledged,
		Params: mcp.NotificationParams{
			Meta:             map[string]any{mcp.MetaKeySubscriptionID: id},
			AdditionalFields: map[string]any{"notifications": allowed},
		},
	}
	if err := s.sendNotificationToSpecificClient(session, ack); err != nil {
		return nil, &requestError{
			id:   id,
			code: mcp.INTERNAL_ERROR,
			err:  fmt.Errorf("sending subscriptions/acknowledged: %w", err),
		}
	}

	// Hold the stream open for as long as anything was subscribed. When the
	// filter is empty there is nothing to deliver, so close immediately.
	if !allowed.IsEmpty() {
		<-ctx.Done()
	}

	result := &mcp.SubscriptionsListenResult{}
	meta := result.EnsureResultMeta()
	meta.SetSubscriptionID(id)
	return result, nil
}

// registerListenSession makes a session serving a subscriptions/listen stream
// reachable by the notifications the server broadcasts. It is for transports
// that serve each request with an ephemeral session they never register, such
// as Streamable HTTP. It fires no session hooks, because the
// session is not a client session in its own right.
func (s *MCPServer) registerListenSession(session ClientSession) {
	s.listenSessions.Store(session, struct{}{})
}

// unregisterListenSession undoes registerListenSession.
func (s *MCPServer) unregisterListenSession(session ClientSession) {
	s.listenSessions.Delete(session)
}

// allowedSubscriptions intersects the notification types a client asked for
// with the capabilities this server actually advertises. The server MUST NOT
// establish a subscription it cannot serve: resourceSubscriptions are never
// granted, because nothing emits notifications/resources/updated yet.
func (s *MCPServer) allowedSubscriptions(want mcp.SubscriptionFilter) mcp.SubscriptionFilter {
	s.capabilitiesMu.RLock()
	defer s.capabilitiesMu.RUnlock()

	var allowed mcp.SubscriptionFilter
	if want.ToolsListChanged && s.capabilities.tools != nil && s.capabilities.tools.listChanged {
		allowed.ToolsListChanged = true
	}
	if want.PromptsListChanged && s.capabilities.prompts != nil && s.capabilities.prompts.listChanged {
		allowed.PromptsListChanged = true
	}
	if want.ResourcesListChanged && s.capabilities.resources != nil && s.capabilities.resources.listChanged {
		allowed.ResourcesListChanged = true
	}
	return allowed
}

// subscriptionAllowsNotification reports whether a session that opted in
// through subscriptions/listen should receive the given notification method.
//
// A session not serving a subscription stream is unaffected: a request's own
// session in either era, or a handshake-era stdio or in-process session,
// where notifications are not opt-in.
func subscriptionAllowsNotification(session ClientSession, method string) bool {
	filtered, ok := session.(SessionWithSubscriptionFilter)
	if !ok {
		return true
	}
	filter, active := filtered.SubscriptionFilter()
	if !active {
		return true
	}
	return filterAllows(filter, method)
}

// filterAllows reports whether a subscriptions/listen filter admits the given
// notification method.
func filterAllows(filter mcp.SubscriptionFilter, method string) bool {
	switch method {
	case mcp.MethodNotificationToolsListChanged:
		return filter.ToolsListChanged
	case mcp.MethodNotificationPromptsListChanged:
		return filter.PromptsListChanged
	case mcp.MethodNotificationResourcesListChanged:
		return filter.ResourcesListChanged
	default:
		// Opt-in is absolute: a server MUST NOT deliver a notification type
		// the client did not request. Anything outside the filter's
		// vocabulary - a custom broadcast, or an extension's notifications -
		// therefore stays off the subscription stream. An extension that
		// defines its own notifications negotiates them through its own
		// capability and needs its own delivery path.
		//
		// notifications/resources/updated is unaffected: it is fanned out per
		// subscribed URI through SessionWithResourceSubscriptions rather than
		// broadcast.
		return false
	}
}
