package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

func TestCORSConfig_DisabledByDefault(t *testing.T) {
	t.Parallel()

	var c *CORSConfig
	assert.False(t, c.enabled(), "nil config must be disabled")

	c = &CORSConfig{}
	assert.False(t, c.enabled(), "config without origins must be disabled")

	c = &CORSConfig{AllowedOrigins: []string{"https://example.com"}}
	assert.True(t, c.enabled(), "config with origins must be enabled")
}

func TestCORSConfig_ResolveOrigin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		cfg         CORSConfig
		origin      string
		wantAllowed string
	}{
		{
			name:        "exact match",
			cfg:         CORSConfig{AllowedOrigins: []string{"https://example.com"}},
			origin:      "https://example.com",
			wantAllowed: "https://example.com",
		},
		{
			name:        "no match returns empty",
			cfg:         CORSConfig{AllowedOrigins: []string{"https://example.com"}},
			origin:      "https://attacker.com",
			wantAllowed: "",
		},
		{
			name:        "wildcard without credentials returns *",
			cfg:         CORSConfig{AllowedOrigins: []string{"*"}},
			origin:      "https://anywhere.com",
			wantAllowed: "*",
		},
		{
			name:        "wildcard with credentials echoes origin",
			cfg:         CORSConfig{AllowedOrigins: []string{"*"}, AllowCredentials: true},
			origin:      "https://anywhere.com",
			wantAllowed: "https://anywhere.com",
		},
		{
			name:        "wildcard with credentials but no origin returns empty",
			cfg:         CORSConfig{AllowedOrigins: []string{"*"}, AllowCredentials: true},
			origin:      "",
			wantAllowed: "",
		},
		{
			name:        "multiple origins picks the matching one",
			cfg:         CORSConfig{AllowedOrigins: []string{"https://a.com", "https://b.com"}},
			origin:      "https://b.com",
			wantAllowed: "https://b.com",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := tt.cfg.resolveOrigin(tt.origin)
			assert.Equal(t, tt.wantAllowed, got)
		})
	}
}

func TestCORSOptionHelpers(t *testing.T) {
	t.Parallel()

	cfg := &CORSConfig{}
	for _, opt := range []CORSOption{
		WithCORSAllowedOrigins("https://example.com", "https://other.com"),
		WithCORSAllowedMethods(http.MethodGet, http.MethodPost),
		WithCORSAllowedHeaders("X-Custom", "Authorization"),
		WithCORSExposedHeaders("X-Trace"),
		WithCORSAllowCredentials(),
		WithCORSMaxAge(600),
	} {
		opt(cfg)
	}

	assert.Equal(t, []string{"https://example.com", "https://other.com"}, cfg.AllowedOrigins)
	assert.Equal(t, []string{http.MethodGet, http.MethodPost}, cfg.AllowedMethods)
	assert.Equal(t, []string{"X-Custom", "Authorization"}, cfg.AllowedHeaders)
	assert.Equal(t, []string{"X-Trace"}, cfg.ExposedHeaders)
	assert.True(t, cfg.AllowCredentials)
	assert.Equal(t, 600, cfg.MaxAge)
}

func TestCORSOptionHelpers_ReplaceOnRepeatedCall(t *testing.T) {
	t.Parallel()

	cfg := &CORSConfig{AllowedOrigins: []string{"https://stale.com"}}
	WithCORSAllowedOrigins("https://fresh.com")(cfg)
	assert.Equal(t, []string{"https://fresh.com"}, cfg.AllowedOrigins)
}

func TestStreamableHTTP_CORS_Preflight(t *testing.T) {
	t.Parallel()

	mcp := NewMCPServer("test", "1.0.0")
	srv := NewStreamableHTTPServer(mcp,
		WithStreamableHTTPCORS(
			WithCORSAllowedOrigins("https://example.com"),
			WithCORSAllowedMethods(http.MethodPost, http.MethodGet, http.MethodDelete, http.MethodOptions),
			WithCORSAllowedHeaders("Content-Type", "Mcp-Session-Id"),
			WithCORSAllowCredentials(),
			WithCORSMaxAge(120),
		),
	)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodOptions, ts.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "Content-Type")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, "https://example.com", resp.Header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "true", resp.Header.Get("Access-Control-Allow-Credentials"))
	assert.Equal(t, "120", resp.Header.Get("Access-Control-Max-Age"))
	assert.Contains(t, resp.Header.Get("Access-Control-Allow-Methods"), http.MethodPost)
	assert.Contains(t, resp.Header.Get("Access-Control-Allow-Headers"), "Mcp-Session-Id")
	assert.Equal(t, "Origin", resp.Header.Get("Vary"))
}

// With no allowed headers configured, the preflight advertises the headers a
// client of either era sends (Transports.md rows 27, 31 and 35), and not
// Last-Event-ID, which a modern request may not carry (row 42).
func TestStreamableHTTP_CORS_PreflightDefaultHeaders(t *testing.T) {
	t.Parallel()

	mcp := NewMCPServer("test", "1.0.0")
	srv := NewStreamableHTTPServer(mcp,
		WithStreamableHTTPCORS(WithCORSAllowedOrigins("https://example.com")),
	)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodOptions, ts.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	assert.Equal(t, "Content-Type, Authorization, Mcp-Protocol-Version, Mcp-Method, Mcp-Name, Mcp-Session-Id", resp.Header.Get("Access-Control-Allow-Headers"))
}

func TestStreamableHTTP_CORS_PreflightDisallowedOrigin(t *testing.T) {
	t.Parallel()

	mcp := NewMCPServer("test", "1.0.0")
	srv := NewStreamableHTTPServer(mcp,
		WithStreamableHTTPCORS(WithCORSAllowedOrigins("https://example.com")),
	)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodOptions, ts.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://attacker.com")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	// An Origin outside the allow list is refused outright, not merely left
	// without CORS headers: the spec wants 403 for an invalid Origin.
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))
}

// TestStreamableHTTP_CORS_DisallowedOriginIsForbiddenOnEveryVerb pins that a
// configured allow list validates Origin on every request (Transports.md row
// 14): a request from an origin outside it is 403 whatever its verb, and not
// served at all, while an allowed origin is served as before.
func TestStreamableHTTP_CORS_DisallowedOriginIsForbiddenOnEveryVerb(t *testing.T) {
	t.Parallel()

	calls := 0
	mcpServer := NewMCPServer("test", "1.0.0", WithHooks(&Hooks{
		OnBeforeInitialize: []OnBeforeInitializeFunc{func(context.Context, any, *mcp.InitializeRequest) { calls++ }},
	}))
	srv := NewStreamableHTTPServer(mcpServer,
		WithStreamableHTTPCORS(WithCORSAllowedOrigins("https://example.com")),
	)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	tests := []struct {
		name       string
		method     string
		origin     string
		wantStatus int
	}{
		{name: "POST from a disallowed origin", method: http.MethodPost, origin: "https://attacker.com", wantStatus: http.StatusForbidden},
		{name: "GET from a disallowed origin", method: http.MethodGet, origin: "https://attacker.com", wantStatus: http.StatusForbidden},
		{name: "POST from an allowed origin", method: http.MethodPost, origin: "https://example.com", wantStatus: http.StatusOK},
		{name: "POST with no origin", method: http.MethodPost, wantStatus: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := calls
			body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
			req, err := http.NewRequestWithContext(t.Context(), tt.method, ts.URL, body)
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			t.Cleanup(func() { _ = resp.Body.Close() })

			assert.Equal(t, tt.wantStatus, resp.StatusCode)
			if tt.wantStatus == http.StatusForbidden {
				assert.Equal(t, before, calls, "a refused request must not reach the server")
				assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))
			}
		})
	}
}

func TestStreamableHTTP_CORS_SimpleRequest(t *testing.T) {
	t.Parallel()

	mcp := NewMCPServer("test", "1.0.0")
	srv := NewStreamableHTTPServer(mcp,
		WithStreamableHTTPCORS(
			WithCORSAllowedOrigins("https://example.com"),
			WithCORSExposedHeaders("Mcp-Session-Id", "X-Trace"),
		),
	)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL, body)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://example.com")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, "https://example.com", resp.Header.Get("Access-Control-Allow-Origin"))
	exposed := resp.Header.Get("Access-Control-Expose-Headers")
	assert.Contains(t, exposed, "Mcp-Session-Id")
	assert.Contains(t, exposed, "X-Trace")
	assert.Equal(t, "Origin", resp.Header.Get("Vary"))
}

func TestStreamableHTTP_CORS_NoConfigPassesPreflightThrough(t *testing.T) {
	t.Parallel()

	mcp := NewMCPServer("test", "1.0.0")
	srv := NewStreamableHTTPServer(mcp)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodOptions, ts.URL, nil)
	require.NoError(t, err)
	// A loopback origin: the test server is on loopback, where a foreign
	// Origin is refused by the localhost protection, not passed through.
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	// With CORS disabled, OPTIONS is not handled specially and falls
	// through to ServeHTTP's default branch (404).
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Empty(t, resp.Header.Get("Access-Control-Allow-Origin"))
}

func TestStreamableHTTP_CORS_WildcardWithCredentialsEchoesOrigin(t *testing.T) {
	t.Parallel()

	mcp := NewMCPServer("test", "1.0.0")
	srv := NewStreamableHTTPServer(mcp,
		WithStreamableHTTPCORS(
			WithCORSAllowedOrigins("*"),
			WithCORSAllowCredentials(),
		),
	)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodOptions, ts.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Origin", "https://anywhere.com")
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })

	assert.Equal(t, "https://anywhere.com", resp.Header.Get("Access-Control-Allow-Origin"))
	assert.Equal(t, "true", resp.Header.Get("Access-Control-Allow-Credentials"))
}

func TestCORSConfig_Clone(t *testing.T) {
	t.Parallel()

	orig := &CORSConfig{
		AllowedOrigins:   []string{"a"},
		AllowedMethods:   []string{"GET"},
		AllowedHeaders:   []string{"X-A"},
		ExposedHeaders:   []string{"X-B"},
		AllowCredentials: true,
		MaxAge:           5,
	}
	cp := orig.clone()
	cp.AllowedOrigins[0] = "mutated"
	assert.Equal(t, "a", orig.AllowedOrigins[0], "clone must not share slice with original")

	var nilCfg *CORSConfig
	assert.Nil(t, nilCfg.clone())
}
