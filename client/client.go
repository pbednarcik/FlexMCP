package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

// sendRequest sends a JSON-RPC request to the server and waits for a response.
// Returns the raw JSON response message or an error if the request fails.
func (c *Client) sendRequest(
	ctx context.Context,
	method string,
	params any,
	header http.Header,
) (*json.RawMessage, error) {
	return c.sendRequestIn(ctx, c.currentEra(), method, params, header)
}

// sendRequestIn sends a request as a request of era e: the era in effect,
// or the one a probe tries.
func (c *Client) sendRequestIn(
	ctx context.Context,
	e *era,
	method string,
	params any,
	header http.Header,
) (*json.RawMessage, error) {
	if !c.initialized.Load() && method != "initialize" && method != string(mcp.MethodServerDiscover) {
		return nil, errors.New("client not initialized")
	}

	// Protocol version 2026-07-28 carries the protocol version, client
	// identity, and client capabilities in the _meta of every request, and
	// mirrors the method and name into HTTP headers (SEP-2575, SEP-2243).
	// Both are no-ops on legacy connections.
	given := params
	params = c.applyRequestMeta(e, params)
	header = c.applyStandardHeaders(e, header, method, params, given)

	id := c.requestID.Add(1)

	ctx, header, span := c.startSendSpan(ctx, method, header)

	request := transport.JSONRPCRequest{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      mcp.NewRequestId(id),
		Method:  method,
		Params:  params,
		Header:  header,
	}

	// A request whose context has already ended isn't sent, so there is
	// nothing to cancel.
	if err := ctx.Err(); err != nil {
		err = transport.NewError(err)
		endSendSpan(span, err)
		return nil, err
	}

	response, err := c.transport.SendRequest(ctx, request)
	if err != nil {
		// A transport's own timeout ends the wait as the context's deadline
		// would, and is cancelled the same way.
		reason := ctx.Err()
		if reason == nil && errors.Is(err, context.DeadlineExceeded) {
			reason = context.DeadlineExceeded
		}
		if reason != nil {
			c.cancelRequest(ctx, request, reason)
		}
		err = transport.NewError(err)
		endSendSpan(span, err)
		return nil, err
	}

	if response.Error != nil {
		err := response.Error.AsError()
		endSendSpan(span, err)
		return nil, err
	}

	endSendSpan(span, nil)
	return &response.Result, nil
}

// cancelRequest tells the server to stop working on a request whose context
// ended, or that timed out, before the response arrived, with a
// notifications/cancelled that references it and gives reason. Over stdio that is the only way to cancel a request, and
// protocol version 2026-07-28 requires it there.
//
// Over HTTP on protocol version 2026-07-28 nothing is sent: the transport
// ending the request's stream is the cancellation. Before that version a
// closed stream is not one, so the notification goes out there too. The
// handshake is left alone, since initialize must not be cancelled and
// nothing else is sent before it completes, and so are task-augmented
// requests, which are cancelled with tasks/cancel instead.
func (c *Client) cancelRequest(ctx context.Context, request transport.JSONRPCRequest, reason error) {
	if !c.initialized.Load() ||
		request.Method == string(mcp.MethodInitialize) ||
		request.Method == string(mcp.MethodServerDiscover) ||
		isTaskAugmented(request.Params) {
		return
	}
	if _, ok := c.transport.(transport.HTTPConnection); ok && c.isModern() {
		return
	}
	notification := mcp.JSONRPCNotification{
		JSONRPC: mcp.JSONRPC_VERSION,
		Method:  string(mcp.MethodNotificationCancelled),
		Params: mcp.NotificationParams{
			AdditionalFields: map[string]any{"requestId": request.ID, "reason": reason.Error()},
		},
	}
	// The request's context may have ended, so send with one of our own, and off
	// the caller's path: the caller already has its error, and a peer that
	// doesn't read must not hold it up. The notification is best effort.
	ctx = context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(ctx, cancelNotificationTimeout)
		defer cancel()
		_ = c.transport.SendNotification(ctx, notification)
	}()
}

// isTaskAugmented reports whether params ask for the request to run as a
// task.
func isTaskAugmented(params any) bool {
	encoded, err := json.Marshal(params)
	if err != nil {
		return false
	}
	var fields struct {
		Task json.RawMessage `json:"task"`
	}
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return false
	}
	return len(fields.Task) > 0 && string(fields.Task) != "null"
}

// cancelNotificationTimeout bounds how long cancelRequest waits to send a
// notifications/cancelled.
const cancelNotificationTimeout = 5 * time.Second

func outboundHeader(header http.Header, requestMethod string) http.Header {
	// A typed request with Method set was decoded from an inbound JSON-RPC
	// message. Its Header contains received HTTP metadata, not opt-in outbound
	// headers for a new client call.
	if requestMethod != "" {
		return nil
	}
	return header
}

// Initialize establishes the connection with the server.
//
// It must be called after Start, and before any request methods.
//
// Protocol version 2026-07-28 removed the initialize handshake (SEP-2575), so
// this method first probes the server with server/discover. When the probe
// succeeds the connection is stateless and every subsequent request carries
// its own protocol metadata; the returned InitializeResult is rendered from
// the discovery response so that callers observe the same value in both eras.
//
// When the probe fails before a modern version shared with the server is
// found, the server is taken to be legacy, and the classic initialize
// handshake is performed instead. That covers a server that answers the probe
// but offers only legacy versions. Once a shared modern version is found, a
// negotiation failure is returned to the caller rather than falling back.
func (c *Client) Initialize(
	ctx context.Context,
	request mcp.InitializeRequest,
) (*mcp.InitializeResult, error) {
	// The negotiation works on a copy of the era, published once it is
	// settled: a request in flight beside it sees the era before or after,
	// never the probe's.
	next := *c.currentEra()
	if request.Params.ClientInfo.Name != "" {
		next.clientInfo = request.Params.ClientInfo
	}
	next.clientCapabilities = mergeClientCapabilities(next.clientCapabilities, request.Params.Capabilities)
	header := outboundHeader(request.Header, request.Method)

	preferred := request.Params.ProtocolVersion
	if preferred == "" {
		preferred = c.preferredVersion
	}

	// Try the stateless protocol core first, unless the caller pinned an
	// earlier revision or configured the transport in a way that rules it out.
	if !c.legacyOnly && !c.transportRequiresLegacyProtocol() &&
		(preferred == "" || mcp.IsModernProtocol(preferred)) {
		discovered, modern, err := c.negotiateModern(ctx, preferred, &next, header)
		if err == nil {
			next.serverCapabilities = discovered.Capabilities
			c.publishEra(&next)
			c.setTransportVersion(next.version)
			c.initialized.Store(true)
			return initializeResultFromDiscover(next.version, discovered), nil
		}
		if modern {
			return nil, err
		}
		// Fall through to the handshake: the server is not modern.
	}

	return c.initializeLegacy(ctx, preferred, &next, header)
}

// initializeLegacy performs the initialize/initialized handshake used by
// protocol versions up to and including 2025-11-25.
func (c *Client) initializeLegacy(
	ctx context.Context,
	preferred string,
	next *era,
	header http.Header,
) (*mcp.InitializeResult, error) {
	capabilities := c.effectiveCapabilities(next)

	// Ensure we send a params object with all required fields
	params := struct {
		ProtocolVersion string                 `json:"protocolVersion"`
		ClientInfo      mcp.Implementation     `json:"clientInfo"`
		Capabilities    mcp.ClientCapabilities `json:"capabilities"`
	}{
		ProtocolVersion: preferred,
		ClientInfo:      next.clientInfo,
		Capabilities:    capabilities,
	}

	// The handshake cannot negotiate 2026-07-28 or later, so ask for the
	// newest revision that still uses it.
	if params.ProtocolVersion == "" || mcp.IsModernProtocol(params.ProtocolVersion) {
		params.ProtocolVersion = mcp.LATEST_LEGACY_PROTOCOL_VERSION
	}

	// The handshake is a legacy request, carrying no per-request metadata.
	handshake := *next
	handshake.version = ""
	response, err := c.sendRequestIn(ctx, &handshake, "initialize", params, header)
	if err != nil {
		return nil, err
	}

	var result mcp.InitializeResult
	if err := json.Unmarshal(*response, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	// Validate protocol version
	if !mcp.IsValidProtocolVersion(result.ProtocolVersion) {
		return nil, mcp.UnsupportedProtocolVersionError{Version: result.ProtocolVersion}
	}

	// Publish the era: the server's capabilities and the negotiated version.
	next.serverCapabilities = result.Capabilities
	next.version = result.ProtocolVersion
	c.publishEra(next)
	c.setTransportVersion(next.version)

	// Send initialized notification
	notification := mcp.JSONRPCNotification{
		JSONRPC: mcp.JSONRPC_VERSION,
		Method:  string(mcp.MethodNotificationInitialized),
	}

	err = c.transport.SendNotification(ctx, notification)
	if err != nil {
		return nil, fmt.Errorf(
			"failed to send initialized notification: %w",
			err,
		)
	}

	c.initialized.Store(true)
	return &result, nil
}

// setTransportVersion propagates the protocol version in effect to HTTP
// transports, which mirror it in the Mcp-Protocol-Version header.
func (c *Client) setTransportVersion(version string) {
	if httpConn, ok := c.transport.(transport.HTTPConnection); ok {
		httpConn.SetProtocolVersion(version)
	}
}

// mergeClientCapabilities overlays the capabilities declared on a request onto
// those configured at construction.
func mergeClientCapabilities(base, overlay mcp.ClientCapabilities) mcp.ClientCapabilities {
	if overlay.Extensions != nil {
		base.Extensions = overlay.Extensions
	}
	if overlay.Experimental != nil {
		base.Experimental = overlay.Experimental
	}
	if overlay.Roots != nil {
		base.Roots = overlay.Roots
	}
	if overlay.Sampling != nil {
		base.Sampling = overlay.Sampling
	}
	if overlay.Elicitation != nil {
		base.Elicitation = overlay.Elicitation
	}
	if overlay.Tasks != nil {
		base.Tasks = overlay.Tasks
	}
	return base
}

// Ping sends a ping request to verify the server is responsive.
//
// Deprecated: the ping RPC was removed in protocol version 2026-07-28
// (SEP-2575); liveness is a transport concern there. On a modern connection
// this method succeeds without sending anything.
func (c *Client) Ping(ctx context.Context) error {
	if c.isModern() {
		return nil
	}
	_, err := c.sendRequest(ctx, string(mcp.MethodPing), nil, nil)
	return err
}

// ListResourcesByPage manually list resources by page.
func (c *Client) ListResourcesByPage(
	ctx context.Context,
	request mcp.ListResourcesRequest,
) (*mcp.ListResourcesResult, error) {
	result, err := listByPage[mcp.ListResourcesResult](ctx, c, request.PaginatedRequest, request.Header, string(mcp.MethodResourcesList))
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ErrRepeatedListCursor is returned when a paginated list repeats a cursor
// that was already requested. Following it would request the same page again.
var ErrRepeatedListCursor = errors.New("list cursor did not advance")

// noteListCursor records next before it is requested. A cursor already in
// seen means the server did not advance.
func noteListCursor(seen map[mcp.Cursor]struct{}, next mcp.Cursor) error {
	if _, dup := seen[next]; dup {
		return fmt.Errorf("%w: %q", ErrRepeatedListCursor, next)
	}
	seen[next] = struct{}{}
	return nil
}

// seenCursors starts the set of cursors a walk has requested: the caller's
// own, when it gave one.
func seenCursors(start *mcp.Cursor) map[mcp.Cursor]struct{} {
	seen := map[mcp.Cursor]struct{}{}
	if start != nil {
		seen[*start] = struct{}{}
	}
	return seen
}

// ListResources lists all resources by following paginated responses.
func (c *Client) ListResources(
	ctx context.Context,
	request mcp.ListResourcesRequest,
) (*mcp.ListResourcesResult, error) {
	seen := seenCursors(request.Params.Cursor)
	result, err := c.ListResourcesByPage(ctx, request)
	if err != nil {
		return nil, err
	}
	for result.NextCursor != nil {
		if err := noteListCursor(seen, *result.NextCursor); err != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			request.Params.Cursor = result.NextCursor
			newPageRes, err := c.ListResourcesByPage(ctx, request)
			if err != nil {
				return nil, err
			}
			result.Resources = append(result.Resources, newPageRes.Resources...)
			result.NextCursor = newPageRes.NextCursor
		}
	}
	return result, nil
}

// ListResourceTemplatesByPage manually lists resource templates by page.
func (c *Client) ListResourceTemplatesByPage(
	ctx context.Context,
	request mcp.ListResourceTemplatesRequest,
) (*mcp.ListResourceTemplatesResult, error) {
	result, err := listByPage[mcp.ListResourceTemplatesResult](ctx, c, request.PaginatedRequest, request.Header, string(mcp.MethodResourcesTemplatesList))
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ListResourceTemplates lists all resource templates by following paginated responses.
func (c *Client) ListResourceTemplates(
	ctx context.Context,
	request mcp.ListResourceTemplatesRequest,
) (*mcp.ListResourceTemplatesResult, error) {
	seen := seenCursors(request.Params.Cursor)
	result, err := c.ListResourceTemplatesByPage(ctx, request)
	if err != nil {
		return nil, err
	}
	for result.NextCursor != nil {
		if err := noteListCursor(seen, *result.NextCursor); err != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			request.Params.Cursor = result.NextCursor
			newPageRes, err := c.ListResourceTemplatesByPage(ctx, request)
			if err != nil {
				return nil, err
			}
			result.ResourceTemplates = append(result.ResourceTemplates, newPageRes.ResourceTemplates...)
			result.NextCursor = newPageRes.NextCursor
		}
	}
	return result, nil
}

// ReadResource reads the contents of a resource from the server.
func (c *Client) ReadResource(
	ctx context.Context,
	request mcp.ReadResourceRequest,
) (*mcp.ReadResourceResult, error) {
	request.Params.Meta = c.injectMeta(ctx, request.Params.Meta)
	header := outboundHeader(request.Header, request.Method)
	return multiRoundTrip(ctx, c, request.Params.MultiRoundTripParams,
		func(ctx context.Context, roundTrip mcp.MultiRoundTripParams) (*mcp.ReadResourceResult, error) {
			return c.readResourceOnce(ctx, request, roundTrip, header)
		},
		readResourceNeedsInput,
	)
}

// ListPromptsByPage manually lists prompts by page.
func (c *Client) ListPromptsByPage(
	ctx context.Context,
	request mcp.ListPromptsRequest,
) (*mcp.ListPromptsResult, error) {
	result, err := listByPage[mcp.ListPromptsResult](ctx, c, request.PaginatedRequest, request.Header, string(mcp.MethodPromptsList))
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ListPrompts lists all prompts by following paginated responses.
func (c *Client) ListPrompts(
	ctx context.Context,
	request mcp.ListPromptsRequest,
) (*mcp.ListPromptsResult, error) {
	seen := seenCursors(request.Params.Cursor)
	result, err := c.ListPromptsByPage(ctx, request)
	if err != nil {
		return nil, err
	}
	for result.NextCursor != nil {
		if err := noteListCursor(seen, *result.NextCursor); err != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			request.Params.Cursor = result.NextCursor
			newPageRes, err := c.ListPromptsByPage(ctx, request)
			if err != nil {
				return nil, err
			}
			result.Prompts = append(result.Prompts, newPageRes.Prompts...)
			result.NextCursor = newPageRes.NextCursor
		}
	}
	return result, nil
}

// GetPrompt gets a prompt and renders it with the provided arguments.
func (c *Client) GetPrompt(
	ctx context.Context,
	request mcp.GetPromptRequest,
) (*mcp.GetPromptResult, error) {
	request.Params.Meta = c.injectMeta(ctx, request.Params.Meta)
	header := outboundHeader(request.Header, request.Method)
	return multiRoundTrip(ctx, c, request.Params.MultiRoundTripParams,
		func(ctx context.Context, roundTrip mcp.MultiRoundTripParams) (*mcp.GetPromptResult, error) {
			return c.getPromptOnce(ctx, request, roundTrip, header)
		},
		getPromptNeedsInput,
	)
}

// ListToolsByPage manually lists tools by page.
func (c *Client) ListToolsByPage(
	ctx context.Context,
	request mcp.ListToolsRequest,
) (*mcp.ListToolsResult, error) {
	result, err := listByPage[mcp.ListToolsResult](ctx, c, request.PaginatedRequest, request.Header, string(mcp.MethodToolsList))
	if err != nil {
		return nil, err
	}
	result.Tools = c.withoutInvalidHeaderTools(ctx, result.Tools)
	// Cache the definitions so that tools/call requests can mirror
	// x-mcp-header annotated parameters into HTTP headers (SEP-2243).
	c.rememberTools(result.Tools)
	return result, nil
}

// ListTools lists all tools by following paginated responses.
func (c *Client) ListTools(
	ctx context.Context,
	request mcp.ListToolsRequest,
) (*mcp.ListToolsResult, error) {
	seen := seenCursors(request.Params.Cursor)
	whole := request.Params.Cursor == nil
	result, err := c.ListToolsByPage(ctx, request)
	if err != nil {
		return nil, err
	}
	for result.NextCursor != nil {
		if err := noteListCursor(seen, *result.NextCursor); err != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			request.Params.Cursor = result.NextCursor
			newPageRes, err := c.ListToolsByPage(ctx, request)
			if err != nil {
				return nil, err
			}
			result.Tools = append(result.Tools, newPageRes.Tools...)
			result.NextCursor = newPageRes.NextCursor
		}
	}
	// A walk from the first page is the server's whole list: the cache
	// becomes it, and a tool the server stopped listing is forgotten. A walk
	// from a caller's cursor is part of the list; its pages merged in.
	if whole {
		c.replaceTools(result.Tools)
	}
	return result, nil
}

// CallTool invokes a tool on the server.
func (c *Client) CallTool(
	ctx context.Context,
	request mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	request.Params.Meta = c.injectMeta(ctx, request.Params.Meta)
	header := outboundHeader(request.Header, request.Method)
	return multiRoundTrip(ctx, c, request.Params.MultiRoundTripParams,
		func(ctx context.Context, roundTrip mcp.MultiRoundTripParams) (*mcp.CallToolResult, error) {
			return c.callToolOnce(ctx, request, roundTrip, header)
		},
		callToolNeedsInput,
	)
}

// SetLevel sets the minimum severity of log messages the server should send.
//
// Protocol version 2026-07-28 removed the logging/setLevel RPC: the level is
// declared per request, in _meta (SEP-2575). On a modern connection this
// method records the level locally, and every subsequent request carries it.
//
// Deprecated: the Logging feature is deprecated as of protocol version
// 2026-07-28 (SEP-2577). Log to stderr or use OpenTelemetry instead.
func (c *Client) SetLevel(
	ctx context.Context,
	request mcp.SetLevelRequest,
) error {
	if c.isModern() {
		c.logLevelMu.Lock()
		c.logLevel = request.Params.Level
		c.logLevelMu.Unlock()
		return nil
	}
	_, err := c.sendRequest(ctx, "logging/setLevel", request.Params, outboundHeader(request.Header, request.Method))
	return err
}

// Complete requests completion suggestions from the server.
func (c *Client) Complete(
	ctx context.Context,
	request mcp.CompleteRequest,
) (*mcp.CompleteResult, error) {
	response, err := c.sendRequest(ctx, "completion/complete", request.Params, outboundHeader(request.Header, request.Method))
	if err != nil {
		return nil, err
	}

	var result mcp.CompleteResult
	if err := json.Unmarshal(*response, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}
	if err := c.checkResultType(result.ResultType); err != nil {
		return nil, err
	}

	return &result, nil
}

// checkResultType refuses a resultType the client does not accept: the
// core values are accepted; "task" belongs to the Tasks extension, which
// answers tools/call alone (see checkToolResultType); any other value is
// invalid rather than complete.
func (c *Client) checkResultType(resultType mcp.ResultType) error {
	return checkResultType(resultType, false, false)
}

// checkInputResultType is checkResultType for prompts/get and
// resources/read, which the multi round-trip pattern may answer with
// "input_required".
func (c *Client) checkInputResultType(resultType mcp.ResultType) error {
	return checkResultType(resultType, false, true)
}

// checkToolResultType is checkResultType for the answer to tools/call, the
// one request the Tasks extension may answer with a task: "task" is accepted
// when this connection declared the extension.
func (c *Client) checkToolResultType(resultType mcp.ResultType) error {
	return checkResultType(resultType, c.acceptsTasks(), true)
}

// errUnacceptedResultType marks a result whose resultType this request may
// not be answered with: a modern server's answer, however wrong.
var errUnacceptedResultType = errors.New("unaccepted resultType")

func checkResultType(resultType mcp.ResultType, tasks, input bool) error {
	if resultType.Accepted(tasks, input) {
		return nil
	}
	return fmt.Errorf("%w %q in the result", errUnacceptedResultType, resultType)
}

// acceptsTasks reports whether this connection declared the Tasks extension:
// extensions["io.modelcontextprotocol/tasks"] in the capabilities every
// modern request carries. Before 2026-07-28 there was no resultType, and the
// tasks capability of that era is the client's own, for the requests it
// receives, so a handshake-era server is never answered with a task.
func (c *Client) acceptsTasks() bool {
	e := c.currentEra()
	if !mcp.IsModernProtocol(e.version) {
		return false
	}
	_, ok := e.clientCapabilities.Extensions[mcp.ExtensionTasks]
	return ok
}

// RootListChanges sends a roots list-changed notification to the server.
//
// Protocol version 2026-07-28 removed the notification along with the
// roots.listChanged capability: a server asks for the roots it needs through
// a multi round-trip request. On a modern connection this method sends
// nothing and returns nil.
//
// Deprecated: the Roots feature is deprecated as of protocol version
// 2026-07-28 (SEP-2577). Pass paths as tool arguments or resource URIs.
func (c *Client) RootListChanges(
	ctx context.Context,
) error {
	if c.isModern() {
		return nil
	}
	// Send root list changes notification
	notification := mcp.JSONRPCNotification{
		JSONRPC: mcp.JSONRPC_VERSION,
		Method:  mcp.MethodNotificationRootsListChanged,
	}

	err := c.transport.SendNotification(ctx, notification)
	if err != nil {
		return fmt.Errorf(
			"failed to send root list change notification: %w",
			err,
		)
	}
	return nil
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

func listByPage[T any](
	ctx context.Context,
	client *Client,
	request mcp.PaginatedRequest,
	header http.Header,
	method string,
) (*T, error) {
	response, err := client.sendRequest(ctx, method, request.Params, outboundHeader(header, request.Method))
	if err != nil {
		return nil, err
	}
	var result T
	if err := json.Unmarshal(*response, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}
	if metadata, ok := any(&result).(mcp.ResultMetadata); ok {
		if err := client.checkResultType(metadata.GetResultType()); err != nil {
			return nil, err
		}
	}
	return &result, nil
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

// CancelTask returns canceled task result
func (c *Client) CancelTask(
	ctx context.Context,
	request mcp.CancelTaskRequest,
) (*mcp.CancelTaskResult, error) {
	response, err := c.sendRequest(ctx, string(mcp.MethodTasksCancel), request.Params, outboundHeader(request.Header, request.Method))
	if err != nil {
		return nil, err
	}

	return mcp.ParseCancelTaskResult(response)
}

// ErrRepeatedTaskListCursor is returned when tasks/list repeats a cursor that
// was already requested. Following it would request the same page again.
var ErrRepeatedTaskListCursor = errors.New("tasks/list cursor did not advance")

// ListTasksByPage lists one page of tasks.
func (c *Client) ListTasksByPage(
	ctx context.Context,
	request mcp.ListTasksRequest,
) (*mcp.ListTasksResult, error) {
	response, err := c.sendRequest(ctx, string(mcp.MethodTasksList), request.Params, outboundHeader(request.Header, request.Method))
	if err != nil {
		return nil, err
	}

	return mcp.ParseListTasksResult(response)
}

// ListTasks lists all tasks by following paginated responses.
func (c *Client) ListTasks(
	ctx context.Context,
	request mcp.ListTasksRequest,
) (*mcp.ListTasksResult, error) {
	seen := seenCursors(request.Params.Cursor)
	result, err := c.ListTasksByPage(ctx, request)
	if err != nil {
		return nil, err
	}
	for result.NextCursor != nil {
		next := *result.NextCursor
		if _, dup := seen[next]; dup {
			return nil, fmt.Errorf("%w: %q", ErrRepeatedTaskListCursor, next)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
			seen[next] = struct{}{}
			request.Params.Cursor = result.NextCursor
			page, err := c.ListTasksByPage(ctx, request)
			if err != nil {
				return nil, err
			}
			result.Tasks = append(result.Tasks, page.Tasks...)
			result.NextCursor = page.NextCursor
		}
	}
	return result, nil
}

// TaskResult returns finished task result
func (c *Client) TaskResult(
	ctx context.Context,
	request mcp.TaskResultRequest,
) (*mcp.TaskResultResult, error) {
	response, err := c.sendRequest(ctx, string(mcp.MethodTasksResult), request.Params, outboundHeader(request.Header, request.Method))
	if err != nil {
		return nil, err
	}

	return mcp.ParseTaskResultResult(response)
}

// GetTask returns task with current status
func (c *Client) GetTask(
	ctx context.Context,
	request mcp.GetTaskRequest,
) (*mcp.GetTaskResult, error) {
	response, err := c.sendRequest(ctx, string(mcp.MethodTasksGet), request.Params, outboundHeader(request.Header, request.Method))
	if err != nil {
		return nil, err
	}

	return mcp.ParseGetTaskResult(response)
}
