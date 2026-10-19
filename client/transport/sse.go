package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// SSE implements the transport layer of the MCP protocol using Server-Sent Events (SSE).
// It maintains a persistent HTTP connection to receive server-pushed events
// while sending requests over regular HTTP POST calls. The client handles
// automatic reconnection and message routing between requests and responses.
type SSE struct {
	baseURL  *url.URL
	endpoint *url.URL
	// endpointURL is endpoint's String, made once when it is set.
	endpointURL    string
	httpClient     *http.Client
	responses      map[responseKey]chan *JSONRPCResponse
	mu             sync.RWMutex
	onNotification func(mcp.JSONRPCNotification)
	notifyMu       sync.RWMutex
	headers        map[string]string
	headerFunc     HTTPHeaderFunc
	host           string
	logger         *slog.Logger

	// startMu protects startDone. Concurrent Start calls share one active
	// handshake and may cancel independently; Start and Close must not
	// overlap.
	startMu   sync.Mutex
	startDone chan struct{}

	// lifecycleMu serializes the closed transition with canceler publication.
	// Start publishes the stream canceler under it and Close flips closed and
	// reads the canceler under it, so an in-flight start cannot escape
	// shutdown with a live request context.
	lifecycleMu sync.Mutex

	started atomic.Bool
	closed  atomic.Bool
	// attempt numbers the Start attempts, and ended is set when the current
	// attempt's stream ends without Close: requests then fail at once, as
	// nothing is left to read their answers.
	attempt atomic.Uint64
	ended   atomic.Bool
	// answers bounds the answers to server requests; Close cancels it.
	answers         context.Context
	stopAnswers     context.CancelFunc
	cancelSSEStream context.CancelFunc
	// stream is the current attempt's stream context: it ends when the
	// stream does, or Close runs, and every POST ends with it. Set under mu,
	// beside endpoint.
	stream           context.Context
	protocolVersion  atomic.Value // string
	onConnectionLost func(error)
	connectionLostMu sync.RWMutex
	// testHookStreamEnding, nil outside tests, runs when a reader whose
	// stream ended has found the transport open and has not yet taken
	// lifecycleMu to settle the end.
	testHookStreamEnding func()

	endpointTimeout time.Duration
	responseTimeout time.Duration

	// OAuth support
	oauthHandler *OAuthHandler
	// oauthRecipient is the base URL the transport set on oauthHandler: a
	// request carries a token only while the handler still targets it.
	oauthRecipient string
}

// ClientOption configures an SSE transport client.
type ClientOption func(*SSE)

// WithSSELogger sets a custom structured logger for the SSE client.
// A nil logger falls back to slog.Default().
func WithSSELogger(logger *slog.Logger) ClientOption {
	return func(sc *SSE) {
		if logger == nil {
			sc.logger = slog.Default()
			return
		}
		sc.logger = logger
	}
}

// WithHeaders sets static headers for SSE HTTP requests.
func WithHeaders(headers map[string]string) ClientOption {
	return func(sc *SSE) {
		sc.headers = headers
	}
}

// WithHeaderFunc sets a function that returns headers for SSE HTTP requests.
func WithHeaderFunc(headerFunc HTTPHeaderFunc) ClientOption {
	return func(sc *SSE) {
		sc.headerFunc = headerFunc
	}
}

// WithHTTPClient sets a custom HTTP client for the SSE transport.
func WithHTTPClient(httpClient *http.Client) ClientOption {
	return func(sc *SSE) {
		sc.httpClient = httpClient
	}
}

// WithOAuth enables OAuth authentication for the SSE transport.
func WithOAuth(config OAuthConfig) ClientOption {
	return func(sc *SSE) {
		sc.oauthHandler = NewOAuthHandler(config)
	}
}

// WithEndpointTimeout sets the maximum time to wait for the SSE endpoint to be
// received during Start(). Defaults to 30 seconds. If the context has a shorter
// deadline, the shorter value is used.
func WithEndpointTimeout(d time.Duration) ClientOption {
	return func(sc *SSE) {
		if d > 0 {
			sc.endpointTimeout = d
		}
	}
}

// WithResponseTimeout sets the maximum time to wait for an SSE response after
// sending a request. Defaults to 60 seconds. If the context has a shorter
// deadline, the shorter value is used.
func WithResponseTimeout(d time.Duration) ClientOption {
	return func(sc *SSE) {
		if d > 0 {
			sc.responseTimeout = d
		}
	}
}

// WithHTTPHost sets a custom Host header for the SSE client, enabling manual DNS resolution.
// This allows connecting to an IP address while sending a specific Host header to the server.
// For example, connecting to "http://192.168.1.100:8080/sse" but sending Host: "api.example.com"
func WithHTTPHost(host string) ClientOption {
	return func(sc *SSE) {
		sc.host = host
	}
}

// NewSSE creates a new SSE-based MCP client with the given base URL.
// Returns an error if the URL is invalid.
//
// The HTTP+SSE transport is Deprecated: new code uses NewStreamableHTTP.
func NewSSE(baseURL string, options ...ClientOption) (*SSE, error) {
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}

	smc := &SSE{
		baseURL:         parsedURL,
		httpClient:      &http.Client{},
		responses:       make(map[responseKey]chan *JSONRPCResponse),
		headers:         make(map[string]string),
		logger:          slog.Default(),
		endpointTimeout: 30 * time.Second,
		responseTimeout: 60 * time.Second,
	}
	smc.answers, smc.stopAnswers = context.WithCancel(context.Background())

	for _, opt := range options {
		opt(smc)
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

// Start initiates the SSE connection to the server and waits for the endpoint information.
// Returns an error if the connection fails or times out waiting for the endpoint.
func (c *SSE) Start(ctx context.Context) error {
	for {
		c.startMu.Lock()
		// Reject any start after Close before the started fast-path so a closed
		// transport reports ErrTransportClosed instead of nil.
		if c.closed.Load() {
			c.startMu.Unlock()
			return ErrTransportClosed
		}
		if c.started.Load() {
			c.startMu.Unlock()
			return nil
		}
		// A concurrent Start is already in flight: wait for it and honor
		// our own context instead of blocking on the mutex.
		if startDone := c.startDone; startDone != nil {
			c.startMu.Unlock()
			select {
			case <-startDone:
				if err := ctx.Err(); err != nil {
					return fmt.Errorf("context cancelled while waiting for concurrent start: %w", err)
				}
				continue
			case <-ctx.Done():
				return fmt.Errorf("context cancelled while waiting for concurrent start: %w", ctx.Err())
			}
		}

		// Become the owner of this handshake attempt.
		startDone := make(chan struct{})
		c.startDone = startDone
		c.startMu.Unlock()

		err := c.start(ctx)

		c.startMu.Lock()
		c.startDone = nil
		close(startDone)
		c.startMu.Unlock()
		return err
	}
}

// start performs one SSE connection and endpoint handshake attempt.
func (c *SSE) start(ctx context.Context) error {
	// The handshake state belongs to this Start attempt only. Delayed
	// endpoint events from a superseded reader (e.g. after a failed Start)
	// close only this attempt's channel and cannot corrupt a later retry.
	endpointChan := make(chan struct{})
	endpointOnce := &sync.Once{}

	// Publish the canceler under the same lock Close uses for the closed
	// transition. Close therefore either rejects this attempt or observes its
	// canceler.
	c.lifecycleMu.Lock()
	if c.closed.Load() {
		c.lifecycleMu.Unlock()
		return ErrTransportClosed
	}
	ctx, cancel := context.WithCancel(ctx)
	c.cancelSSEStream = cancel
	c.mu.Lock()
	c.stream = ctx
	c.mu.Unlock()
	attempt := c.attempt.Add(1)
	c.ended.Store(false)
	c.lifecycleMu.Unlock()

	startSucceeded := false
	defer func() {
		if !startSucceeded {
			cancel()
		}
	}()

	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL.String(), nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Set custom Host header if provided
	if c.host != "" {
		req.Host = c.host
	}

	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Connection", "keep-alive")

	if err := c.setHeaders(ctx, req); err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to SSE stream: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || insufficientScope(resp, c.oauthHandler) {
			return authorizationError(resp, c.oauthHandler, c.oauthRecipient)
		}
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	go c.readSSE(resp.Body, endpointChan, endpointOnce, attempt)

	// Wait for the endpoint to be received
	endpointTimeout := c.endpointTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		// If context deadline has already passed, return immediately
		if remaining <= 0 {
			cancel()
			return ctx.Err()
		}
		// Use the shorter of remaining time or default timeout
		if remaining < endpointTimeout {
			endpointTimeout = remaining
		}
	}

	timer := time.NewTimer(endpointTimeout)
	defer timer.Stop()

	select {
	case <-endpointChan:
		// Endpoint received, proceed
	case <-ctx.Done():
		return fmt.Errorf("context cancelled while waiting for endpoint: %w", ctx.Err())
	case <-timer.C:
		cancel()
		return fmt.Errorf("timeout waiting for endpoint after %v", endpointTimeout)
	}

	// Do not publish a successful start after a concurrent Close has won,
	// or on a stream that has already ended: its reader saw no started
	// transport to report the end to.
	c.lifecycleMu.Lock()
	if c.closed.Load() {
		c.lifecycleMu.Unlock()
		return ErrTransportClosed
	}
	if c.ended.Load() {
		c.lifecycleMu.Unlock()
		return errors.New("the SSE stream ended while starting")
	}
	c.started.Store(true)
	startSucceeded = true
	c.lifecycleMu.Unlock()
	return nil
}

// failPending fails the requests waiting on a stream that ended. Requests
// register under mu after checking ended, which is set first, so each is
// either failed here or refused.
func (c *SSE) failPending() {
	c.mu.Lock()
	for _, ch := range c.responses {
		close(ch)
	}
	c.responses = make(map[responseKey]chan *JSONRPCResponse)
	c.mu.Unlock()
}

// errStreamEnded is the error of a request sent after the stream ended.
var errStreamEnded = fmt.Errorf("the SSE stream has ended: %w", ErrTransportClosed)

// streamError is the error of a message sent, or cut short, once the
// transport has closed or its stream has ended; nil while the stream lives.
func (c *SSE) streamError() error {
	if c.closed.Load() {
		return errSSEClosed
	}
	if c.ended.Load() {
		return errStreamEnded
	}
	return nil
}

// closedError is an error that reads as its own text and is
// ErrTransportClosed to errors.Is.
type closedError string

func (e closedError) Error() string { return string(e) }

func (closedError) Unwrap() error { return ErrTransportClosed }

const (
	// errSSEClosed is the error of a message sent after Close.
	errSSEClosed closedError = "transport has been closed"
	// errConnectionClosed is the error of a request whose answer can no
	// longer arrive: Close ran, or the stream ended, while it waited.
	errConnectionClosed closedError = "connection has been closed"
)

// SetNotificationHandler sets the handler for incoming JSON-RPC notifications.
func (c *SSE) SetNotificationHandler(handler func(notification mcp.JSONRPCNotification)) {
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	c.onNotification = handler
}

// SetConnectionLostHandler sets the handler called when the SSE connection is lost.
func (c *SSE) SetConnectionLostHandler(handler func(error)) {
	c.connectionLostMu.Lock()
	defer c.connectionLostMu.Unlock()
	c.onConnectionLost = handler
}

// SendRequest sends a JSON-RPC request to the server and waits for a response.
// Returns the raw JSON response message or an error if the request fails.
func (c *SSE) SendRequest(
	ctx context.Context,
	request JSONRPCRequest,
) (*JSONRPCResponse, error) {
	if !c.started.Load() {
		return nil, errors.New("transport not started yet")
	}
	if c.closed.Load() {
		return nil, errSSEClosed
	}
	if c.ended.Load() {
		return nil, errStreamEnded
	}

	c.mu.RLock()
	endpoint, target, stream := c.endpoint, c.endpointURL, c.stream
	c.mu.RUnlock()

	if endpoint == nil {
		return nil, errors.New("endpoint not received")
	}

	// Marshal request
	requestBytes, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// The POST ends with its stream: a server that ends the stream, or a
	// Close, does not leave it waiting for the response headers.
	postCtx, cancelPost := context.WithCancel(ctx)
	defer cancelPost()
	if stream != nil {
		stop := context.AfterFunc(stream, cancelPost)
		defer stop()
	}

	// Create HTTP request
	req, err := http.NewRequestWithContext(postCtx, http.MethodPost, endpointTarget(endpoint, target), bytes.NewReader(requestBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// The request's own headers go first, so the version of its era is the
	// one sent; the transport fills in what the request leaves out, and its
	// static headers are set over them, as on Streamable HTTP.
	maps.Copy(req.Header, request.Header)
	req.Header.Set("Content-Type", "application/json")
	// The negotiated version fills in the header on a request without one;
	// a request that carries its era's version keeps it.
	if req.Header.Get(HeaderKeyProtocolVersion) == "" {
		if v := c.protocolVersion.Load(); v != nil {
			if version, ok := v.(string); ok && version != "" {
				req.Header.Set(HeaderKeyProtocolVersion, version)
			}
		}
	}
	if err := c.setHeaders(ctx, req); err != nil {
		return nil, err
	}

	// Set custom Host header if provided
	if c.host != "" {
		req.Host = c.host
	}

	idKey := responseKeyOf(request.ID)

	// Register response channel
	// Close sets closed before it fails the registered requests under mu, so
	// a request either registers in time to be failed or is refused here.
	responseChan := make(chan *JSONRPCResponse, 1)
	c.mu.Lock()
	if c.closed.Load() {
		c.mu.Unlock()
		return nil, errSSEClosed
	}
	if c.ended.Load() {
		c.mu.Unlock()
		return nil, errStreamEnded
	}
	c.responses[idKey] = responseChan
	c.mu.Unlock()
	deleteResponseChan := func() {
		c.mu.Lock()
		delete(c.responses, idKey)
		c.mu.Unlock()
	}

	// Send request
	resp, err := c.httpClient.Do(req)
	if err != nil {
		deleteResponseChan()
		if streamErr := c.streamError(); streamErr != nil {
			return nil, streamErr
		}
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	// Drain the body: its answer comes on the stream, so a 200 or 202 body is
	// read to its end and dropped, and only an error status keeps it, for
	// the error's text.
	var body []byte
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted {
		_, err = io.Copy(io.Discard, resp.Body)
	} else {
		body, err = io.ReadAll(resp.Body)
	}
	resp.Body.Close()

	if err != nil {
		deleteResponseChan()
		if streamErr := c.streamError(); streamErr != nil {
			return nil, streamErr
		}
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	// Check if we got an error response
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		deleteResponseChan()
		if resp.StatusCode == http.StatusUnauthorized || insufficientScope(resp, c.oauthHandler) {
			return nil, authorizationError(resp, c.oauthHandler, c.oauthRecipient)
		}
		return nil, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, body)
	}

	// Calculate response timeout
	responseTimeout := c.responseTimeout
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		// Check if context deadline has already passed
		if remaining <= 0 {
			deleteResponseChan()
			return nil, ctx.Err()
		}
		// Use the shorter of remaining time or default timeout
		if remaining < responseTimeout {
			responseTimeout = remaining
		}
	}

	timer := time.NewTimer(responseTimeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		deleteResponseChan()
		return nil, ctx.Err()
	case <-timer.C:
		// Timeout handling
		deleteResponseChan()
		return nil, fmt.Errorf("timeout waiting for SSE response after %v: %w", responseTimeout, context.DeadlineExceeded)
	case response, ok := <-responseChan:
		if ok {
			return response, nil
		}
		return nil, errConnectionClosed
	}
}

// Close shuts down the SSE client connection and cleans up any pending responses.
// Returns an error if the shutdown process fails.
func (c *SSE) Close() error {
	// Synchronize the closed transition with Start's canceler publication so
	// an in-flight start cannot escape shutdown with a live request context.
	c.lifecycleMu.Lock()
	if c.closed.Load() {
		c.lifecycleMu.Unlock()
		return nil // Already closed
	}
	c.closed.Store(true)
	cancelSSEStream := c.cancelSSEStream
	c.lifecycleMu.Unlock()

	if cancelSSEStream != nil {
		// It could stop the sse stream body, to quit the readSSE loop immediately
		// Also, it could quit start() immediately if not receiving the endpoint
		cancelSSEStream()
	}
	if c.stopAnswers != nil {
		c.stopAnswers()
	}

	// Clean up any pending responses
	c.mu.Lock()
	for _, ch := range c.responses {
		close(ch)
	}
	c.responses = make(map[responseKey]chan *JSONRPCResponse)
	c.mu.Unlock()

	return nil
}

// GetSessionId returns the session ID of the transport.
// Since SSE does not maintain a session ID, it returns an empty string.
func (c *SSE) GetSessionId() string {
	return ""
}

// SetProtocolVersion sets the negotiated protocol version for this connection.
func (c *SSE) SetProtocolVersion(version string) {
	c.protocolVersion.Store(version)
}

// SendNotification sends a JSON-RPC notification to the server without expecting a response.
func (c *SSE) SendNotification(ctx context.Context, notification mcp.JSONRPCNotification) error {
	notificationBytes, err := json.Marshal(notification)
	if err != nil {
		return fmt.Errorf("failed to marshal notification: %w", err)
	}
	return c.post(ctx, notificationBytes, "notification")
}

// post sends one message, a notification or a response, to the endpoint;
// what names it in errors.
func (c *SSE) post(ctx context.Context, body []byte, what string) error {
	if err := c.streamError(); err != nil {
		return err
	}

	c.mu.RLock()
	endpoint, target, stream := c.endpoint, c.endpointURL, c.stream
	c.mu.RUnlock()

	if endpoint == nil {
		return errors.New("endpoint not received")
	}

	// The POST ends with its stream, as a request's does.
	postCtx, cancelPost := context.WithCancel(ctx)
	defer cancelPost()
	if stream != nil {
		stop := context.AfterFunc(stream, cancelPost)
		defer stop()
	}

	req, err := http.NewRequestWithContext(
		postCtx,
		"POST",
		endpointTarget(endpoint, target),
		bytes.NewReader(body),
	)
	if err != nil {
		return fmt.Errorf("failed to create %s request: %w", what, err)
	}

	req.Header.Set("Content-Type", "application/json")
	// The negotiated version fills in the header on a request without one;
	// a request that carries its era's version keeps it.
	if req.Header.Get(HeaderKeyProtocolVersion) == "" {
		if v := c.protocolVersion.Load(); v != nil {
			if version, ok := v.(string); ok && version != "" {
				req.Header.Set(HeaderKeyProtocolVersion, version)
			}
		}
	}
	if err := c.setHeaders(ctx, req); err != nil {
		return err
	}

	// Set custom Host header if provided
	if c.host != "" {
		req.Host = c.host
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if streamErr := c.streamError(); streamErr != nil {
			return streamErr
		}
		return fmt.Errorf("failed to send %s: %w", what, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		if resp.StatusCode == http.StatusUnauthorized || insufficientScope(resp, c.oauthHandler) {
			return authorizationError(resp, c.oauthHandler, c.oauthRecipient)
		}

		// Handle other error responses
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf(
			"%s failed with status %d: %s",
			what,
			resp.StatusCode,
			body,
		)
	}

	return nil
}

// setHeaders sets the headers every request to the server carries, in
// Streamable HTTP's order: the static headers, then the OAuth
// Authorization, then headerFunc's, which win over both.
func (c *SSE) setHeaders(ctx context.Context, req *http.Request) error {
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if c.oauthHandler != nil {
		authHeader, err := c.oauthHandler.authorizationHeader(ctx, c.oauthRecipient)
		if err != nil {
			// An authorization error is returned as one the client can handle.
			if errors.Is(err, ErrOAuthAuthorizationRequired) {
				return &OAuthAuthorizationRequiredError{
					Handler: c.oauthHandler,
					AuthorizationRequiredError: AuthorizationRequiredError{
						ResourceMetadataURL: "", // No response available in this code path
					},
				}
			}
			return fmt.Errorf("failed to get authorization header: %w", err)
		}
		req.Header.Set("Authorization", authHeader)
	}
	if c.headerFunc != nil {
		for k, v := range c.headerFunc(ctx) {
			req.Header.Set(k, v)
		}
	}
	return nil
}

// endpointTarget is the URL a POST to endpoint goes to: target, the string
// made when the endpoint was set, or, for an endpoint set without it, its
// String.
func endpointTarget(endpoint *url.URL, target string) string {
	if target == "" {
		return endpoint.String()
	}
	return target
}

// GetEndpoint returns the current endpoint URL for the SSE connection.
func (c *SSE) GetEndpoint() *url.URL {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.endpoint
}

// GetBaseURL returns the base URL set in the SSE constructor.
func (c *SSE) GetBaseURL() *url.URL {
	return c.baseURL
}

// GetOAuthHandler returns the OAuth handler if configured
func (c *SSE) GetOAuthHandler() *OAuthHandler {
	return c.oauthHandler
}

// IsOAuthEnabled returns true if OAuth is enabled
func (c *SSE) IsOAuthEnabled() bool {
	return c.oauthHandler != nil
}
