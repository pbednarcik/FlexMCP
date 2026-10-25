package server

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"

	"github.com/pbednarcik/flexmcp/mcp"
)

// ClientSession represents an active session that can be used by MCPServer to interact with client.
type ClientSession interface {
	// Initialize marks session as fully initialized and ready for notifications
	Initialize()
	// Initialized returns if session is ready to accept notifications
	Initialized() bool
	// NotificationChannel provides a channel suitable for sending notifications to client.
	NotificationChannel() chan<- mcp.JSONRPCNotification
	// SessionID is a unique identifier used to track user session.
	SessionID() string
}

// SessionWithResourceSubscriptions is an optional extension of ClientSession
// implemented by sessions that track resource subscriptions, the store a
// notifications/resources/updated fan-out would target. Nothing drives it
// yet: the server emits no resource updates and grants no
// resourceSubscriptions on subscriptions/listen until it does.
//
// Implementations must be safe for concurrent use because requests and
// notifications may run on independent goroutines.
type SessionWithResourceSubscriptions interface {
	ClientSession
	// SubscribeToResource records that this session has subscribed to updates
	// for the given resource URI. Subscribing the same URI twice is a no-op.
	SubscribeToResource(uri string)
	// UnsubscribeFromResource removes a previously recorded subscription for
	// the given resource URI. Unsubscribing a URI that was never subscribed
	// to is a no-op.
	UnsubscribeFromResource(uri string)
	// SubscribedResources returns a snapshot of the URIs this session is
	// currently subscribed to. The returned slice is owned by the caller and
	// safe to mutate.
	SubscribedResources() []string
	// IsSubscribedToResource reports whether the session is currently
	// subscribed to updates for the given resource URI.
	IsSubscribedToResource(uri string) bool
}

// SessionWithResourceSubscriptionsErr is an optional extension of
// SessionWithResourceSubscriptions whose SubscribeToResourceErr method can
// reject a subscription. When a session implements this interface, MCPServer
// uses SubscribeToResourceErr in preference to SubscribeToResource, and
// subscriptions/listen fails with INVALID_PARAMS if it reports an error.
//
// Implementations must be safe for concurrent use because requests and
// notifications may run on independent goroutines.
type SessionWithResourceSubscriptionsErr interface {
	SessionWithResourceSubscriptions
	// SubscribeToResourceErr records that this session has subscribed to updates
	// for the given resource URI, or returns an error if the URI cannot be
	// subscribed to.
	SubscribeToResourceErr(uri string) error
}

// SessionWithLogging is an optional extension of ClientSession for a session
// that keeps the log level a handshake-era client set with logging/setLevel
// (2025-11-25 Utilities.md row 14). A session without it is served at
// legacyLogLevel.
type SessionWithLogging interface {
	ClientSession
	// SetLogLevel keeps the level the client asked for.
	SetLogLevel(level mcp.LoggingLevel)
	// GetLogLevel is the level the client asked for, or "" for none yet.
	GetLogLevel() mcp.LoggingLevel
}

// logLevelStore keeps a session's log level; the sessions that serve a
// handshake-era client over time embed it.
type logLevelStore struct {
	level atomic.Value // mcp.LoggingLevel
}

// SetLogLevel keeps the level the client asked for.
func (s *logLevelStore) SetLogLevel(level mcp.LoggingLevel) { s.level.Store(level) }

// GetLogLevel is the level the client asked for, or "" for none yet.
func (s *logLevelStore) GetLogLevel() mcp.LoggingLevel {
	level, _ := s.level.Load().(mcp.LoggingLevel)
	return level
}

// SessionWithClientInfo is an extension of ClientSession that can store client info
type SessionWithClientInfo interface {
	ClientSession
	// GetClientInfo returns the client information for this session
	GetClientInfo() mcp.Implementation
	// SetClientInfo sets the client information for this session
	SetClientInfo(clientInfo mcp.Implementation)
	// GetClientCapabilities returns the client capabilities for this session
	GetClientCapabilities() mcp.ClientCapabilities
	// SetClientCapabilities sets the client capabilities for this session
	SetClientCapabilities(clientCapabilities mcp.ClientCapabilities)
}

// SessionWithStreamableHTTPConfig extends ClientSession to support streamable HTTP transport configurations
type SessionWithStreamableHTTPConfig interface {
	ClientSession
	// UpgradeToSSEWhenReceiveNotification upgrades the client-server communication to SSE stream when the server
	// sends notifications to the client
	//
	// The protocol specification:
	// - If the server response contains any JSON-RPC notifications, it MUST either:
	//   - Return Content-Type: text/event-stream to initiate an SSE stream, OR
	//   - Return Content-Type: application/json for a single JSON object
	// - The client MUST support both response types.
	//
	// Reference: https://modelcontextprotocol.io/specification/2025-03-26/basic/transports#sending-messages-to-the-server
	UpgradeToSSEWhenReceiveNotification()
}

// clientSessionKey is the context key for storing current client notification channel.
type clientSessionKey struct{}

// ClientSessionFromContext retrieves current client notification context from context.
func ClientSessionFromContext(ctx context.Context) ClientSession {
	if session, ok := ctx.Value(clientSessionKey{}).(ClientSession); ok {
		return session
	}
	return nil
}

// WithContext sets the current client session and returns the provided context
func (s *MCPServer) WithContext(
	ctx context.Context,
	session ClientSession,
) context.Context {
	return context.WithValue(ctx, clientSessionKey{}, session)
}

// RegisterSession saves session that should be notified in case if some server attributes changed.
func (s *MCPServer) RegisterSession(
	ctx context.Context,
	session ClientSession,
) error {
	sessionID := session.SessionID()
	if _, exists := s.sessions.LoadOrStore(sessionID, session); exists {
		return ErrSessionExists
	}
	s.hooks.RegisterSession(ctx, session)
	return nil
}

func (s *MCPServer) buildLogNotification(notification mcp.LoggingMessageNotification) mcp.JSONRPCNotification {
	return mcp.JSONRPCNotification{
		JSONRPC: mcp.JSONRPC_VERSION,
		Method:  notification.Method,
		Params: mcp.NotificationParams{
			AdditionalFields: map[string]any{
				"level":  notification.Params.Level,
				"logger": notification.Params.Logger,
				"data":   notification.Params.Data,
			},
		},
	}
}

// legacyLogLevel is the log threshold of a handshake-era client that set
// none with logging/setLevel: error and worse.
const legacyLogLevel = mcp.LoggingLevelError

// legacyThreshold is a handshake-era session's log threshold: the level it
// set with logging/setLevel, when the session keeps one, else legacyLogLevel.
func legacyThreshold(session ClientSession) mcp.LoggingLevel {
	if s, ok := session.(SessionWithLogging); ok {
		if level := s.GetLogLevel(); level != "" {
			return level
		}
	}
	return legacyLogLevel
}

// SendLogMessageToClient sends a log message to the client of the request in
// ctx if its level reaches the request's threshold: the log level a modern
// request declares in _meta, or for a legacy one the level its session
// keeps, else legacyLogLevel.
func (s *MCPServer) SendLogMessageToClient(ctx context.Context, notification mcp.LoggingMessageNotification) error {
	session := ClientSessionFromContext(ctx)
	if session == nil || !session.Initialized() {
		return ErrNotificationNotInitialized
	}
	threshold := legacyThreshold(session)
	// A modern request that declares no level gets no log notifications (SEP-2575).
	if info := RequestProtocolInfoFromContext(ctx); info != nil && info.Modern {
		if info.LogLevel == "" {
			return nil
		}
		threshold = info.LogLevel
	}
	if !notification.Params.Level.ShouldSendTo(threshold) {
		return nil
	}
	return s.sendNotificationCore(ctx, session, s.buildLogNotification(notification))
}

func (s *MCPServer) sendNotificationToAllClients(notification mcp.JSONRPCNotification) {
	// A list change counts itself started before it reads the sessions and
	// finished once it is done with them; see listChangeCounts.
	if counts := s.listChanges.of(notification.Method); counts != nil {
		counts.started.Add(1)
		defer counts.finished.Add(1)
	}
	if s.testHookBroadcast != nil {
		s.testHookBroadcast("started")
		defer s.testHookBroadcast("delivered")
	}
	s.sessions.Range(func(k, v any) bool {
		if session, ok := v.(ClientSession); ok && session.Initialized() {
			s.broadcastToSession(session, notification)
		}
		return true
	})
	s.listenSessions.Range(func(k, _ any) bool {
		s.broadcastToSession(k.(ClientSession), notification)
		return true
	})
}

func (s *MCPServer) broadcastToSession(session ClientSession, notification mcp.JSONRPCNotification) {
	// From protocol version 2026-07-28 every server-to-client notification is
	// opt-in (SEP-2575). A session that holds subscriptions/listen streams on
	// one long-lived channel receives a broadcast once per stream that asked
	// for it, tagged with the stream's id, and not at all without one;
	// a handshake-era client on such a session is served below, unfiltered.
	if streams, shared, ok := ownStreams(session); ok {
		open, modern := streams.streamsToBroadcast()
		if modern {
			for _, stream := range open {
				if filterAllows(stream.filter, notification.Method) {
					s.deliverBroadcast(session, stream.tagged(notification, shared))
				}
			}
			return
		}
	} else if streams, ok := session.(SessionWithSubscriptionStreams); ok {
		open, modern := streams.SubscriptionStreams()
		if modern {
			for _, stream := range open {
				if filterAllows(stream.Filter, notification.Method) {
					s.deliverBroadcast(session, tagSubscription(notification, stream.ID))
				}
			}
			return
		}
	}
	// A request's own session that opened a stream receives only the types
	// it asked for. Sessions that never opened one are unaffected.
	if !subscriptionAllowsNotification(session, notification.Method) {
		return
	}
	s.deliverBroadcast(session, notification)
}

// deliverBroadcast queues a broadcast notification on a session, reporting a
// full channel through the error hook.
func (s *MCPServer) deliverBroadcast(session ClientSession, notification mcp.JSONRPCNotification) {
	if sessionWithStreamableHTTPConfig, ok := session.(SessionWithStreamableHTTPConfig); ok {
		sessionWithStreamableHTTPConfig.UpgradeToSSEWhenReceiveNotification()
	}
	select {
	case session.NotificationChannel() <- notification:
		// Successfully sent notification
	default:
		// Channel is blocked, if there's an error hook, use it
		if s.hooks != nil && len(s.hooks.OnError) > 0 {
			err := ErrNotificationChannelBlocked
			// Copy hooks pointer to local variable to avoid race condition
			hooks := s.hooks
			go func(sessionID string, hooks *Hooks) {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("flexmcp: panic in OnError hook (notification blocked, session %s): %v", sessionID, r)
					}
				}()
				ctx := context.Background()
				// Use the error hook to report the blocked channel
				hooks.onError(ctx, nil, "notification", map[string]any{
					"method":    notification.Method,
					"sessionID": sessionID,
				}, fmt.Errorf("notification channel blocked for session %s: %w", sessionID, err))
			}(session.SessionID(), hooks)
		}
	}
}

func (s *MCPServer) sendNotificationToSpecificClient(session ClientSession, notification mcp.JSONRPCNotification) error {
	// upgrades the client-server communication to SSE stream when the server sends notifications to the client
	if sessionWithStreamableHTTPConfig, ok := session.(SessionWithStreamableHTTPConfig); ok {
		sessionWithStreamableHTTPConfig.UpgradeToSSEWhenReceiveNotification()
	}
	select {
	case session.NotificationChannel() <- notification:
		return nil
	default:
		// Channel is blocked, if there's an error hook, use it
		if s.hooks != nil && len(s.hooks.OnError) > 0 {
			err := ErrNotificationChannelBlocked
			ctx := context.Background()
			// Copy hooks pointer to local variable to avoid race condition
			hooks := s.hooks
			go func(sID string, hooks *Hooks) {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("flexmcp: panic in OnError hook (notification blocked, session %s): %v", sID, r)
					}
				}()
				// Use the error hook to report the blocked channel
				hooks.onError(ctx, nil, "notification", map[string]any{
					"method":    notification.Method,
					"sessionID": sID,
				}, fmt.Errorf("notification channel blocked for session %s: %w", sID, err))
			}(session.SessionID(), hooks)
		}
		return ErrNotificationChannelBlocked
	}
}

func (s *MCPServer) SendLogMessageToSpecificClient(sessionID string, notification mcp.LoggingMessageNotification) error {
	sessionValue, ok := s.sessions.Load(sessionID)
	if !ok {
		return ErrSessionNotFound
	}
	session, ok := sessionValue.(ClientSession)
	if !ok || !session.Initialized() {
		return ErrSessionNotInitialized
	}
	if !notification.Params.Level.ShouldSendTo(legacyThreshold(session)) {
		return nil
	}
	return s.sendNotificationToSpecificClient(session, s.buildLogNotification(notification))
}

// UnregisterSession removes from storage session that is shut down.
func (s *MCPServer) UnregisterSession(
	ctx context.Context,
	sessionID string,
) {
	sessionValue, ok := s.sessions.LoadAndDelete(sessionID)
	if !ok {
		return
	}
	if session, ok := sessionValue.(ClientSession); ok {
		s.hooks.UnregisterSession(ctx, session)
	}
}

// SendNotificationToAllClients sends a notification to all the currently active clients.
func (s *MCPServer) SendNotificationToAllClients(
	method string,
	params map[string]any,
) {
	notification := mcp.JSONRPCNotification{
		JSONRPC: mcp.JSONRPC_VERSION,
		Method:  method,
		Params: mcp.NotificationParams{
			AdditionalFields: params,
		},
	}
	s.sendNotificationToAllClients(notification)
}

// SendNotificationToClient sends a notification to the current client
func (s *MCPServer) sendNotificationCore(
	ctx context.Context,
	session ClientSession,
	notification mcp.JSONRPCNotification,
) error {
	// upgrades the client-server communication to SSE stream when the server sends notifications to the client
	if sessionWithStreamableHTTPConfig, ok := session.(SessionWithStreamableHTTPConfig); ok {
		sessionWithStreamableHTTPConfig.UpgradeToSSEWhenReceiveNotification()
	}
	select {
	case session.NotificationChannel() <- notification:
		return nil
	default:
		// Channel is blocked, if there's an error hook, use it
		if s.hooks != nil && len(s.hooks.OnError) > 0 {
			method := notification.Method
			err := ErrNotificationChannelBlocked
			// Copy hooks pointer to local variable to avoid race condition
			hooks := s.hooks
			go func(sessionID string, hooks *Hooks) {
				defer func() {
					if r := recover(); r != nil {
						log.Printf("flexmcp: panic in OnError hook (notification blocked, session %s): %v", sessionID, r)
					}
				}()
				// Use the error hook to report the blocked channel
				hooks.onError(ctx, nil, "notification", map[string]any{
					"method":    method,
					"sessionID": sessionID,
				}, fmt.Errorf("notification channel blocked for session %s: %w", sessionID, err))
			}(session.SessionID(), hooks)
		}
		return ErrNotificationChannelBlocked
	}
}

// SendNotificationToClient sends a notification to the current client
func (s *MCPServer) SendNotificationToClient(
	ctx context.Context,
	method string,
	params map[string]any,
) error {
	session := ClientSessionFromContext(ctx)
	if session == nil || !session.Initialized() {
		return ErrNotificationNotInitialized
	}
	notification := mcp.JSONRPCNotification{
		JSONRPC: mcp.JSONRPC_VERSION,
		Method:  method,
		Params: mcp.NotificationParams{
			AdditionalFields: params,
		},
	}
	return s.sendNotificationCore(ctx, session, notification)
}

// SendNotificationToSpecificClient sends a notification to a specific client by session ID
func (s *MCPServer) SendNotificationToSpecificClient(
	sessionID string,
	method string,
	params map[string]any,
) error {
	sessionValue, ok := s.sessions.Load(sessionID)
	if !ok {
		return ErrSessionNotFound
	}
	session, ok := sessionValue.(ClientSession)
	if !ok || !session.Initialized() {
		return ErrSessionNotInitialized
	}
	notification := mcp.JSONRPCNotification{
		JSONRPC: mcp.JSONRPC_VERSION,
		Method:  method,
		Params: mcp.NotificationParams{
			AdditionalFields: params,
		},
	}
	return s.sendNotificationToSpecificClient(session, notification)
}
