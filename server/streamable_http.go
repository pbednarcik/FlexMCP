package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

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
	// allowedHosts are the hosts WithAllowedHosts named.
	allowedHosts []string

	// maxRequestBodyBytes bounds a POST body read by ServeHTTP; zero or less
	// is no bound. See WithMaxRequestBodyBytes.
	maxRequestBodyBytes int64
	// requestBodyTimeout bounds the wait for a POST body; zero or less is
	// no bound. See WithRequestBodyTimeout.
	requestBodyTimeout time.Duration
	// readHeaderTimeout and idleTimeout go to the http.Server Start builds.
	readHeaderTimeout time.Duration
	idleTimeout       time.Duration
	// writeTimeout bounds each write to a response; zero or less is no
	// bound. See WithWriteTimeout.
	writeTimeout time.Duration
	// keepAlive is the interval of a listen stream's keep-alive comments;
	// zero or less is none. See WithKeepAlive.
	keepAlive time.Duration

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
	// supportedVersions is supportedProtocolVersions, boxed once for the
	// context of every request.
	supportedVersions any

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
		server:              server,
		endpointPath:        "/mcp",
		logger:              slog.Default(),
		maxRequestBodyBytes: DefaultMaxRequestBodyBytes,
		requestBodyTimeout:  DefaultRequestBodyTimeout,
		readHeaderTimeout:   DefaultReadHeaderTimeout,
		idleTimeout:         DefaultIdleTimeout,
		writeTimeout:        DefaultWriteTimeout,
		shutdown:            shutdown,
		endListens:          endListens,
	}

	// Apply all options
	for _, opt := range opts {
		opt(s)
	}
	s.supportedVersions = s.supportedProtocolVersions()

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
// Requests arriving over a loopback, private or link-local connection with
// an Origin header that does not name the server, or over a loopback
// connection with a Host header that does not, are rejected with 403
// Forbidden to protect against DNS rebinding attacks, unless
// WithDisableLocalhostProtection is set; see WithAllowedHosts.
//
// ServeHTTP is the conventional net/http entry point; for non-net/http HTTP
// frameworks (fasthttp, fiber, etc.), see Handle.
func (s *StreamableHTTPServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Origin is validated on every request: by the CORS allow list when one
	// is configured, by the loopback check otherwise.
	corsEnabled := s.corsConfig.enabled()
	if !s.disableLocalhostProtection && rejectDNSRebinding(w, r, !corsEnabled, s.allowedHosts) {
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
		// The bound on a body's size does not bound how long reading it
		// holds this goroutine and its connection: a body still arriving
		// when the timeout is up is refused. The deadline is lifted once the
		// body is read, before anything is written: a listen stream stays
		// open for as long as the client wants, and net/http's own read,
		// which tells a closed connection, must not end with it. After a
		// failed read it stays: net/http drains what is left of a small body
		// before answering, and that drain would wait for bytes that never
		// come.
		var deadline readDeadliner
		if s.requestBodyTimeout > 0 && !serverHasReadTimeout(r) {
			if d, ok := readDeadlinerOf(w); ok && d.SetReadDeadline(time.Now().Add(s.requestBodyTimeout)) == nil {
				deadline = d
			}
		}
		switch limit := s.maxRequestBodyBytes; {
		case limit <= 0:
			body, bodyErr = io.ReadAll(r.Body)
		case r.ContentLength > limit:
			// Refused unread: net/http closes the connection rather than
			// drain the body.
			bodyErr = errBodyTooLarge
		default:
			body, bodyErr = readBounded(r.Body, limit)
		}
		if deadline != nil && bodyErr == nil {
			_ = deadline.SetReadDeadline(time.Time{})
		}
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
		if errors.Is(bodyErr, errBodyTooLarge) {
			writeJSONRPCErrorStatus(hw, http.StatusRequestEntityTooLarge, nil, mcp.INVALID_REQUEST,
				fmt.Sprintf("request body exceeds %d bytes", s.maxRequestBodyBytes), func(err error) {
					s.logger.Error("Failed to write JSONRPCError", "err", err)
				})
			return
		}
		// The cause names addresses and middleware: it stays in the log.
		s.logger.Debug("request body not read", "err", bodyErr)
		s.writeJSONRPCError(hw, nil, mcp.PARSE_ERROR, "request body could not be read")
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

// readDeadliner is the deadline a ResponseWriter puts on reading its
// request, as net/http's own implement it.
type readDeadliner interface {
	SetReadDeadline(time.Time) error
}

// readDeadlinerOf finds the readDeadliner behind w, through the Unwrap chain
// http.ResponseController would follow; unlike the controller it allocates
// nothing, on a writer without deadlines too.
func readDeadlinerOf(w http.ResponseWriter) (readDeadliner, bool) {
	return deadlinerOf[readDeadliner](w)
}

// writeDeadlinerOf is readDeadlinerOf for the write deadline.
func writeDeadlinerOf(w http.ResponseWriter) (writeDeadliner, bool) {
	return deadlinerOf[writeDeadliner](w)
}

// deadlinerOf finds the D behind w, through the Unwrap chain.
func deadlinerOf[D any](w http.ResponseWriter) (D, bool) {
	for {
		if d, ok := w.(D); ok {
			return d, true
		}
		u, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			var none D
			return none, false
		}
		w = u.Unwrap()
	}
}

// httpServerOf is the http.Server serving the request whose context ctx
// is: nil for a request net/http did not start, as through Handle.
func httpServerOf(ctx context.Context) *http.Server {
	srv, _ := ctx.Value(http.ServerContextKey).(*http.Server)
	return srv
}

// serverHasReadTimeout reports whether the http.Server serving r bounds the
// read of the whole request itself. Its ReadTimeout is the deployer's bound
// on the body: the body deadline neither replaces it with a looser one nor
// lifts it.
func serverHasReadTimeout(r *http.Request) bool {
	srv := httpServerOf(r.Context())
	return srv != nil && srv.ReadTimeout > 0
}

// serverHasWriteTimeout reports whether the http.Server serving the request
// whose context ctx is bounds the whole response itself. Its WriteTimeout
// is the deployer's bound on the writes: the write deadline, which is set
// on the same connection, neither replaces it with a looser one nor lifts
// it.
func serverHasWriteTimeout(ctx context.Context) bool {
	srv := httpServerOf(ctx)
	return srv != nil && srv.WriteTimeout > 0
}

// errBodyTooLarge is the read error of a POST body over the bound.
var errBodyTooLarge = errors.New("request body too large")

// readBounded reads r to its end, up to limit bytes; a longer r is
// errBodyTooLarge. It is io.ReadAll with the bound checked as the buffer
// grows, and without a reader of its own, which would allocate on every
// request. Content-Length is not trusted: middleware may replace the body.
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	// The reader is never offered more than the bound and one byte, the one
	// that proves the body too long, whatever the buffer's capacity. A bound
	// at the top of int's range has no room for that byte: a body that
	// fills it is too long as well.
	most := int(min(limit, math.MaxInt-1)) + 1
	b := make([]byte, 0, min(512, most))
	for {
		if len(b) == cap(b) {
			b = slices.Grow(b, min(cap(b), most-len(b)))
		}
		n, err := r.Read(b[len(b):min(cap(b), most)])
		b = b[:len(b)+n]
		if int64(len(b)) > limit || len(b) == most {
			return nil, errBodyTooLarge
		}
		if err != nil {
			if err == io.EOF {
				return b, nil
			}
			return b, err
		}
	}
}

// Start begins serving the http server on the specified address and path
// (endpointPath). like:
//
//	s.Start(":8080")
//
// A server is not restarted: Start after Shutdown returns
// http.ErrServerClosed, whether or not Start had run before, since the
// listen streams Shutdown ended stay ended.
func (s *StreamableHTTPServer) Start(addr string) error {
	s.mu.Lock()
	if s.shutdown != nil && s.shutdown.Err() != nil {
		s.mu.Unlock()
		return http.ErrServerClosed
	}
	if s.httpServer == nil {
		mux := http.NewServeMux()
		mux.Handle(s.endpointPath, s)
		if s.protectedResourceMetadataHandler != nil && s.protectedResourceMetadataPath != s.endpointPath {
			mux.Handle(s.protectedResourceMetadataPath, s.protectedResourceMetadataHandler)
		}
		s.httpServer = &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: max(s.readHeaderTimeout, 0),
			IdleTimeout:       max(s.idleTimeout, 0),
		}
	} else {
		if s.httpServer.Addr == "" {
			s.httpServer.Addr = addr
		} else if s.httpServer.Addr != addr {
			s.mu.Unlock()
			return fmt.Errorf("conflicting listen address: WithStreamableHTTPServer(%q) vs Start(%q)", s.httpServer.Addr, addr)
		}
	}
	srv := s.httpServer
	s.mu.Unlock()

	if s.tlsCertFile != "" || s.tlsKeyFile != "" {
		if s.tlsCertFile == "" || s.tlsKeyFile == "" {
			return errors.New("both TLS cert and key must be provided")
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

// writeDeadliner is the deadline a response writer puts on its writes, as
// net/http's own and the adapter implement it.
type writeDeadliner interface {
	SetWriteDeadline(time.Time) error
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
