package server

import (
	"log/slog"
	"net/http"
	"strings"
	"time"
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

// The timeouts of the http.Server Start builds, and of a POST body's read,
// when the options below do not set them. A server given through
// WithStreamableHTTPServer keeps its own timeouts.
const (
	DefaultReadHeaderTimeout  = 10 * time.Second
	DefaultIdleTimeout        = 2 * time.Minute
	DefaultRequestBodyTimeout = 30 * time.Second
	DefaultWriteTimeout       = 30 * time.Second
)

// WithWriteTimeout bounds each write to a response: a client that stops
// reading, with the response's SSE events or its body unsent, cannot hold
// the request, its goroutines, or Shutdown, which waits for requests. A
// write still unsent when the time is up fails, and nothing more is
// written for that request. The default is DefaultWriteTimeout; zero or
// less removes the bound.
//
// The bound is on a whole write, not on its progress: one large body, a
// resource read or a result of many megabytes, must reach a slow client
// within the time, or it needs a longer one. Through Handle with a writer
// of the caller's own, a listen stream whose client went away ends only
// when that writer's write fails, as net/http's does.
func WithWriteTimeout(d time.Duration) StreamableHTTPOption {
	return func(s *StreamableHTTPServer) {
		s.writeTimeout = d
	}
}

// WithKeepAlive has a subscriptions/listen stream carry an SSE comment line
// every d, a keep-alive the client ignores (Transports.md row 40), so a
// proxy or a load balancer with an idle timeout does not cut a quiet
// stream. Off by default; zero or less leaves it off.
func WithKeepAlive(d time.Duration) StreamableHTTPOption {
	return func(s *StreamableHTTPServer) {
		s.keepAlive = d
	}
}

// WithReadHeaderTimeout sets the http.Server's ReadHeaderTimeout for the
// server Start builds: how long a client may take to send a request's
// headers. The default is DefaultReadHeaderTimeout; zero or less leaves
// net/http's own, which is no timeout.
func WithReadHeaderTimeout(d time.Duration) StreamableHTTPOption {
	return func(s *StreamableHTTPServer) {
		s.readHeaderTimeout = d
	}
}

// WithIdleTimeout sets the http.Server's IdleTimeout for the server Start
// builds: how long a keep-alive connection may sit idle between requests.
// The default is DefaultIdleTimeout; zero or less leaves net/http's own,
// which is no timeout.
func WithIdleTimeout(d time.Duration) StreamableHTTPOption {
	return func(s *StreamableHTTPServer) {
		s.idleTimeout = d
	}
}

// WithRequestBodyTimeout bounds how long ServeHTTP waits for a POST body to
// arrive: a body that is still arriving when the time is up is refused with
// a JSON-RPC parse error, so a client sending one slowly cannot hold the
// handler and its connection. The deadline is lifted once the body is read,
// so it never ends a subscriptions/listen stream. An http.Server with a
// ReadTimeout of its own keeps it: that bounds the body already, and the
// deadline would replace it. The default is DefaultRequestBodyTimeout,
// enough for a body at the default size bound over an ordinary link; a
// server taking large bodies from slow clients raises it. Zero or less
// removes the bound.
func WithRequestBodyTimeout(d time.Duration) StreamableHTTPOption {
	return func(s *StreamableHTTPServer) {
		s.requestBodyTimeout = d
	}
}

// WithDisableLocalhostProtection disables the automatic DNS rebinding
// protection of the streamable HTTP server.
//
// By default, requests arriving over a loopback connection (127.0.0.1,
// [::1]), a private address (RFC 1918, 100.64.0.0/10 or an IPv6 unique
// local address) or a link-local address whose Origin header is present
// and does not name the server are rejected with 403 Forbidden, and so are
// those on a loopback address whose Host header does not. A header names
// the server when it is a loopback name, the address the connection
// arrived on, or a host WithAllowedHosts allows. This protects local and
// LAN MCP servers against DNS rebinding attacks, where a malicious website
// rebinds its own domain to the server's address to make a victim's
// browser issue requests against it; a browser sends Origin on every POST,
// so Origin catches the rebound request, while Host is left to the proxies
// and container networks that set it on a private address. The protection
// applies regardless of whether the server listens on one address or on
// 0.0.0.0, and never affects requests arriving on a public address. When
// WithStreamableHTTPCORS names allowed origins, that list judges Origin
// instead, and the loopback Host check stays.
//
// A browser page served from the server's own hostname on a private
// network names that host with WithAllowedHosts. Disable the protection
// only if you understand the security implications.
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

// WithAllowedHosts names hosts, besides loopback names and the connection's
// own address, that a request arriving on a loopback, private or link-local
// address may carry in its Origin header when no CORS allow list judges
// Origin, and in its Host header on a loopback address: the server's own
// hostnames, as a reverse proxy forwards them or a browser page served
// from the server names them. A host is compared without its port,
// case-insensitively and without a trailing dot; an IP literal as an
// address. See WithDisableLocalhostProtection.
//
// On a public address Origin is left to the CORS allow list, when one is
// configured: only a browser can be rebound, a browser's cross-origin POST
// is already stopped by the content type the server requires and by the
// absent preflight answer, and an allow list there would turn away every
// unlisted browser client.
func WithAllowedHosts(hosts ...string) StreamableHTTPOption {
	return func(s *StreamableHTTPServer) {
		s.allowedHosts = append(s.allowedHosts, hosts...)
	}
}

// DefaultMaxRequestBodyBytes is the bound WithMaxRequestBodyBytes puts on a
// POST body when the option is not given.
const DefaultMaxRequestBodyBytes = 4 << 20

// WithMaxRequestBodyBytes bounds the body of a POST request served through
// ServeHTTP: the server reads at most n bytes of it, and answers a larger
// one 413 Request Entity Too Large with a JSON-RPC error, so a client cannot
// make the server hold an arbitrary amount of memory. The default is
// DefaultMaxRequestBodyBytes; zero or less removes the bound. A request
// brought through Handle arrives with its body already read, so the caller
// bounds it there.
func WithMaxRequestBodyBytes(n int64) StreamableHTTPOption {
	return func(s *StreamableHTTPServer) {
		s.maxRequestBodyBytes = n
	}
}
