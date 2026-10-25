package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pbednarcik/flexmcp/mcp"
)

// StreamableHTTPCOption configures a StreamableHTTP transport client.
type StreamableHTTPCOption func(*StreamableHTTP)

// WithContinuousListening enables receiving server-to-client notifications when no request is in flight.
// In particular, if you want to receive global notifications from the server (like ToolListChangedNotification),
// you should enable this option.
//
// It will establish a standalone long-live GET HTTP connection to the server.
// https://modelcontextprotocol.io/specification/2025-03-26/basic/transports#listening-for-messages-from-the-server
// NOTICE: Even enabled, the server may not support this feature.
//
// Deprecated: protocol version 2026-07-28 removed the standalone GET stream
// (SEP-2575). A transport with this option enabled keeps the connection on a
// legacy protocol version; use Client.Listen to open a subscriptions/listen
// stream instead.
func WithContinuousListening() StreamableHTTPCOption {
	return func(sc *StreamableHTTP) {
		sc.getListeningEnabled = true
	}
}

// WithHTTPBasicClient sets a custom HTTP client on the StreamableHTTP transport.
func WithHTTPBasicClient(client *http.Client) StreamableHTTPCOption {
	return func(sc *StreamableHTTP) {
		sc.httpClient = client
	}
}

// WithHTTPHeaders sets static headers for StreamableHTTP requests.
func WithHTTPHeaders(headers map[string]string) StreamableHTTPCOption {
	return func(sc *StreamableHTTP) {
		sc.headers = headers
	}
}

// WithHTTPHeaderFunc sets a function that returns headers for StreamableHTTP requests.
func WithHTTPHeaderFunc(headerFunc HTTPHeaderFunc) StreamableHTTPCOption {
	return func(sc *StreamableHTTP) {
		sc.headerFunc = headerFunc
	}
}

// WithHTTPTimeout sets the timeout for a HTTP request and stream.
func WithHTTPTimeout(timeout time.Duration) StreamableHTTPCOption {
	return func(sc *StreamableHTTP) {
		sc.httpClient.Timeout = timeout
	}
}

// WithHTTPOAuth enables OAuth authentication for the client.
func WithHTTPOAuth(config OAuthConfig) StreamableHTTPCOption {
	return func(sc *StreamableHTTP) {
		sc.oauthHandler = NewOAuthHandler(config)
	}
}

// WithHTTPLogger sets a custom structured logger for the StreamableHTTP
// transport. A nil logger falls back to slog.Default().
func WithHTTPLogger(logger *slog.Logger) StreamableHTTPCOption {
	return func(sc *StreamableHTTP) {
		if logger == nil {
			sc.logger = slog.Default()
			return
		}
		sc.logger = logger
	}
}

// WithLogger sets a custom structured logger for the StreamableHTTP transport.
//
// Deprecated: Use [WithHTTPLogger] instead.
func WithLogger(logger *slog.Logger) StreamableHTTPCOption {
	return WithHTTPLogger(logger)
}

// WithSession creates a client with a pre-configured session
func WithSession(sessionID string) StreamableHTTPCOption {
	return func(sc *StreamableHTTP) {
		sc.sessionID.Store(sessionID)
	}
}

// WithStreamableHTTPHost sets a custom Host header for the StreamableHTTP client, enabling manual DNS resolution.
// This allows connecting to an IP address while sending a specific Host header to the server.
// For example, connecting to "http://192.168.1.100:8080/mcp" but sending Host: "api.example.com"
func WithStreamableHTTPHost(host string) StreamableHTTPCOption {
	return func(sc *StreamableHTTP) {
		sc.host = host
	}
}

// StreamableHTTP implements Streamable HTTP transport.
//
// It transmits JSON-RPC messages over individual HTTP requests. One message per request.
// The HTTP response body can either be a single JSON-RPC response,
// or an upgraded SSE stream that concludes with a JSON-RPC response for the same request.
//
// https://modelcontextprotocol.io/specification/2025-03-26/basic/transports
//
// Before protocol version 2026-07-28, a response stream the server ends
// before the response, once it has sent an event ID, is resumed with a GET
// carrying Last-Event-ID (SEP-1699). Streams that break off are not resumed
// (https://modelcontextprotocol.io/specification/2025-11-25/basic/transports#resumability-and-redelivery).
// A server may keep a resumed request open indefinitely, so callers should
// give requests a deadline.
type StreamableHTTP struct {
	serverURL           *url.URL
	target              string // serverURL as a string, the URL every request goes to
	httpClient          *http.Client
	headers             map[string]string
	headerFunc          HTTPHeaderFunc
	host                string
	logger              *slog.Logger
	getListeningEnabled bool

	sessionID       atomic.Value // string
	protocolVersion atomic.Value // string

	// sessionMu guards sessionEnded and the GET stream's connection, and
	// makes reading the session ID for a request and ending the session
	// exclusive.
	sessionMu sync.Mutex
	// sessionEnded is set when the server answers 404 for the session, and
	// closed and cleared when a new connection starts. Meanwhile requests fail
	// with ErrSessionTerminated rather than go out without a session ID, which
	// the server would reject or take as the start of another session.
	sessionEnded chan struct{}
	// listenSession is the session the GET stream is connected on, and
	// listenCancel ends that connection.
	listenSession string
	listenCancel  context.CancelFunc

	initialized     chan struct{}
	initializedOnce sync.Once

	notificationHandler func(mcp.JSONRPCNotification)
	notifyMu            sync.RWMutex

	// Request handler for incoming server-to-client requests (like sampling)
	requestHandler RequestHandler
	requestMu      sync.RWMutex

	// closing ends when Close runs, and closed is its Done channel; a
	// request's context watches closing through context.AfterFunc.
	closing        context.Context
	closeTransport context.CancelFunc
	closed         <-chan struct{}
	closeOnce      sync.Once

	// OAuth support
	oauthHandler *OAuthHandler
	// oauthRecipient is the base URL the transport set on oauthHandler: a
	// request carries a token only while the handler still targets it.
	oauthRecipient string
}

// NewStreamableHTTP creates a new Streamable HTTP transport with the given server URL.
// Returns an error if the URL is invalid.
func NewStreamableHTTP(serverURL string, options ...StreamableHTTPCOption) (*StreamableHTTP, error) {
	parsedURL, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	smc := &StreamableHTTP{
		serverURL:   parsedURL,
		target:      parsedURL.String(),
		httpClient:  &http.Client{},
		headers:     make(map[string]string),
		logger:      slog.Default(),
		initialized: make(chan struct{}),
	}
	smc.closing, smc.closeTransport = context.WithCancel(context.Background())
	smc.closed = smc.closing.Done()
	smc.sessionID.Store("") // set initial value to simplify later usage

	for _, opt := range options {
		if opt != nil {
			opt(smc)
		}
	}

	// If OAuth is configured, set the base URL for metadata discovery
	if smc.oauthHandler != nil {
		discoveryURL := *parsedURL
		discoveryURL.RawQuery = ""
		discoveryURL.Fragment = ""
		smc.oauthRecipient = discoveryURL.String()
		smc.oauthHandler.SetBaseURL(smc.oauthRecipient)
		smc.httpClient = originBoundClient(smc.httpClient)
	}

	return smc, nil
}

// Start initiates the HTTP connection to the server.
func (c *StreamableHTTP) Start(ctx context.Context) error {
	// Start is idempotent - check if already initialized
	select {
	case <-c.initialized:
		return nil
	default:
	}

	// For Streamable HTTP, we don't need to establish a persistent connection by default
	if c.getListeningEnabled {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					c.logger.Error("panic in listener goroutine", "panic", r)
				}
			}()
			select {
			case <-c.initialized:
				// Protocol version 2026-07-28 removed the standalone GET
				// stream; server-to-client notifications arrive on a
				// subscriptions/listen response stream instead.
				if c.isModern() {
					return
				}
				ctx, cancel := c.contextAwareOfClientClose(ctx)
				defer cancel()
				c.listenForever(ctx)
			case <-c.closed:
				return
			case <-ctx.Done():
				return
			}
		}()
	}

	return nil
}

// Close closes the all the HTTP connections to the server.
func (c *StreamableHTTP) Close() error {
	c.closeOnce.Do(func() {
		// Cancel all in-flight requests
		c.closeTransport()

		// Protocol version 2026-07-28 removed protocol-level sessions, so
		// there is nothing to terminate and DELETE is not part of the
		// transport any more.
		sessionId := c.sessionID.Load().(string)
		if sessionId != "" && !c.isModern() {
			c.sessionID.Store("")
			// notify server session closed
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.target, nil)
			if err != nil {
				c.logger.Error("failed to create close request", "err", err)
				return
			}
			req.Header.Set(HeaderKeySessionID, sessionId)
			// Set protocol version header if negotiated
			if v := c.protocolVersion.Load(); v != nil {
				if version, ok := v.(string); ok && version != "" {
					req.Header.Set(HeaderKeyProtocolVersion, version)
				}
			}

			// Set custom Host header if provided
			if c.host != "" {
				req.Host = c.host
			}
			res, err := c.httpClient.Do(req)
			if err != nil {
				c.logger.Error("failed to send close request", "err", err)
				return
			}
			res.Body.Close()
		}
	})
	return nil
}

// SetProtocolVersion sets the negotiated protocol version for this connection.
func (c *StreamableHTTP) SetProtocolVersion(version string) {
	c.protocolVersion.Store(version)
}

// markConnected records what a successful initialize or server/discover
// response establishes: the initialization gate, and the session a GET
// stream follows (session, the ID the response's headers left in place). A
// response carrying a JSON-RPC error establishes nothing: a server predating
// server/discover answers the probe with one, and the gate must stay shut
// so the caller can fall back to the initialize handshake; a refused
// initialize leaves the transport as it was, and gives back the session ID
// its headers carried (sessionID), which SendRequest took before the body.
func (c *StreamableHTTP) markConnected(request JSONRPCRequest, response *JSONRPCResponse, sessionID, session string) {
	switch request.Method {
	case string(mcp.MethodInitialize), string(mcp.MethodServerDiscover):
	default:
		return
	}
	if response.Error != nil {
		if sessionID != "" {
			c.sessionID.CompareAndSwap(sessionID, "")
		}
		return
	}
	c.initializedOnce.Do(func() {
		close(c.initialized)
	})
	c.markSessionStarted(request, response, session)
}

// currentSession returns the session ID to send with a request, and whether
// the session has ended.
func (c *StreamableHTTP) currentSession() (string, bool) {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	sessionID, _ := c.sessionID.Load().(string)
	return sessionID, c.sessionEnded != nil
}

// endSession records that the server no longer knows sessionID: the caller
// has to start a new session.
func (c *StreamableHTTP) endSession(sessionID string) {
	if sessionID == "" {
		return
	}
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	if c.sessionID.CompareAndSwap(sessionID, "") && c.sessionEnded == nil {
		c.sessionEnded = make(chan struct{})
	}
}

// startsConnection reports whether request starts a new connection, and so
// may go out after the session has ended: initialize, or server/discover on
// 2026-07-28, which has no session.
func (c *StreamableHTTP) startsConnection(request JSONRPCRequest) bool {
	return request.Method == string(mcp.MethodInitialize) ||
		request.Method == string(mcp.MethodServerDiscover) && c.isModern()
}

// markSessionStarted records that a request that starts a new connection
// succeeded, leaving session as the session ID: requests go out again, and a
// GET stream connected on another session reconnects on this one. If the
// session ID has changed since, as when this session has ended already, it
// does nothing.
func (c *StreamableHTTP) markSessionStarted(request JSONRPCRequest, response *JSONRPCResponse, session string) {
	if !c.startsConnection(request) || response.Error != nil {
		return
	}
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	if current, _ := c.sessionID.Load().(string); current != session {
		return
	}
	if c.sessionEnded != nil {
		close(c.sessionEnded)
		c.sessionEnded = nil
	}
	if c.listenCancel != nil && c.listenSession != session {
		c.listenCancel()
	}
}

// listenOn waits while the session has ended, then records cancel as the way
// to end the GET stream's next connection, and returns the session that
// connection uses. It returns false if ctx ends first.
func (c *StreamableHTTP) listenOn(ctx context.Context, cancel context.CancelFunc) (string, bool) {
	for {
		c.sessionMu.Lock()
		ended := c.sessionEnded
		if ended == nil {
			c.listenSession, _ = c.sessionID.Load().(string)
			c.listenCancel = cancel
			session := c.listenSession
			c.sessionMu.Unlock()
			return session, true
		}
		c.sessionMu.Unlock()
		select {
		case <-ended:
		case <-ctx.Done():
			return "", false
		}
	}
}

// listenDone forgets the GET stream's connection once it is over.
func (c *StreamableHTTP) listenDone() {
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	c.listenSession = ""
	c.listenCancel = nil
}

// negotiatedProtocolVersion returns the protocol version in effect, or "" when
// none has been negotiated yet.
func (c *StreamableHTTP) negotiatedProtocolVersion() string {
	if v := c.protocolVersion.Load(); v != nil {
		if version, ok := v.(string); ok {
			return version
		}
	}
	return ""
}

// isModern reports whether this connection uses the stateless protocol core
// introduced in 2026-07-28, where the transport carries no session state.
func (c *StreamableHTTP) isModern() bool {
	return mcp.IsModernProtocol(c.negotiatedProtocolVersion())
}

// RequiresLegacyProtocol reports whether this transport has been configured in
// a way that only protocol versions before 2026-07-28 can satisfy.
//
// Continuous listening depends on the standalone GET stream, which that
// revision removed, so a transport using it must stay on a legacy version.
func (c *StreamableHTTP) RequiresLegacyProtocol() bool {
	return c.getListeningEnabled
}

// SendRequest sends a JSON-RPC request to the server and waits for a response.
// Returns the raw JSON response message or an error if the request fails.
func (c *StreamableHTTP) SendRequest(
	ctx context.Context,
	request JSONRPCRequest,
) (*JSONRPCResponse, error) {
	// Marshal request
	requestBody, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	ctx, cancel := c.contextAwareOfClientClose(ctx)

	// After the session has ended, only the requests that start a new
	// connection go out.
	resp, err := c.sendHTTP(ctx, http.MethodPost, bytes.NewReader(requestBody), "application/json, text/event-stream", request.Header, c.startsConnection(request))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	// Only proceed if we have a valid response.
	if resp == nil {
		cancel()
		return nil, errors.New("failed to send request: no response received")
	}
	// Cancel the context before closing the body. On HTTP/2, Close() blocks in a
	// select on cs.donec (stream cleanup) or cs.ctx.Done() (context cancellation).
	// If cc.wmu is contended, cs.donec may never close, making ctx.Done() the only
	// exit path. Canceling first guarantees Close() returns promptly.
	defer func() { cancel(); resp.Body.Close() }()

	// Check if we got an error response
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		if resp.StatusCode == http.StatusUnauthorized || insufficientScope(resp, c.oauthHandler) {
			return nil, authorizationError(resp, c.oauthHandler, c.oauthRecipient)
		}

		// A JSON-RPC error in the body is the answer, whatever the status, if
		// it answers this request. Any other JSON body, a gateway's or a
		// framework's error object, is no answer: the status is reported.
		var errResponse JSONRPCResponse
		body, _ := io.ReadAll(resp.Body)
		if isJSONRPCErrorBody(body) {
			if err := json.Unmarshal(body, &errResponse); err == nil && errResponse.Error != nil && answers(request, &errResponse) == nil {
				return &errResponse, nil
			}
		}
		// Per the MCP spec's backwards compatibility section: a 4xx to an
		// initialize POST (404, 405) whose body is not a JSON-RPC error means
		// the server likely only supports the legacy HTTP+SSE transport.
		if request.Method == string(mcp.MethodInitialize) && resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return nil, ErrLegacySSEServer
		}
		return nil, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, body)
	}

	// The session ID of an initialize response is in its headers, so it is
	// taken before the body, which a server may stream and hold: a GET
	// stream reconnecting meanwhile follows it. A refused initialize gives
	// it back (markConnected).
	sessionID := ""
	if request.Method == string(mcp.MethodInitialize) {
		if sessionID = resp.Header.Get(HeaderKeySessionID); sessionID != "" {
			c.sessionID.Store(sessionID)
		}
	}
	// The session ID this response leaves in place, for markSessionStarted:
	// read now, since the session may end while the body is still coming.
	session, _ := c.sessionID.Load().(string)
	// An initialize that gets no answer of its own gives back the session ID
	// its headers carried, as a refused one does.
	unanswered := func(err error) (*JSONRPCResponse, error) {
		if sessionID != "" {
			c.sessionID.CompareAndSwap(sessionID, "")
		}
		return nil, err
	}

	// Handle different response types
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	switch mediaType {
	case "application/json":
		// Single response
		var response JSONRPCResponse
		if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
			return unanswered(fmt.Errorf("failed to decode response: %w", err))
		}
		if err := answers(request, &response); err != nil {
			return unanswered(err)
		}

		c.markConnected(request, &response, sessionID, session)
		return &response, nil

	case "text/event-stream":
		// Server is using SSE for streaming responses
		sseResponse, err := c.handleSSEResponse(ctx, resp.Body, &request.ID)
		if err != nil || sseResponse == nil {
			return unanswered(err)
		}
		c.markConnected(request, sseResponse, sessionID, session)
		return sseResponse, nil

	default:
		return nil, fmt.Errorf("unexpected content type: %s", resp.Header.Get("Content-Type"))
	}
}

// sendHTTP sends an HTTP request to the server. Once the session has ended,
// it refuses to, unless startsSession is set.
func (c *StreamableHTTP) sendHTTP(
	ctx context.Context,
	method string,
	body io.Reader,
	acceptType string,
	header http.Header,
	startsSession bool,
) (resp *http.Response, err error) {
	sessionID, ended := c.currentSession()
	if ended && !startsSession {
		return nil, ErrSessionTerminated
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, method, c.target, body)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// request headers; cloned because the transport headers below must not
	// leak into (or race on) the caller's map
	if header != nil {
		req.Header = header.Clone()
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", acceptType)
	// Protocol version 2026-07-28 retired the session header: a modern client
	// neither sends nor stores one (SEP-2567).
	sentSession := ""
	if sessionID != "" && !c.isModern() {
		sentSession = sessionID
		req.Header.Set(HeaderKeySessionID, sessionID)
	}
	// The negotiated version fills in the header on a request without one;
	// a request that carries its era's version keeps it.
	if req.Header.Get(HeaderKeyProtocolVersion) == "" {
		if v := c.protocolVersion.Load(); v != nil {
			if version, ok := v.(string); ok && version != "" {
				req.Header.Set(HeaderKeyProtocolVersion, version)
			}
		}
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	// Set custom Host header if provided
	if c.host != "" {
		req.Host = c.host
	}

	// Add OAuth authorization if configured
	if c.oauthHandler != nil {
		authHeader, err := c.oauthHandler.authorizationHeader(ctx, c.oauthRecipient)
		if err != nil {
			// If we get an authorization error, return a specific error that can be handled by the client
			if errors.Is(err, ErrOAuthAuthorizationRequired) {
				return nil, &OAuthAuthorizationRequiredError{
					Handler: c.oauthHandler,
					AuthorizationRequiredError: AuthorizationRequiredError{
						ResourceMetadataURL: "", // No response available in this code path
					},
				}
			}
			return nil, fmt.Errorf("failed to get authorization header: %w", err)
		}
		req.Header.Set("Authorization", authHeader)
	}

	if c.headerFunc != nil {
		for k, v := range c.headerFunc(ctx) {
			req.Header.Set(k, v)
		}
	}

	// Send request
	resp, err = c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	// Before protocol version 2026-07-28 a 404 means the server no longer
	// knows the session. On 2026-07-28 there are no sessions: a 404 is a
	// method the server does not implement, and its JSON-RPC body is the
	// answer, which the caller reads like any other error status.
	if resp.StatusCode == http.StatusNotFound && !c.isModern() {
		// A 404 for the GET stream may only mean that the server doesn't
		// offer it, so listenForever decides.
		if method != http.MethodGet {
			c.endSession(sentSession)
		}
		// A 404 to initialize is answered by its body, which the caller
		// reads: a JSON-RPC error, or the sign of a legacy HTTP+SSE server.
		if startsSession {
			return resp, nil
		}
		resp.Body.Close()
		return nil, ErrSessionTerminated
	}

	return resp, nil
}

// SendNotification sends a JSON-RPC notification to the server without expecting a response.
func (c *StreamableHTTP) SendNotification(ctx context.Context, notification mcp.JSONRPCNotification) error {
	// Marshal request
	requestBody, err := json.Marshal(notification)
	if err != nil {
		return fmt.Errorf("failed to marshal notification: %w", err)
	}

	// Create HTTP request
	ctx, cancel := c.contextAwareOfClientClose(ctx)

	resp, err := c.sendHTTP(ctx, http.MethodPost, bytes.NewReader(requestBody), "application/json, text/event-stream", nil, false)
	if err != nil {
		cancel()
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer func() { cancel(); resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusOK, resp.StatusCode == http.StatusAccepted, resp.StatusCode == http.StatusNoContent:
		return nil
	case resp.StatusCode == http.StatusUnauthorized, insufficientScope(resp, c.oauthHandler):
		return authorizationError(resp, c.oauthHandler, c.oauthRecipient)
	default:
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf(
			"notification failed with status %d: %s",
			resp.StatusCode,
			body,
		)
	}
}

// SetNotificationHandler sets the handler for incoming JSON-RPC notifications.
func (c *StreamableHTTP) SetNotificationHandler(handler func(mcp.JSONRPCNotification)) {
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	c.notificationHandler = handler
}

// SetRequestHandler sets the handler for incoming requests from the server.
func (c *StreamableHTTP) SetRequestHandler(handler RequestHandler) {
	c.requestMu.Lock()
	defer c.requestMu.Unlock()
	c.requestHandler = handler
}

// GetSessionId returns the current StreamableHTTP session ID.
func (c *StreamableHTTP) GetSessionId() string {
	return c.sessionID.Load().(string)
}

// GetOAuthHandler returns the OAuth handler if configured
func (c *StreamableHTTP) GetOAuthHandler() *OAuthHandler {
	return c.oauthHandler
}

// IsOAuthEnabled returns true if OAuth is enabled
func (c *StreamableHTTP) IsOAuthEnabled() bool {
	return c.oauthHandler != nil
}

func (c *StreamableHTTP) contextAwareOfClientClose(ctx context.Context) (context.Context, context.CancelFunc) {
	newCtx, cancel := context.WithCancel(ctx)
	// Close cancels the request without a goroutine to watch for it, and
	// the request's end lets go of the watch.
	stop := context.AfterFunc(c.closing, cancel)
	return newCtx, func() {
		stop()
		cancel()
	}
}

// answers says whether response is request's answer: it carries the request's
// id, or a null one on an error, the server saying it could not read the id
// (JSON-RPC 2.0, section 5).
func answers(request JSONRPCRequest, response *JSONRPCResponse) error {
	if response.ID.IsNil() {
		if response.Error == nil {
			return fmt.Errorf("response should contain RPC id: %v", response)
		}
		return nil
	}
	if response.ID.String() != request.ID.String() {
		got, _ := json.Marshal(response.ID)
		want, _ := json.Marshal(request.ID)
		return fmt.Errorf("the response answers request %s, not %s", got, want)
	}
	return nil
}

// isJSONRPCErrorBody says whether body is a JSON-RPC error message: version
// "2.0" and an error with a numeric code and a string message. Member names
// are matched exactly, where json.Unmarshal into a struct ignores case, so a
// body that only resembles one is not taken for an answer.
func isJSONRPCErrorBody(body []byte) bool {
	var msg map[string]json.RawMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		return false
	}
	var version string
	if err := json.Unmarshal(msg["jsonrpc"], &version); err != nil || version != mcp.JSONRPC_VERSION {
		return false
	}
	var errObj map[string]json.RawMessage
	if err := json.Unmarshal(msg["error"], &errObj); err != nil {
		return false
	}
	var code *int
	if err := json.Unmarshal(errObj["code"], &code); err != nil || code == nil {
		return false
	}
	var message *string
	if err := json.Unmarshal(errObj["message"], &message); err != nil || message == nil {
		return false
	}
	return true
}
