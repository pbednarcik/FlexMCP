package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"

	"github.com/mark3labs/mcp-go/internal/jsonrpc"
	"github.com/mark3labs/mcp-go/mcp"
)

// StreamableHTTPOption defines a function type for configuring StreamableHTTPServer
type StreamableHTTPOption func(*StreamableHTTPServer)

// WithEndpointPath sets the endpoint path for the server.
// The default is "/mcp".
// It's only works for `Start` method. When used as a http.Handler, it has no effect.
func WithEndpointPath(endpointPath string) StreamableHTTPOption {
	return func(s *StreamableHTTPServer) {
		// Normalize the endpoint path to ensure it starts with a slash and doesn't end with one
		normalizedPath := "/" + strings.Trim(endpointPath, "/")
		s.endpointPath = normalizedPath
	}
}

// WithDisableLocalhostProtection disables the automatic DNS rebinding
// protection of the streamable HTTP server.
//
// By default, requests arriving over a loopback connection (127.0.0.1,
// [::1]) whose Host header is not a localhost value, or whose Origin header
// is present and not a localhost origin, are rejected with 403 Forbidden.
// This protects local MCP servers against DNS rebinding attacks, where a
// malicious website rebinds its own domain to 127.0.0.1 to make a victim's
// browser issue requests against a local server. The protection applies
// regardless of whether the server listens on localhost specifically or on
// 0.0.0.0, and never affects requests arriving via non-loopback addresses.
// When WithStreamableHTTPCORS names allowed origins, that list judges Origin
// instead, and the Host check stays.
//
// Disable it only if you understand the security implications, for example
// when a reverse proxy on the same host forwards requests via localhost
// while preserving the original Host header. In that case, prefer
// configuring the proxy to rewrite the Host header to localhost instead.
//
// See https://modelcontextprotocol.io/specification/2025-11-25/basic/security_best_practices#local-mcp-server-compromise
func WithDisableLocalhostProtection(disable bool) StreamableHTTPOption {
	return func(s *StreamableHTTPServer) {
		s.disableLocalhostProtection = disable
	}
}

// WithHTTPContextFunc sets a function that will be called to customise the context
// to the server using the incoming request.
// This can be used to inject context values from headers, for example.
func WithHTTPContextFunc(fn HTTPContextFunc) StreamableHTTPOption {
	return func(s *StreamableHTTPServer) {
		s.contextFunc = fn
	}
}

// WithStreamableHTTPServer sets the HTTP server instance for StreamableHTTPServer.
// NOTE: When providing a custom HTTP server, you must handle routing yourself
// If routing is not set up, the server will start but won't handle any MCP requests.
func WithStreamableHTTPServer(srv *http.Server) StreamableHTTPOption {
	return func(s *StreamableHTTPServer) {
		s.httpServer = srv
	}
}

// WithStreamableHTTPLogger sets the structured logger for transport-level
// events emitted by the HTTP server (panics in goroutines, SSE event-write
// errors, and so on). It is renamed from WithLogger so that
// server-level structured logging — see WithLogger — can carry that name on
// the MCPServer. A nil logger falls back to slog.Default().
func WithStreamableHTTPLogger(logger *slog.Logger) StreamableHTTPOption {
	return func(s *StreamableHTTPServer) {
		if logger == nil {
			s.logger = slog.Default()
			return
		}
		s.logger = logger
	}
}

// WithTLSCert sets the TLS certificate and key files for HTTPS support.
// Both certFile and keyFile must be provided to enable TLS.
func WithTLSCert(certFile, keyFile string) StreamableHTTPOption {
	return func(s *StreamableHTTPServer) {
		s.tlsCertFile = certFile
		s.tlsKeyFile = keyFile
	}
}

// WithProtectedResourceMetadata configures the StreamableHTTPServer to serve
// OAuth 2.0 Protected Resource Metadata (RFC 9728) at the well-known endpoint
// derived from the configured Resource (see ProtectedResourceMetadataPath).
//
// The metadata is served both when the server is started via Start (registered
// on the internal mux) and when the server is used directly as an
// http.Handler via ServeHTTP. To mount the metadata endpoint manually on a
// custom router, use NewProtectedResourceMetadataHandler instead.
func WithProtectedResourceMetadata(config ProtectedResourceMetadataConfig) StreamableHTTPOption {
	return func(s *StreamableHTTPServer) {
		cfg := config
		s.protectedResourceMetadata = &cfg
		s.protectedResourceMetadataPath = ProtectedResourceMetadataPath(cfg.Resource)
		s.protectedResourceMetadataHandler = NewProtectedResourceMetadataHandler(cfg)
	}
}

// WithStreamableHTTPCORS configures Cross-Origin Resource Sharing for the
// Streamable HTTP server.
//
// CORS handling is opt-in: callers must specify at least one allowed origin
// via WithCORSAllowedOrigins for any Access-Control-* headers to be emitted.
// When enabled, a request whose Origin is outside the allow list is refused
// with 403 Forbidden on every verb, preflight (OPTIONS) requests from an
// allowed origin are answered directly by the server, and simple
// cross-origin responses are decorated with the configured Allow-Origin,
// Allow-Credentials, Expose-Headers and Vary headers. A request with no
// Origin is served as before.
//
// Example:
//
//	srv := server.NewStreamableHTTPServer(mcpServer,
//	    server.WithEndpointPath("/mcp"),
//	    server.WithStreamableHTTPCORS(
//	        server.WithCORSAllowedOrigins("https://my-ai-app.com"),
//	        server.WithCORSAllowCredentials(),
//	    ),
//	)
func WithStreamableHTTPCORS(opts ...CORSOption) StreamableHTTPOption {
	return func(s *StreamableHTTPServer) {
		if s.corsConfig == nil {
			s.corsConfig = &CORSConfig{}
		}
		for _, opt := range opts {
			if opt != nil {
				opt(s.corsConfig)
			}
		}
	}
}

// StreamableHTTPServer implements a Streamable-http based MCP server.
// It communicates with clients over HTTP protocol, supporting both direct HTTP responses, and SSE streams.
// https://modelcontextprotocol.io/specification/2025-03-26/basic/transports#streamable-http
//
// Usage:
//
//	server := NewStreamableHTTPServer(mcpServer)
//	server.Start(":8080") // The final url for client is http://xxxx:8080/mcp by default
//
// or the server itself can be used as a http.Handler, which is convenient to
// integrate with existing http servers, or advanced usage:
//
//	handler := NewStreamableHTTPServer(mcpServer)
//	http.Handle("/streamable-http", handler)
//	http.ListenAndServe(":8080", nil)
//
// The server keeps no sessions. Protocol version 2026-07-28 removed them
// (SEP-2567), and a legacy client is served the same way: its initialize is
// answered for real with no session ID, every later request is served
// whatever Mcp-Session-Id it carries, and GET and DELETE answer 405. Every
// request runs on its own ephemeral session, so no HTTP client is registered
// with the MCPServer and none is reachable by ID.
type StreamableHTTPServer struct {
	server *MCPServer

	httpServer *http.Server
	mu         sync.RWMutex

	endpointPath string
	contextFunc  HTTPContextFunc
	logger       *slog.Logger

	// disableLocalhostProtection, when true, turns off the automatic DNS
	// rebinding protection applied to requests arriving over loopback
	// connections. See WithDisableLocalhostProtection.
	disableLocalhostProtection bool

	tlsCertFile string
	tlsKeyFile  string

	// protectedResourceMetadata, when non-nil, is served as RFC 9728 OAuth
	// 2.0 Protected Resource Metadata. The well-known path is derived from
	// the configured Resource via ProtectedResourceMetadataPath.
	protectedResourceMetadata        *ProtectedResourceMetadataConfig
	protectedResourceMetadataPath    string
	protectedResourceMetadataHandler http.Handler

	// corsConfig, when non-nil and with at least one allowed origin, makes
	// the server emit CORS headers and answer preflight requests. See
	// WithStreamableHTTPCORS.
	corsConfig *CORSConfig

	// protocolVersions restricts the protocol versions advertised through
	// server/discover. Empty means every version this SDK implements.
	protocolVersions []string

	// shutdown is cancelled by Shutdown, which is how an open
	// subscriptions/listen stream learns to end: net/http's Shutdown waits
	// for in-flight requests, and a listen stream never ends on its own.
	shutdown   context.Context
	endListens context.CancelFunc
}

// NewStreamableHTTPServer creates a new streamable-http server instance
func NewStreamableHTTPServer(server *MCPServer, opts ...StreamableHTTPOption) *StreamableHTTPServer {
	shutdown, endListens := context.WithCancel(context.Background())
	s := &StreamableHTTPServer{
		server:       server,
		endpointPath: "/mcp",
		logger:       slog.Default(),
		shutdown:     shutdown,
		endListens:   endListens,
	}

	// Apply all options
	for _, opt := range opts {
		opt(s)
	}

	return s
}

// ServeHTTP implements the http.Handler interface.
//
// When WithProtectedResourceMetadata has been configured, requests to the
// derived /.well-known/oauth-protected-resource path are dispatched to the
// metadata handler so that the same StreamableHTTPServer can be mounted as a
// single http.Handler while still exposing OAuth discovery metadata.
//
// When WithStreamableHTTPCORS has been configured, CORS preflight (OPTIONS)
// requests are answered directly and simple cross-origin responses are
// decorated with the configured Access-Control-* headers before dispatch.
//
// Requests arriving over a loopback connection with a non-localhost Host
// header are rejected with 403 Forbidden to protect against DNS rebinding
// attacks, unless WithDisableLocalhostProtection is set.
//
// ServeHTTP is the conventional net/http entry point; for non-net/http HTTP
// frameworks (fasthttp, fiber, etc.), see Handle.
func (s *StreamableHTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Origin is validated on every request: by the CORS allow list when one
	// is configured, by the loopback check otherwise.
	corsEnabled := s.corsConfig.enabled()
	if !s.disableLocalhostProtection && rejectDNSRebinding(w, r, !corsEnabled) {
		return
	}
	if corsEnabled {
		if s.corsConfig.rejectDisallowedOrigin(w, r) {
			return
		}
		if s.corsConfig.handlePreflight(w, r) {
			return
		}
		s.corsConfig.applySimple(w, r)
	}
	if s.protectedResourceMetadataHandler != nil && r.URL.Path == s.protectedResourceMetadataPath {
		s.protectedResourceMetadataHandler.ServeHTTP(w, r)
		return
	}

	// Read the request body up-front so the transport-agnostic core never
	// needs to keep an io.Reader alive across SSE upgrades. Body errors are
	// surfaced by the per-method handlers below for parity with the previous
	// behavior (PARSE_ERROR JSON-RPC reply for POST, ignored for GET/DELETE).
	var body []byte
	var bodyErr error
	if r.Method == http.MethodPost && r.Body != nil {
		body, bodyErr = io.ReadAll(r.Body)
	}

	hr := &HTTPRequest{
		Method:   r.Method,
		URL:      r.URL,
		Header:   r.Header,
		Body:     body,
		Context:  r.Context(),
		original: r,
	}
	hw := newHTTPResponseWriterAdapter(w)

	if bodyErr != nil && r.Method == http.MethodPost {
		s.writeJSONRPCError(hw, nil, mcp.PARSE_ERROR, fmt.Sprintf("read request body error: %v", bodyErr))
		return
	}

	switch r.Method {
	case http.MethodPost:
		s.handlePost(hw, hr)
	case http.MethodGet:
		s.handleGet(hw, hr)
	case http.MethodDelete:
		s.handleDelete(hw, hr)
	case http.MethodHead:
		s.handleHead(hw, hr)
	default:
		http.NotFound(w, r)
	}
}

// Start begins serving the http server on the specified address and path
// (endpointPath). like:
//
//	s.Start(":8080")
func (s *StreamableHTTPServer) Start(addr string) error {
	s.mu.Lock()
	if s.httpServer == nil {
		mux := http.NewServeMux()
		mux.Handle(s.endpointPath, s)
		if s.protectedResourceMetadataHandler != nil && s.protectedResourceMetadataPath != s.endpointPath {
			mux.Handle(s.protectedResourceMetadataPath, s.protectedResourceMetadataHandler)
		}
		s.httpServer = &http.Server{
			Addr:    addr,
			Handler: mux,
		}
	} else {
		if s.httpServer.Addr == "" {
			s.httpServer.Addr = addr
		} else if s.httpServer.Addr != addr {
			return fmt.Errorf("conflicting listen address: WithStreamableHTTPServer(%q) vs Start(%q)", s.httpServer.Addr, addr)
		}
	}
	srv := s.httpServer
	s.mu.Unlock()

	if s.tlsCertFile != "" || s.tlsKeyFile != "" {
		if s.tlsCertFile == "" || s.tlsKeyFile == "" {
			return fmt.Errorf("both TLS cert and key must be provided")
		}
		if _, err := os.Stat(s.tlsCertFile); err != nil {
			return fmt.Errorf("failed to find TLS certificate file: %w", err)
		}
		if _, err := os.Stat(s.tlsKeyFile); err != nil {
			return fmt.Errorf("failed to find TLS key file: %w", err)
		}
		return srv.ListenAndServeTLS(s.tlsCertFile, s.tlsKeyFile)
	}

	return srv.ListenAndServe()
}

// Shutdown stops the HTTP server, if this server started one or was given
// one; as a bare http.Handler there is nothing to stop. Open
// subscriptions/listen streams are ended first, in either case: net/http's
// Shutdown waits for in-flight requests, and a listen stream never ends on
// its own. Other in-flight requests finish, as net/http arranges.
func (s *StreamableHTTPServer) Shutdown(ctx context.Context) error {
	if s.endListens != nil {
		s.endListens()
	}
	s.mu.RLock()
	srv := s.httpServer
	s.mu.RUnlock()
	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

// --- internal methods ---

func (s *StreamableHTTPServer) handlePost(w HTTPResponseWriter, r *HTTPRequest) {
	// post request carry request/notification message

	// Check content type
	contentType := r.header().Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "application/json" {
		writeHTTPError(w, "Invalid content type: must be 'application/json'", http.StatusBadRequest)
		return
	}

	// Body has already been buffered by the caller (ServeHTTP or Handle).
	rawData := r.Body
	// The envelope: a client's response carries an id and no method.
	var jsonMessage struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  json.RawMessage `json:"error,omitempty"`
		Method mcp.MCPMethod   `json:"method"`
	}
	if err := json.Unmarshal(rawData, &jsonMessage); err != nil {
		if json.Valid(rawData) {
			id, _ := requestIDValue(jsonMessage.ID)
			s.writeJSONRPCError(w, id, mcp.INVALID_REQUEST, "request body is not a JSON-RPC message")
			return
		}
		s.writeJSONRPCError(w, nil, mcp.PARSE_ERROR, "request body is not valid json")
		return
	}

	method := jsonrpc.MethodOf(string(jsonMessage.Method), rawData)
	isInitializeRequest := jsonMessage.Method == mcp.MethodInitialize

	requestID, validID := requestIDValue(jsonMessage.ID)
	isResponse := !method.Present && (len(jsonMessage.Result) > 0 || len(jsonMessage.Error) > 0)
	if !validID && !isResponse {
		s.writeJSONRPCError(w, nil, mcp.INVALID_REQUEST, "invalid request id")
		return
	}

	// Decide which protocol era this message belongs to. Both eras run through
	// the same stateless path below; the era only chooses the version check
	// here, the modern header validation, and the HTTP status of an error.
	era := detectRequestEra(r.header(), rawData)

	// On 2026-07-28 the server sends no requests, so a response from the client
	// answers none: it is input the server cannot accept (rule 4), refused with
	// an error that names no request.
	if era.modern && isResponse {
		s.writeJSONRPCError(w, nil, mcp.INVALID_REQUEST, "a client sends no responses on this protocol revision")
		return
	}
	// A message with no method that is no response is an invalid request.
	// Earlier revisions still take an id alone as an empty response, the way a
	// lax client answers a ping.
	if !method.Present && !isResponse && (len(jsonMessage.ID) == 0 || era.modern) {
		s.writeJSONRPCError(w, requestID, mcp.INVALID_REQUEST, "Invalid request: no method")
		return
	}
	// A method that is not a string makes no request, nor a response.
	if method.Present && !method.IsString {
		s.writeJSONRPCError(w, requestID, mcp.INVALID_REQUEST, "Invalid request: method must be a string")
		return
	}
	// A notification is never answered, so one the server cannot read is
	// refused here, as input it cannot accept (rule 4), not accepted with 202.
	if method.Present && len(jsonMessage.ID) == 0 && json.Unmarshal(rawData, new(mcp.JSONRPCNotification)) != nil {
		s.writeJSONRPCError(w, nil, mcp.INVALID_REQUEST, "notification could not be read")
		return
	}

	// The header is only required after initialize, which negotiates the
	// version itself. Responses posted by the client are checked as well.
	if !era.modern && !isInitializeRequest && s.rejectUnsupportedProtocolVersion(w, era.headerVersion, requestID) {
		return
	}

	// A client's answer to a server ping, or any empty response, is accepted
	// without being dispatched.
	isEmptyResponse := !method.Present && jsonMessage.ID != nil &&
		(isJSONEmpty(jsonMessage.Result) && isJSONEmpty(jsonMessage.Error))
	isPingResponse := !method.Present && jsonMessage.ID != nil &&
		isExplicitEmptyObject(jsonMessage.Result) && len(bytes.TrimSpace(jsonMessage.Error)) == 0

	if isPingResponse {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if isEmptyResponse {
		// Per MCP spec (Streamable HTTP transport, rule 4): the server MUST return
		// 202 Accepted for any accepted JSON-RPC response or notification, regardless
		// of whether result is {} or omitted. HTTP 200 with no body has no defined
		// meaning in this transport for POST requests carrying responses.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Every request runs on its own ephemeral session: the server keeps none
	// in either era, so a legacy client's Mcp-Session-Id is ignored and its
	// initialize mints none.
	listening := false
	if era.modern {
		if !s.validateModernRequest(w, r, era, requestID) {
			return
		}
		// A subscriptions/listen stream is where a modern client receives
		// the notifications the server broadcasts, such as
		// notifications/tools/list_changed.
		listening = jsonMessage.Method == mcp.MethodSubscriptionsListen && requestID != nil
	}
	session := newStreamableHttpSession()

	// Broadcasts only reach sessions the server knows of, and a modern
	// request's session is known nowhere else. A listen stream's session is
	// made reachable while the request runs; it receives nothing until
	// subscriptions/listen records the types the client asked for.
	if listening {
		session.serveSubscriptionStream()
		s.server.registerListenSession(session)
		defer s.server.unregisterListenSession(session)
	}

	// Set the client context before handling the message
	ctx := s.server.WithContext(r.ctx(), session)
	ctx = WithSupportedProtocolVersions(ctx, s.supportedProtocolVersions())
	if s.contextFunc != nil {
		ctx = s.contextFunc(ctx, r.asHTTPRequest())
	}

	// handle potential notifications
	mu := sync.Mutex{}
	upgradedHeader := false
	// upgrade commits the response as SSE once; every later message rides it.
	upgrade := func() {
		if !upgradedHeader {
			startSSEResponse(w)
			upgradedHeader = true
		}
	}
	ctx = context.WithValue(ctx, requestHeader, r.header())

	// A listen stream ends when Shutdown says so; see Shutdown.
	if listening && s.shutdown != nil {
		var stop context.CancelFunc
		ctx, stop = context.WithCancel(ctx)
		defer stop()
		defer context.AfterFunc(s.shutdown, stop)()
	}

	// SSE upgrades require a streaming-capable response writer; if the
	// underlying transport can't stream, we still attempt the request but
	// any notifications will be buffered and flushed at the end as a single
	// application/json response (the upgrade simply won't fire).
	canStream := w.CanStream()

	// The forwarder starts with the first notification the request sends:
	// most requests send none, and a goroutine per request costs a thread
	// wakeup each. done stops it; forwarderExited lets
	// the response path wait until it can no longer be holding an
	// undelivered notification.
	var done, forwarderExited chan struct{}
	session.forwardWith(func(notifications <-chan mcp.JSONRPCNotification) {
		done = make(chan struct{})
		forwarderExited = make(chan struct{})
		go func() {
			defer close(forwarderExited)
			defer func() {
				if r := recover(); r != nil {
					s.logger.Error("panic in notification forwarder", "panic", r)
				}
			}()
			for {
				select {
				case nt := <-notifications:
					if listening {
						nt = tagSubscription(nt, requestID)
					}
					func() {
						mu.Lock()
						defer mu.Unlock()
						if !canStream {
							// Without streaming we can't deliver notifications mid-flight;
							// they will be dropped on the floor here. The final response
							// path remains the buffered JSON reply below.
							return
						}
						defer w.Flush()

						upgrade()
						err := writeSSEEvent(w, nt)
						if err != nil {
							s.logger.Error("Failed to write SSE event", "err", err)
							return
						}
					}()
				case <-done:
					return
				case <-ctx.Done():
					return
				}
			}
		}()
	})

	response := s.server.HandleMessage(ctx, rawData)
	if listening {
		// Stop broadcasts before draining what is already queued.
		s.server.unregisterListenSession(session)
	}

	// Stop the forwarder, if one started, and wait for it before touching the
	// response: it may hold a notification it has not written yet, and every
	// notification the handler sent must reach the client, in order, ahead of
	// the response. From here on this goroutine is the only writer.
	if session.stopForwarding() {
		close(done)
		<-forwarderExited
	}

	if response == nil {
		if !upgradedHeader {
			w.WriteHeader(http.StatusAccepted)
		}
		return
	}

	mu.Lock()

	notifications := session.pending()
drainLoop:
	for {
		select {
		case nt := <-notifications:
			if !canStream {
				continue
			}
			if listening {
				nt = tagSubscription(nt, requestID)
			}
			upgrade()
			if err := writeSSEEvent(w, nt); err != nil {
				s.logger.Error("Failed to write SSE event during drain", "err", err)
			}
			w.Flush()
		default:
			break drainLoop
		}
	}

	mu.Unlock()
	if ctx.Err() != nil {
		// The connection is gone; there is nobody to answer.
		return
	}
	// If client-server communication already upgraded to SSE stream
	// Also check upgradedHeader: a notification during HandleMessage processing
	// may have already written SSE headers on this response, so we must continue
	// in SSE mode to avoid writing JSON on top of SSE data.
	if (session.upgradeToSSE.Load() && canStream) || upgradedHeader {
		upgrade()
		if err := writeSSEEvent(w, response); err != nil {
			s.logger.Error("Failed to write final SSE response event", "err", err)
		}
		w.Flush()
	} else {
		w.Header().Set("Content-Type", "application/json")
		// Protocol version 2026-07-28 maps JSON-RPC error codes onto HTTP
		// status codes, so that intermediaries can act on failures without
		// parsing the body.
		status := http.StatusOK
		if era.modern {
			if errResponse, ok := response.(mcp.JSONRPCError); ok {
				status = httpStatusForJSONRPCError(errResponse.Error.Code)
			}
		}
		w.WriteHeader(status)
		err := json.NewEncoder(w).Encode(response)
		if err != nil {
			s.logger.Error("Failed to write response", "err", err)
		}
	}
}

// handleGet answers 405 in both eras: protocol version 2026-07-28 removed the
// standalone GET stream, and a legacy client is not served one either.
func (s *StreamableHTTPServer) handleGet(w HTTPResponseWriter, r *HTTPRequest) {
	s.methodNotAllowed(w, r, "GET is not supported: this server opens no standalone stream")
}

// handleDelete answers 405 in both eras: protocol version 2026-07-28 removed
// protocol-level sessions, and the server keeps none for a legacy client
// either, so there is nothing for a DELETE to terminate.
func (s *StreamableHTTPServer) handleDelete(w HTTPResponseWriter, r *HTTPRequest) {
	s.methodNotAllowed(w, r, "DELETE is not supported: this server keeps no sessions")
}

// handleHead answers 405 as GET does: the endpoint serves POST only, and a
// HEAD that answered 200 would promise a GET the endpoint refuses.
func (s *StreamableHTTPServer) handleHead(w HTTPResponseWriter, r *HTTPRequest) {
	s.methodNotAllowed(w, r, "HEAD is not supported: this endpoint serves POST only")
}

// methodNotAllowed answers a verb the endpoint does not serve in either era,
// through the one path every verb shares: a protocol version this transport
// does not serve is reported first, with the JSON-RPC body a POST would get,
// then 405 with Allow: POST and reason as the body.
func (s *StreamableHTTPServer) methodNotAllowed(w HTTPResponseWriter, r *HTTPRequest, reason string) {
	if s.rejectUnsupportedProtocolVersion(w, r.header().Get(mcp.HeaderProtocolVersion), nil) {
		return
	}
	// RFC 9110 section 15.5.6 requires an Allow header on 405 responses.
	w.Header().Set("Allow", http.MethodPost)
	writeHTTPError(w, reason, http.StatusMethodNotAllowed)
}

func writeSSEEvent(w io.Writer, data any) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal data: %w", err)
	}
	_, err = fmt.Fprintf(w, "event: message\ndata: %s\n\n", jsonData)
	if err != nil {
		return fmt.Errorf("failed to write SSE event: %w", err)
	}
	return nil
}

// startSSEResponse commits the response as an SSE stream: the headers every
// stream carries, including the one that asks proxies not to buffer events
// (Transports.md row 23), then the 200 that sends them.
func startSSEResponse(w HTTPResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// writeJSONRPCError writes a JSON-RPC error response with the given error details.
func (s *StreamableHTTPServer) writeJSONRPCError(
	w HTTPResponseWriter,
	id any,
	code int,
	message string,
) {
	writeJSONRPCError(w, id, code, message, func(err error) {
		s.logger.Error("Failed to write JSONRPCError", "err", err)
	})
}

// --- session ---

// streamableHttpSession is the session of one request: it lives for the
// request, in both eras, and has no ID. A broadcast reaches it only while it
// serves a subscriptions/listen stream.
type streamableHttpSession struct {
	clientInfoStore // provides Get/SetClientInfo and Get/SetClientCapabilities via method promotion

	// notifyMu guards the channel and the forwarder's start; it is never held
	// across a send or a write.
	notifyMu            sync.Mutex
	notificationChannel chan mcp.JSONRPCNotification         // server -> client notifications, made on first use
	startForwarder      func(<-chan mcp.JSONRPCNotification) // armed by handlePost while it handles the request
	forwarding          bool                                 // the forwarder was started
	upgradeToSSE        atomic.Bool

	// Whether the session serves a subscriptions/listen stream, and the
	// notification types that stream opted in to.
	subscriptionMu     sync.RWMutex
	listening          bool
	subscriptionFilter mcp.SubscriptionFilter
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

// forwardWith arms start to run once, with the channel, when the first
// notification is queued; at once when one is queued already.
func (s *streamableHttpSession) forwardWith(start func(<-chan mcp.JSONRPCNotification)) {
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
		s.startForwarder(s.notificationChannel)
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

// isJSONEmpty reports whether the provided JSON value is "empty":
//   - null
//   - empty object: {}
//   - empty array: []
//
// It also treats nil/whitespace-only input as empty.
// It does NOT treat 0, false, "" or non-empty composites as empty.
func isJSONEmpty(data json.RawMessage) bool {
	if len(data) == 0 {
		return true
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return true
	}

	switch trimmed[0] {
	case '{':
		if len(trimmed) == 2 && trimmed[1] == '}' {
			return true
		}
		for i := 1; i < len(trimmed); i++ {
			if !unicode.IsSpace(rune(trimmed[i])) {
				return trimmed[i] == '}'
			}
		}
	case '[':
		if len(trimmed) == 2 && trimmed[1] == ']' {
			return true
		}
		for i := 1; i < len(trimmed); i++ {
			if !unicode.IsSpace(rune(trimmed[i])) {
				return trimmed[i] == ']'
			}
		}

	case '"': // treat "" as not empty
		return false

	case 'n': // null
		return len(trimmed) == 4 &&
			trimmed[1] == 'u' &&
			trimmed[2] == 'l' &&
			trimmed[3] == 'l'
	}
	return false
}

// isExplicitEmptyObject reports whether data is a JSON object literal with no fields.
func isExplicitEmptyObject(data json.RawMessage) bool {
	if len(data) == 0 {
		return false
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return false
	}

	return len(obj) == 0
}
