package server

import (
	"context"
	"fmt"
	"log"
	"maps"
	"net/url"

	"github.com/mark3labs/mcp-go/mcp"
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

// SessionWithTools is an extension of ClientSession that can store session-specific tool data
type SessionWithTools interface {
	ClientSession
	// GetSessionTools returns the tools specific to this session, if any
	// This method must be thread-safe for concurrent access
	GetSessionTools() map[string]ServerTool
	// SetSessionTools sets tools specific to this session
	// This method must be thread-safe for concurrent access
	SetSessionTools(tools map[string]ServerTool)
}

// SessionWithResources is an extension of ClientSession that can store session-specific resource data
type SessionWithResources interface {
	ClientSession
	// GetSessionResources returns the resources specific to this session, if any
	// This method must be thread-safe for concurrent access
	GetSessionResources() map[string]ServerResource
	// SetSessionResources sets resources specific to this session
	// This method must be thread-safe for concurrent access
	SetSessionResources(resources map[string]ServerResource)
}

// SessionWithResourceSubscriptions is an optional extension of ClientSession
// implemented by sessions that track resource subscriptions. While a
// subscriptions/listen stream asks for resourceSubscriptions, the server calls
// SubscribeToResource for each URI and UnsubscribeFromResource when the stream
// ends, so notifications/resources/updated can be targeted at the sessions
// that asked for updates.
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

// SessionWithResourceTemplates is an extension of ClientSession that can store session-specific resource template data
type SessionWithResourceTemplates interface {
	ClientSession
	// GetSessionResourceTemplates returns the resource templates specific to this session, if any
	// This method must be thread-safe for concurrent access
	GetSessionResourceTemplates() map[string]ServerResourceTemplate
	// SetSessionResourceTemplates sets resource templates specific to this session
	// This method must be thread-safe for concurrent access
	SetSessionResourceTemplates(templates map[string]ServerResourceTemplate)
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

// legacyLogLevel is the log threshold of a handshake-era client: error and
// worse. Its logging/setLevel is acknowledged but not kept, since the server
// holds no per-client state.
const legacyLogLevel = mcp.LoggingLevelError

// SendLogMessageToClient sends a log message to the client of the request in
// ctx if its level reaches the request's threshold: the log level a modern
// request declares in _meta, or legacyLogLevel for a legacy one.
func (s *MCPServer) SendLogMessageToClient(ctx context.Context, notification mcp.LoggingMessageNotification) error {
	session := ClientSessionFromContext(ctx)
	if session == nil || !session.Initialized() {
		return ErrNotificationNotInitialized
	}
	threshold := legacyLogLevel
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
	// opt-in: a session that opened a subscriptions/listen stream receives
	// only the types it asked for (SEP-2575). Sessions that never opened one
	// are unaffected.
	if !subscriptionAllowsNotification(session, notification.Method) {
		return
	}
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
						log.Printf("mcp-go: panic in OnError hook (notification blocked, session %s): %v", sessionID, r)
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
						log.Printf("mcp-go: panic in OnError hook (notification blocked, session %s): %v", sID, r)
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
	if !notification.Params.Level.ShouldSendTo(legacyLogLevel) {
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
						log.Printf("mcp-go: panic in OnError hook (notification blocked, session %s): %v", sessionID, r)
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

// sessionCatalog is one of the per-session catalogues (tools, resources,
// resource templates): how a session exposes it, and what a change to it
// owes the client.
type sessionCatalog[V any] struct {
	// open returns the catalogue's accessors on a session, or false when the
	// session does not support it.
	open        func(ClientSession) (get func() map[string]V, set func(map[string]V), ok bool)
	unsupported error
	// register turns the capability on when a first entry is added.
	register func(*MCPServer)
	// listChanged reports whether the capability announces changes.
	listChanged  func(*MCPServer) bool
	notification string
	what         string // "tools", "resources", "resource templates" in the hook's error text
}

var (
	sessionToolCatalog = sessionCatalog[ServerTool]{
		open: func(session ClientSession) (func() map[string]ServerTool, func(map[string]ServerTool), bool) {
			st, ok := session.(SessionWithTools)
			if !ok {
				return nil, nil, false
			}
			return st.GetSessionTools, st.SetSessionTools, true
		},
		unsupported: ErrSessionDoesNotSupportTools,
		register:    (*MCPServer).implicitlyRegisterToolCapabilities,
		listChanged: func(s *MCPServer) bool {
			return s.capabilities.tools != nil && s.capabilities.tools.listChanged
		},
		notification: "notifications/tools/list_changed",
		what:         "tools",
	}

	sessionResourceCatalog = sessionCatalog[ServerResource]{
		open: func(session ClientSession) (func() map[string]ServerResource, func(map[string]ServerResource), bool) {
			sr, ok := session.(SessionWithResources)
			if !ok {
				return nil, nil, false
			}
			return sr.GetSessionResources, sr.SetSessionResources, true
		},
		unsupported:  ErrSessionDoesNotSupportResources,
		register:     registerSessionResourceCapabilities,
		listChanged:  resourcesListChanged,
		notification: "notifications/resources/list_changed",
		what:         "resources",
	}

	sessionResourceTemplateCatalog = sessionCatalog[ServerResourceTemplate]{
		open: func(session ClientSession) (func() map[string]ServerResourceTemplate, func(map[string]ServerResourceTemplate), bool) {
			st, ok := session.(SessionWithResourceTemplates)
			if !ok {
				return nil, nil, false
			}
			return st.GetSessionResourceTemplates, st.SetSessionResourceTemplates, true
		},
		unsupported:  ErrSessionDoesNotSupportResourceTemplates,
		register:     registerSessionResourceCapabilities,
		listChanged:  resourcesListChanged,
		notification: "notifications/resources/list_changed",
		what:         "resource templates",
	}
)

// registerSessionResourceCapabilities turns resources on with listChanged,
// unlike the server-wide registration, which leaves it off.
func registerSessionResourceCapabilities(s *MCPServer) {
	s.implicitlyRegisterCapabilities(
		func() bool { return s.capabilities.resources != nil },
		func() { s.capabilities.resources = &resourceCapabilities{listChanged: true} },
	)
}

func resourcesListChanged(s *MCPServer) bool {
	return s.capabilities.resources != nil && s.capabilities.resources.listChanged
}

// openSession finds the session and the catalogue on it.
func (c sessionCatalog[V]) openSession(s *MCPServer, sessionID string) (ClientSession, func() map[string]V, func(map[string]V), error) {
	value, ok := s.sessions.Load(sessionID)
	if !ok {
		return nil, nil, nil, ErrSessionNotFound
	}
	session, ok := value.(ClientSession)
	if !ok {
		return nil, nil, nil, c.unsupported
	}
	get, set, ok := c.open(session)
	if !ok {
		return nil, nil, nil, c.unsupported
	}
	return session, get, set, nil
}

// addSessionEntries extends a session's catalogue: add fills a copy of the
// current map with n new entries, the copy is stored back, and an
// initialized session is told the list changed. An error from add leaves
// the session untouched.
func addSessionEntries[V any](s *MCPServer, sessionID string, c sessionCatalog[V], n int, add func(map[string]V) error) error {
	session, get, set, err := c.openSession(s, sessionID)
	if err != nil {
		return err
	}
	c.register(s)
	current := get()
	next := make(map[string]V, len(current)+n)
	maps.Copy(next, current)
	if err := add(next); err != nil {
		return err
	}
	set(next)
	notifySessionListChanged(s, sessionID, session, c, "adding", "added")
	return nil
}

// deleteSessionEntries removes keys from a session's catalogue. Nothing is
// written and nobody is told when none of the keys was there.
func deleteSessionEntries[V any](s *MCPServer, sessionID string, c sessionCatalog[V], keys []string) error {
	session, get, set, err := c.openSession(s, sessionID)
	if err != nil {
		return err
	}
	current := get()
	next := make(map[string]V, len(current))
	maps.Copy(next, current)
	deleted := false
	for _, key := range keys {
		if _, ok := next[key]; ok {
			delete(next, key)
			deleted = true
		}
	}
	if !deleted {
		return nil
	}
	set(next)
	notifySessionListChanged(s, sessionID, session, c, "deleting", "deleted")
	return nil
}

// notifySessionListChanged tells an initialized session its catalogue
// changed, when the capability announces changes. A failed send goes to the
// OnError hooks and never fails the change that already landed.
func notifySessionListChanged[V any](s *MCPServer, sessionID string, session ClientSession, c sessionCatalog[V], verb, done string) {
	if !session.Initialized() || !c.listChanged(s) {
		return
	}
	err := s.SendNotificationToSpecificClient(sessionID, c.notification, nil)
	if err == nil || s.hooks == nil || len(s.hooks.OnError) == 0 {
		return
	}
	hooks := s.hooks
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("mcp-go: panic in OnError hook (%s %s, session %s): %v", c.what, done, sessionID, r)
			}
		}()
		hooks.onError(context.Background(), nil, "notification", map[string]any{
			"method":    c.notification,
			"sessionID": sessionID,
		}, fmt.Errorf("failed to send notification after %s %s: %w", verb, c.what, err))
	}()
}

// AddSessionTool adds a tool for a specific session
func (s *MCPServer) AddSessionTool(sessionID string, tool mcp.Tool, handler ToolHandlerFunc) error {
	return s.AddSessionTools(sessionID, ServerTool{Tool: tool, Handler: handler})
}

// AddSessionTools adds tools for a specific session
func (s *MCPServer) AddSessionTools(sessionID string, tools ...ServerTool) error {
	return addSessionEntries(s, sessionID, sessionToolCatalog, len(tools), func(next map[string]ServerTool) error {
		for _, tool := range tools {
			s.applyStrictInputSchemaDefault(&tool.Tool)
			if err := validateToolHeaderAnnotations(&tool.Tool); err != nil {
				return err
			}
			next[tool.Tool.Name] = tool
		}
		return nil
	})
}

// DeleteSessionTools removes tools from a specific session
func (s *MCPServer) DeleteSessionTools(sessionID string, names ...string) error {
	return deleteSessionEntries(s, sessionID, sessionToolCatalog, names)
}

// AddSessionResource adds a resource for a specific session
func (s *MCPServer) AddSessionResource(sessionID string, resource mcp.Resource, handler ResourceHandlerFunc) error {
	return s.AddSessionResources(sessionID, ServerResource{Resource: resource, Handler: handler})
}

// AddSessionResources adds resources for a specific session
func (s *MCPServer) AddSessionResources(sessionID string, resources ...ServerResource) error {
	return addSessionEntries(s, sessionID, sessionResourceCatalog, len(resources), func(next map[string]ServerResource) error {
		for _, resource := range resources {
			if resource.Resource.URI == "" {
				return fmt.Errorf("resource URI cannot be empty")
			}
			if _, err := url.ParseRequestURI(resource.Resource.URI); err != nil {
				return fmt.Errorf("invalid resource URI: %w", err)
			}
			next[resource.Resource.URI] = resource
		}
		return nil
	})
}

// DeleteSessionResources removes resources from a specific session
func (s *MCPServer) DeleteSessionResources(sessionID string, uris ...string) error {
	return deleteSessionEntries(s, sessionID, sessionResourceCatalog, uris)
}

// AddSessionResourceTemplate adds a resource template for a specific session
func (s *MCPServer) AddSessionResourceTemplate(sessionID string, template mcp.ResourceTemplate, handler ResourceTemplateHandlerFunc) error {
	return s.AddSessionResourceTemplates(sessionID, ServerResourceTemplate{
		Template: template,
		Handler:  handler,
	})
}

// AddSessionResourceTemplates adds resource templates for a specific session
func (s *MCPServer) AddSessionResourceTemplates(sessionID string, templates ...ServerResourceTemplate) error {
	return addSessionEntries(s, sessionID, sessionResourceTemplateCatalog, len(templates), func(next map[string]ServerResourceTemplate) error {
		for _, t := range templates {
			if t.Template.URITemplate == nil {
				return fmt.Errorf("resource template URITemplate cannot be nil")
			}
			raw := t.Template.URITemplate.Raw()
			if raw == "" {
				return fmt.Errorf("resource template URITemplate cannot be empty")
			}
			if t.Template.Name == "" {
				return fmt.Errorf("resource template name cannot be empty")
			}
			next[raw] = t
		}
		return nil
	})
}

// DeleteSessionResourceTemplates removes resource templates from a specific session
func (s *MCPServer) DeleteSessionResourceTemplates(sessionID string, uriTemplates ...string) error {
	return deleteSessionEntries(s, sessionID, sessionResourceTemplateCatalog, uriTemplates)
}
