package client

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/tracing"
)

// Client implements the MCP client.
type Client struct {
	transport transport.Interface

	initialized atomic.Bool
	// notifications is the handlers in the order registered. A dispatch reads
	// the list without a lock; OnNotification, under notifyMu, publishes a
	// new list and never changes one published.
	notifications atomic.Pointer[[]func(mcp.JSONRPCNotification)]
	notifyMu      sync.Mutex
	requestID     atomic.Int64

	// era is the connection's negotiated state: the protocol version, the
	// client's identity and capabilities, the server's capabilities. Every
	// request reads it as one snapshot; Initialize swaps it once.
	era atomic.Pointer[era]

	samplingHandler    SamplingHandler
	rootsHandler       RootsHandler
	elicitationHandler ElicitationHandler
	invalidToolHandler InvalidToolHandler
	tracer             tracing.Tracer
	propagator         tracing.Propagator
	metaPropagator     tracing.MetaPropagator
	// metaInjects is set when a real meta propagator is installed: only then
	// is a request's Meta copied for injection.
	metaInjects bool

	// preferredVersion pins the protocol version to negotiate. Empty means
	// "prefer the newest this SDK implements".
	preferredVersion string

	// legacyOnly disables the server/discover probe, keeping the client on the
	// initialize handshake.
	legacyOnly bool

	// logLevel is the per-request log level sent in _meta, replacing the
	// logging/setLevel RPC removed in 2026-07-28. It is guarded by logLevelMu
	// because SetLevel may be called while other goroutines send requests.
	logLevel   mcp.LoggingLevel
	logLevelMu sync.RWMutex

	// knownTools caches tool definitions so that tools/call requests can
	// mirror x-mcp-header annotated parameters into HTTP headers (SEP-2243).
	knownTools map[string]mcp.Tool
	toolsMu    sync.RWMutex

	// maxInputRoundTrips bounds the multi round-trip retry loop.
	maxInputRoundTrips int

	// discoverTimeout bounds the server/discover probe sent during Initialize.
	discoverTimeout time.Duration

	// subscriptions tracks the subscriptions/listen filter and stream.
	subscriptions subscriptionState
}

// ClientOption configures a Client during construction.
type ClientOption func(*Client)

// WithClientCapabilities sets the client capabilities for the client.
func WithClientCapabilities(capabilities mcp.ClientCapabilities) ClientOption {
	return func(c *Client) {
		c.updateEra(func(e *era) { e.clientCapabilities = capabilities })
	}
}

// WithSession assumes a MCP Session has already been initialized
func WithSession() ClientOption {
	return func(c *Client) {
		c.initialized.Store(true)
	}
}

// WithProtocolVersion pins the protocol version the client negotiates.
//
// By default the client prefers the newest version this SDK implements and
// negotiates down when the server asks it to. Pinning a version earlier than
// 2026-07-28 keeps the client on the initialize handshake.
func WithProtocolVersion(version string) ClientOption {
	return func(c *Client) {
		c.preferredVersion = version
		if version != "" && !mcp.IsModernProtocol(version) {
			c.legacyOnly = true
		}
	}
}

// WithLegacyProtocolOnly keeps the client on the initialize handshake,
// skipping the server/discover probe.
//
// Deprecated: the initialize handshake was removed in protocol version
// 2026-07-28. This option exists for deployments that depend on protocol-level
// session state and will be removed once the deprecation window closes.
func WithLegacyProtocolOnly() ClientOption {
	return func(c *Client) {
		c.legacyOnly = true
	}
}

// WithDiscoverTimeout bounds how long Initialize waits for a server/discover
// reply before concluding that the server predates protocol version 2026-07-28
// and falling back to the initialize handshake.
//
// The default is five seconds. A negative value disables the bound.
func WithDiscoverTimeout(timeout time.Duration) ClientOption {
	return func(c *Client) {
		c.discoverTimeout = timeout
	}
}

// NewClient creates a new MCP client with the given transport.
// Usage:
//
//	stdio := transport.NewStdio("./mcp_server", nil, "xxx")
//	client, err := NewClient(stdio)
//	if err != nil {
//	    log.Fatalf("Failed to create client: %v", err)
//	}
func NewClient(transport transport.Interface, options ...ClientOption) *Client {
	client := &Client{
		transport:  transport,
		tracer:     tracing.NoopTracer(),
		propagator: tracing.NoopPropagator(),
	}
	client.publishEra(&era{clientInfo: defaultClientInfo()})

	for _, opt := range options {
		opt(client)
	}

	return client
}

// Start initiates the connection to the server.
// Must be called before using the client.
func (c *Client) Start(ctx context.Context) error {
	if c.transport == nil {
		return errors.New("transport is nil")
	}

	// Start is idempotent - transports handle being called multiple times
	err := c.transport.Start(ctx)
	if err != nil {
		return err
	}

	c.transport.SetNotificationHandler(func(notification mcp.JSONRPCNotification) {
		// The handlers run on the list published when the notification
		// arrived, so a handler that registers another handler does not wait
		// on itself, and the new one sees the next notification.
		if handlers := c.notifications.Load(); handlers != nil {
			for _, handler := range *handlers {
				handler(notification)
			}
		}
	})

	// Set up request handler for bidirectional communication (e.g., sampling)
	if bidirectional, ok := c.transport.(transport.BidirectionalInterface); ok {
		bidirectional.SetRequestHandler(c.handleIncomingRequest)
	}

	return nil
}

// Close shuts down the client and closes the transport. A client without a
// transport has nothing to close.
func (c *Client) Close() error {
	if c.transport == nil {
		return nil
	}
	return c.transport.Close()
}

// OnNotification registers a handler function to be called when notifications are received.
// Multiple handlers can be registered and will be called in the order they were added.
// Handlers registered during dispatch receive subsequent notifications.
func (c *Client) OnNotification(
	handler func(notification mcp.JSONRPCNotification),
) {
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	var next []func(mcp.JSONRPCNotification)
	if handlers := c.notifications.Load(); handlers != nil {
		next = slices.Clone(*handlers)
	}
	next = append(next, handler)
	c.notifications.Store(&next)
}

// OnConnectionLost registers a handler function to be called when the connection is lost.
// This is useful for handling HTTP2 idle timeout disconnections that should not be treated as errors.
func (c *Client) OnConnectionLost(handler func(error)) {
	type connectionLostSetter interface {
		SetConnectionLostHandler(func(error))
	}
	if setter, ok := c.transport.(connectionLostSetter); ok {
		setter.SetConnectionLostHandler(handler)
	}
}

// Helper methods

// GetTransport gives access to the underlying transport layer.
// Cast it to the specific transport type and obtain the other helper methods.
func (c *Client) GetTransport() transport.Interface {
	return c.transport
}

// GetServerCapabilities returns the server capabilities.
func (c *Client) GetServerCapabilities() mcp.ServerCapabilities {
	return c.currentEra().serverCapabilities
}

// GetClientCapabilities returns the client capabilities.
func (c *Client) GetClientCapabilities() mcp.ClientCapabilities {
	return c.currentEra().clientCapabilities
}

// GetSessionId returns the session ID of the transport.
// If the transport does not support sessions, it returns an empty string.
func (c *Client) GetSessionId() string {
	if c.transport == nil {
		return ""
	}
	return c.transport.GetSessionId()
}

// IsInitialized returns true if the client has been initialized.
func (c *Client) IsInitialized() bool {
	return c.initialized.Load()
}
