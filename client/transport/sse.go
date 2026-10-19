package transport

import (
	"bufio"
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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mark3labs/mcp-go/internal/jsonrpc"
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

// readSSE continuously reads the SSE stream and processes events.
// It runs until the connection is closed or an error occurs. attempt is the
// Start attempt that opened the stream.
func (c *SSE) readSSE(reader io.ReadCloser, endpointChan chan struct{}, endpointOnce *sync.Once, attempt uint64) {
	defer reader.Close()

	br := bufio.NewReader(reader)
	// The line, the event's name and its data are read into buffers kept
	// from one event to the next; one a message grew past maxRetainedLine is
	// let go.
	var line, event, data []byte

	for {
		// when close or start's ctx cancel, the reader will be closed
		// and the for loop will break.
		var err error
		line, err = readLine(br, line)
		if err != nil {
			// Close() cancels the stream context, which surfaces here as a read
			// error; firing the handler would reconnect a transport the caller
			// just shut down.
			if c.closed.Load() {
				return
			}
			if err == io.EOF {
				// Process any pending event before exit
				if len(data) > 0 {
					c.handleSSEEvent(sseEventName(event), data, endpointChan, endpointOnce, attempt)
				}
			}
			// A stream an earlier attempt opened ends nothing a later one
			// serves. Under lifecycleMu, as Start publishes its success: the
			// end is reported to the handler if Start succeeded first, and
			// by Start's error otherwise. The pending requests are failed
			// under it too: a retry's Start waits for it, so none of the
			// retry's requests is among them.
			c.lifecycleMu.Lock()
			current := c.attempt.Load() == attempt
			var endStream context.CancelFunc
			if current {
				c.ended.Store(true)
				endStream = c.cancelSSEStream
				c.failPending()
			}
			started := current && c.started.Load()
			c.lifecycleMu.Unlock()
			if !current {
				return
			}
			// The POSTs sent on this stream end with it.
			if endStream != nil {
				endStream()
			}
			if !started {
				return
			}
			c.connectionLostMu.RLock()
			handler := c.onConnectionLost
			c.connectionLostMu.RUnlock()
			if handler != nil {
				// Notify that the connection will be closed due to an error
				handler(err)
			} else {
				c.logger.Error("SSE stream error", "err", err)
			}
			return
		}

		if len(line) == 0 {
			// Empty line means end of event
			if len(data) > 0 {
				c.handleSSEEvent(sseEventName(event), data, endpointChan, endpointOnce, attempt)
				event, data = resetEvent(event, data)
			}
			continue
		}

		if after, ok := bytes.CutPrefix(line, []byte("event:")); ok {
			event = append(event[:0], bytes.TrimSpace(after)...)
		} else if after, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = appendSSEDataBytes(data, after)
		}
		if cap(line) > maxRetainedLine {
			line = nil
		}
	}
}

// resetEvent empties an event's name and data for the next event, keeping
// their storage unless a message grew it past maxRetainedLine.
func resetEvent(event, data []byte) ([]byte, []byte) {
	event, data = event[:0], data[:0]
	if cap(event) > maxRetainedLine {
		event = nil
	}
	if cap(data) > maxRetainedLine {
		data = nil
	}
	return event, data
}

// sseEventName is the name an event is handled by: "message" for one the
// stream named none, as SSE specifies.
func sseEventName(event []byte) string {
	switch string(event) {
	case "", "message":
		return "message"
	case "endpoint":
		return "endpoint"
	}
	return string(event)
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

// appendSSEData joins the value of an SSE "data:" field onto any data already
// accumulated for the current event. Per the Server-Sent Events specification,
// a single event may carry multiple "data:" lines and they must be
// concatenated with a newline. The previous implementation overwrote the
// buffer on every line, silently truncating multi-line payloads to their final
// line and corrupting JSON-RPC messages that span more than one data line.
//
// Only a single optional leading space after "data:" is removed, as the spec
// requires; any other leading or trailing whitespace in the value is
// significant and preserved.
func appendSSEData(existing, line string) string {
	value := strings.TrimPrefix(line, " ")
	if existing == "" {
		return value
	}
	return existing + "\n" + value
}

// appendSSEDataBytes is appendSSEData appending into existing's storage.
func appendSSEDataBytes(existing, line []byte) []byte {
	value := bytes.TrimPrefix(line, []byte(" "))
	if len(existing) > 0 {
		existing = append(existing, '\n')
	}
	return append(existing, value...)
}

// handleSSEEvent processes SSE events based on their type.
// Handles 'endpoint' events for connection setup and 'message' events for JSON-RPC communication.
// attempt is the Start attempt whose stream carried the event: once a later
// attempt has begun, the event is dropped, so a superseded reader neither
// replaces the endpoint nor dispatches what it had already read.
func (c *SSE) handleSSEEvent(event string, data []byte, endpointChan chan struct{}, endpointOnce *sync.Once, attempt uint64) {
	if c.attempt.Load() != attempt {
		return
	}
	switch event {
	case "endpoint":
		endpoint, err := c.baseURL.Parse(string(data))
		if err != nil {
			c.logger.Error("Error parsing endpoint URL", "err", err)
			return
		}
		// The endpoint keeps the connection's origin, scheme included: an
		// https connection must not send its requests, and their bearer
		// token, to an http endpoint on the same host.
		if endpoint.Host != c.baseURL.Host || !strings.EqualFold(endpoint.Scheme, c.baseURL.Scheme) ||
			endpoint.User.String() != c.baseURL.User.String() {
			c.logger.Error("Endpoint origin does not match connection origin")
			return
		}
		// Checked again under mu: a later attempt is begun before its reader
		// runs, so a reader of an earlier one that gets here after the later
		// reader published its endpoint sees that it is superseded.
		c.mu.Lock()
		if c.attempt.Load() != attempt {
			c.mu.Unlock()
			return
		}
		c.endpoint, c.endpointURL = endpoint, endpoint.String()
		c.mu.Unlock()
		// The SSE spec sends a single endpoint event, but a proxy or a
		// re-broadcasting server can emit it more than once. Closing the
		// channel twice panics, so guard the close with the attempt's
		// sync.Once.
		endpointOnce.Do(func() {
			close(endpointChan)
		})
	case "message":
		// Every decode below reads these bytes, and none keeps them: they are
		// the reader's, reused for the next event.
		encoded := data
		// A response's result and error are read here too, so a response is
		// decoded once.
		var envelope struct {
			JSONRPC jsonrpcVersion  `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  jsonrpc.Method  `json:"method"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(encoded, &envelope); err != nil {
			c.logger.Error("Error unmarshaling message", "err", err)
			return
		}
		var id mcp.RequestId
		readableID := len(envelope.ID) > 0 && id.UnmarshalJSON(envelope.ID) == nil && !id.IsNil()

		// A message with a method member is a request or a notification,
		// whatever its value; only one without is a response.
		switch {
		case envelope.Method.Present && !envelope.Method.IsString:
			answerID := mcp.NewRequestId(nil)
			if readableID {
				answerID = id
			}
			c.answer(NewJSONRPCErrorResponse(answerID, mcp.INVALID_REQUEST, "Invalid request: method must be a string", nil))
			return
		case envelope.Method.Present && len(envelope.ID) == 0:
			var notification mcp.JSONRPCNotification
			if err := json.Unmarshal(encoded, &notification); err != nil {
				return
			}
			// The handler runs outside the lock, so one that sets the handler
			// does not deadlock the stream.
			c.notifyMu.RLock()
			handler := c.onNotification
			c.notifyMu.RUnlock()
			if handler != nil {
				handler(notification)
			}
			return
		case envelope.Method.Present && !readableID:
			// A request id is a string or an integer and never null.
			c.answer(NewJSONRPCErrorResponse(mcp.NewRequestId(nil), mcp.INVALID_REQUEST, "Invalid request id", nil))
			return
		case envelope.Method.Present && !envelope.JSONRPC.is20:
			c.answer(invalidVersionResponse(id))
			return
		case envelope.Method.Name == string(mcp.MethodPing):
			// The receiver of a ping MUST answer it on the earlier revisions.
			c.answer(NewJSONRPCResultResponse(id, json.RawMessage(`{}`)))
			return
		case envelope.Method.Present:
			// A request from the server is never a response, even when it
			// carries the id of one the client is waiting on: request ids are
			// per sender. This transport serves no other server request.
			c.logger.Debug("Refusing a server request: the SSE transport serves none", "method", envelope.Method.Name)
			c.answer(NewJSONRPCErrorResponse(id, mcp.METHOD_NOT_FOUND, "Method not found: the SSE transport serves no server requests", nil))
			return
		case !readableID:
			// A response without an id, an error to a request whose id the
			// server could not read, answers no request this shared stream
			// can name.
			c.logger.Error("Dropping a response without a readable id", "id", string(envelope.ID))
			return
		}

		// A JSON-RPC 2.0 response is built from the envelope; one with any
		// other jsonrpc member is decoded whole, as it always was, so its
		// member is kept, or the response dropped when it is not a string.
		baseMessage := JSONRPCResponse{JSONRPC: mcp.JSONRPC_VERSION, ID: id, Result: envelope.Result}
		if !envelope.JSONRPC.is20 {
			baseMessage = JSONRPCResponse{}
			if err := json.Unmarshal(encoded, &baseMessage); err != nil {
				c.logger.Error("Error unmarshaling response", "err", err)
				return
			}
		} else if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
			var details mcp.JSONRPCErrorDetails
			if err := json.Unmarshal(envelope.Error, &details); err != nil {
				c.logger.Error("Error unmarshaling response", "err", err)
				return
			}
			baseMessage.Error = &details
		}

		idKey := responseKeyOf(id)

		// Hold the write lock while sending: Close() also takes the write
		// lock to close pending response channels, so the send can never
		// race a close and panic with "send on closed channel".
		c.mu.Lock()
		ch, exists := c.responses[idKey]
		if exists {
			ch <- &baseMessage
			delete(c.responses, idKey)
		}
		c.mu.Unlock()
	}
}

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

// answer sends the client's response to a server request. It runs on the
// stream's reader, as the Streamable HTTP transport's answers do, bounded by
// the time a server request may take there, and Close abandons it.
func (c *SSE) answer(response *JSONRPCResponse) {
	body, err := json.Marshal(response)
	if err != nil {
		c.logger.Error("Error marshaling response", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(c.answers, 30*time.Second)
	defer cancel()
	if err := c.post(ctx, body, "response"); err != nil {
		c.logger.Error("Error answering a server request", "err", err)
	}
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
