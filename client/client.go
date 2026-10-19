package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
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

// WithSamplingHandler sets the sampling handler for the client.
// When set, the client declares the sampling capability, unless the
// capabilities it was given declare it already: in the initialize handshake
// on a legacy connection, and in every request's _meta on protocol version
// 2026-07-28.
func WithSamplingHandler(handler SamplingHandler) ClientOption {
	return func(c *Client) {
		c.samplingHandler = handler
	}
}

// WithRootsHandler sets the roots handler for the client.
// When set, the client declares the roots capability, unless the
// capabilities it was given declare it already: in the initialize handshake
// on a legacy connection, with listChanged, and in every request's _meta on
// protocol version 2026-07-28, without it, since that version removed the
// roots list_changed notification.
func WithRootsHandler(handler RootsHandler) ClientOption {
	return func(c *Client) {
		c.rootsHandler = handler
	}
}

// WithElicitationHandler sets the elicitation handler for the client.
// When set, the client declares the elicitation capability, unless the
// capabilities it was given declare it already: in the initialize handshake
// on a legacy connection, and in every request's _meta on protocol version
// 2026-07-28.
func WithElicitationHandler(handler ElicitationHandler) ClientOption {
	return func(c *Client) {
		c.elicitationHandler = handler
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

// WithMaxInputRoundTrips bounds how many requests one call sends, the first
// included, while the server keeps asking for input; past it the call fails
// with ErrMaxInputRoundTripsExceeded.
//
// The default is 10. See the multi round-trip request pattern (SEP-2322).
func WithMaxInputRoundTrips(limit int) ClientOption {
	return func(c *Client) {
		c.maxInputRoundTrips = limit
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

// requestError is a server request the client could not serve: its text as
// written, and its kind, mcp.ErrMethodNotFound or mcp.ErrInvalidParams,
// which the transport answers with the matching JSON-RPC code.
type requestError struct {
	kind error
	err  error
}

func (e requestError) Error() string   { return e.err.Error() }
func (e requestError) Unwrap() []error { return []error{e.kind, e.err} }

// requestFailed is a requestError of kind, its text formatted as fmt.Errorf
// formats it.
func requestFailed(kind error, format string, args ...any) error {
	return requestError{kind: kind, err: fmt.Errorf(format, args...)}
}

// handleIncomingRequest processes incoming requests from the server.
// This is the main entry point for server-to-client requests like sampling and elicitation.
func (c *Client) handleIncomingRequest(ctx context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	switch request.Method {
	case string(mcp.MethodSamplingCreateMessage):
		return c.handleSamplingRequestTransport(ctx, request)
	case string(mcp.MethodElicitationCreate):
		return c.handleElicitationRequestTransport(ctx, request)
	case string(mcp.MethodPing):
		return c.handlePingRequestTransport(ctx, request)
	case string(mcp.MethodListRoots):
		return c.handleListRootsRequestTransport(ctx, request)
	default:
		return nil, requestFailed(mcp.ErrMethodNotFound, "unsupported request method: %s", request.Method)
	}
}

// handleSamplingRequestTransport handles sampling requests at the transport level.
func (c *Client) handleSamplingRequestTransport(ctx context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	if c.samplingHandler == nil {
		return nil, requestFailed(mcp.ErrMethodNotFound, "no sampling handler configured")
	}

	// Parse the request parameters
	var params mcp.CreateMessageParams
	if request.Params != nil {
		paramsBytes, err := json.Marshal(request.Params)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal params: %w", err)
		}
		if err := json.Unmarshal(paramsBytes, &params); err != nil {
			return nil, requestFailed(mcp.ErrInvalidParams, "failed to unmarshal params: %w", err)
		}
	}

	// Fix content parsing - HTTP transport unmarshals TextContent as map[string]any
	// Use the helper function to properly handle content from different transports
	for i := range params.Messages {
		content, err := mcp.ParseSamplingContent(params.Messages[i].Content)
		if err != nil {
			return nil, requestFailed(mcp.ErrInvalidParams, "failed to parse content for message %d: %w", i, err)
		}
		params.Messages[i].Content = content
	}

	// Create the MCP request
	mcpRequest := mcp.CreateMessageRequest{
		Method:              string(mcp.MethodSamplingCreateMessage),
		CreateMessageParams: params,
	}

	// Call the sampling handler
	result, err := c.samplingHandler.CreateMessage(ctx, mcpRequest)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("sampling handler returned no result")
	}

	// Marshal the result
	resultBytes, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal result: %w", err)
	}

	// Create the transport response
	response := transport.NewJSONRPCResultResponse(request.ID, json.RawMessage(resultBytes))

	return response, nil
}

// checkRoots returns result with a non-nil Roots, so that a client with no
// roots answers with "roots": [] rather than null, which the schema doesn't
// allow, and refuses a root that is not a file:// URI, the only kind the
// protocol defines.
func checkRoots(result *mcp.ListRootsResult) (*mcp.ListRootsResult, error) {
	for _, root := range result.Roots {
		if !strings.HasPrefix(root.URI, "file://") {
			return nil, fmt.Errorf("root %q is not a file:// URI", root.URI)
		}
	}
	if result.Roots != nil {
		return result, nil
	}
	withRoots := *result
	withRoots.Roots = []mcp.Root{}
	return &withRoots, nil
}

// handleListRootsRequestTransport handles list roots requests at the transport level.
func (c *Client) handleListRootsRequestTransport(ctx context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	if c.rootsHandler == nil {
		return nil, requestFailed(mcp.ErrMethodNotFound, "no roots handler configured")
	}

	// Create the MCP request
	mcpRequest := mcp.ListRootsRequest{
		Method: string(mcp.MethodListRoots),
	}

	// Call the list roots handler
	result, err := c.rootsHandler.ListRoots(ctx, mcpRequest)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("roots handler returned no result")
	}
	result, err = checkRoots(result)
	if err != nil {
		return nil, err
	}

	// Marshal the result
	resultBytes, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal result: %w", err)
	}

	// Create the transport response
	response := transport.NewJSONRPCResultResponse(request.ID, json.RawMessage(resultBytes))

	return response, nil
}

// handleElicitationRequestTransport handles elicitation requests at the transport level.
func (c *Client) handleElicitationRequestTransport(ctx context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	if c.elicitationHandler == nil {
		return nil, requestFailed(mcp.ErrMethodNotFound, "no elicitation handler configured")
	}

	// Parse the request parameters
	var params mcp.ElicitationParams
	if request.Params != nil {
		paramsBytes, err := json.Marshal(request.Params)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal params: %w", err)
		}
		if err := json.Unmarshal(paramsBytes, &params); err != nil {
			return nil, requestFailed(mcp.ErrInvalidParams, "failed to unmarshal params: %w", err)
		}
	}

	// As for a multi round-trip input request: valid, and in a mode this
	// client declared.
	if err := c.checkElicitation(params); err != nil {
		return nil, requestFailed(mcp.ErrInvalidParams, "%w", err)
	}

	// Create the MCP request
	mcpRequest := mcp.ElicitationRequest{
		Method: string(mcp.MethodElicitationCreate),
		Params: params,
	}

	// Call the elicitation handler
	result, err := c.elicitationHandler.Elicit(ctx, mcpRequest)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("elicitation handler returned no result")
	}

	// Marshal the result
	resultBytes, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal result: %w", err)
	}

	// Create the transport response
	response := transport.NewJSONRPCResultResponse(request.ID, resultBytes)

	return response, nil
}

func (c *Client) handlePingRequestTransport(ctx context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	b, _ := json.Marshal(&mcp.EmptyResult{})
	return transport.NewJSONRPCResultResponse(request.ID, b), nil
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
