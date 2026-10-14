package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsLoopbackHost(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"localhost", true},
		{"LocalHost", true},
		{"localhost:3000", true},
		{"127.0.0.1", true},
		{"127.0.0.1:3000", true},
		{"127.1.2.3:3000", true},
		{"[::1]", true},
		{"[::1]:3000", true},
		{"::1", true},
		{"", false},
		{"evil.com", false},
		{"evil.com:80", false},
		{"localhost.evil.com", false},
		{"127.0.0.1.evil.com", false},
		{"192.168.1.10:8080", false},
		{"[2001:db8::1]:8080", false},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			assert.Equal(t, tt.want, isLoopbackHost(tt.addr), "isLoopbackHost(%q)", tt.addr)
		})
	}
}

// localhostProtectionTests is the shared table for the transport-level DNS
// rebinding protection tests below.
var localhostProtectionTests = []struct {
	name              string
	hostHeader        string
	disableProtection bool
	wantForbidden     bool
}{
	{
		name:       "accepts 127.0.0.1 host header",
		hostHeader: "127.0.0.1:1234",
	},
	{
		name:       "accepts localhost host header",
		hostHeader: "localhost:1234",
	},
	{
		name:       "accepts [::1] host header",
		hostHeader: "[::1]:1234",
	},
	{
		name:          "rejects evil.com",
		hostHeader:    "evil.com",
		wantForbidden: true,
	},
	{
		name:          "rejects evil.com:80",
		hostHeader:    "evil.com:80",
		wantForbidden: true,
	},
	{
		name:          "rejects localhost.evil.com",
		hostHeader:    "localhost.evil.com",
		wantForbidden: true,
	},
	{
		name:              "disabled accepts evil.com",
		hostHeader:        "evil.com",
		disableProtection: true,
	},
}

// startLoopbackServer serves handler on a real loopback listener so that
// http.LocalAddrContextKey reflects an actual 127.0.0.1 connection. The
// server and the returned client both carry explicit timeouts so a stalled
// handler or connection cannot hang the test run.
func startLoopbackServer(t *testing.T, handler http.Handler) (addr string, client *http.Client) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
	}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	return listener.Addr().String(), &http.Client{Timeout: 5 * time.Second}
}

// TestStreamableHTTP_LocalhostProtection verifies that DNS rebinding
// protection is automatically enabled for the streamable HTTP transport when
// requests arrive over a loopback connection.
func TestStreamableHTTP_LocalhostProtection(t *testing.T) {
	for _, tt := range localhostProtectionTests {
		t.Run(tt.name, func(t *testing.T) {
			mcpServer := NewMCPServer("test", "1.0.0")
			httpServer := NewStreamableHTTPServer(mcpServer,
				WithDisableLocalhostProtection(tt.disableProtection),
			)
			addr, client := startLoopbackServer(t, httpServer)

			body, err := json.Marshal(initRequest)
			require.NoError(t, err)

			req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s/mcp", addr), bytes.NewReader(body))
			require.NoError(t, err)
			req.Host = tt.hostHeader
			req.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			if tt.wantForbidden {
				assert.Equal(t, http.StatusForbidden, resp.StatusCode)
				payload, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				assert.Contains(t, string(payload), "invalid Host header")
			} else {
				assert.Equal(t, http.StatusOK, resp.StatusCode)
			}
		})
	}
}

// A connection on a loopback, private, shared or link-local address is a
// DNS rebinding target. Only a browser can be rebound, and a browser sends
// Origin on every POST, so a request carrying an Origin that does not name
// the server is refused there; Host, which a proxy or a container network
// sets to the server's own hostname, is checked on loopback only. A header
// names the server when it is a loopback name, the connection's own
// address, or a host WithAllowedHosts allows (Transports.md rows 14 and
// 15). A public address is reachable without rebinding, so it is served as
// before.
func TestStreamableHTTP_PrivateNetworkProtection(t *testing.T) {
	for _, tt := range []struct {
		name          string
		localIP       string
		host, origin  string
		allowed       []string
		wantForbidden string // the header the 403 names, empty when served
	}{
		{name: "a private address serves a non-browser client whatever its host", localIP: "192.168.1.10", host: "mcp.example.com"},
		{name: "a private address rejects a foreign origin", localIP: "192.168.1.10", host: "evil.com:8080", origin: "http://evil.com:8080", wantForbidden: "Origin"},
		{name: "a private address accepts its own origin", localIP: "192.168.1.10", host: "192.168.1.10:8080", origin: "http://192.168.1.10:8080"},
		{name: "a private address accepts a loopback origin", localIP: "192.168.1.10", host: "192.168.1.10:8080", origin: "http://localhost:5173"},
		{name: "a private address rejects another private origin", localIP: "192.168.1.10", host: "192.168.1.10:8080", origin: "http://10.0.0.1:8080", wantForbidden: "Origin"},
		{name: "an allowed host is accepted as an origin", localIP: "192.168.1.10", host: "mcp.internal", origin: "https://MCP.internal:3000", allowed: []string{"mcp.internal"}},
		{name: "an allowed host with a trailing dot is accepted", localIP: "192.168.1.10", host: "mcp.internal", origin: "https://mcp.internal.", allowed: []string{"mcp.internal"}},
		{name: "a hostname not allowed is rejected as an origin", localIP: "192.168.1.10", host: "mcp.internal", origin: "https://mcp.internal", wantForbidden: "Origin"},
		{name: "an allowed IP is accepted as an origin", localIP: "172.17.0.2", host: "192.168.1.5:8080", origin: "http://192.168.1.5:8080", allowed: []string{"192.168.1.5"}},
		{name: "an allowed bracketed IPv6 entry is accepted", localIP: "fd12::1", host: "[fd12::2]:8080", origin: "http://[fd12::2]:8080", allowed: []string{"[fd12::2]"}},
		{name: "an opaque origin is rejected", localIP: "192.168.1.10", host: "192.168.1.10:8080", origin: "null", wantForbidden: "Origin"},
		{name: "a link-local address rejects a foreign origin", localIP: "169.254.10.10", host: "evil.com", origin: "http://evil.com", wantForbidden: "Origin"},
		{name: "a unique local address rejects a foreign origin", localIP: "fd12::1", host: "evil.com", origin: "http://evil.com", wantForbidden: "Origin"},
		{name: "a shared address rejects a foreign origin", localIP: "100.64.1.2", host: "evil.com", origin: "http://evil.com", wantForbidden: "Origin"},
		{name: "a mapped private address rejects a foreign origin", localIP: "::ffff:192.168.1.10", host: "evil.com", origin: "http://evil.com", wantForbidden: "Origin"},
		{name: "a public address is served as before", localIP: "203.0.113.5", host: "evil.com", origin: "http://evil.com"},
		{name: "a loopback address rejects a foreign host", localIP: "127.0.0.1", host: "evil.com", wantForbidden: "Host"},
		{name: "a loopback address accepts an allowed host", localIP: "127.0.0.1", host: "mcp.internal:8080", allowed: []string{"mcp.internal"}},
		{name: "a loopback address accepts an allowed IP host", localIP: "127.0.0.1", host: "203.0.113.5:8080", allowed: []string{"203.0.113.5"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			httpServer := NewStreamableHTTPServer(NewMCPServer("test", "1.0.0"), WithAllowedHosts(tt.allowed...))
			body, err := json.Marshal(initRequest)
			require.NoError(t, err)
			req, err := http.NewRequest(http.MethodPost, "http://"+tt.host+"/mcp", bytes.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			localAddr := &net.TCPAddr{IP: net.ParseIP(tt.localIP), Port: 8080}
			req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, localAddr))

			rr := httptest.NewRecorder()
			httpServer.ServeHTTP(rr, req)
			if tt.wantForbidden == "" {
				assert.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
				return
			}
			assert.Equal(t, http.StatusForbidden, rr.Code)
			assert.Contains(t, rr.Body.String(), "invalid "+tt.wantForbidden+" header")
		})
	}
}

// originProtectionTests is the table for the Origin half of the loopback
// protection: a browser's Origin, unlike Host, survives DNS rebinding, so a
// loopback connection carrying a non-loopback Origin is refused (Transports.md
// row 14). A configured CORS allow list is the authority instead.
var originProtectionTests = []struct {
	name              string
	origin            string
	disableProtection bool
	allowedOrigins    []string
	wantForbidden     bool
}{
	{name: "no Origin is a non-browser client"},
	{name: "accepts a localhost origin", origin: "http://localhost:5173"},
	{name: "accepts a 127.0.0.1 origin", origin: "http://127.0.0.1:3000"},
	{name: "accepts a [::1] origin", origin: "http://[::1]:3000"},
	{name: "rejects a foreign origin", origin: "https://attacker.example", wantForbidden: true},
	{name: "rejects an opaque origin", origin: "null", wantForbidden: true},
	{name: "disabled accepts a foreign origin", origin: "https://attacker.example", disableProtection: true},
	{name: "a CORS allow list is the authority", origin: "https://my-ai-app.example", allowedOrigins: []string{"https://my-ai-app.example"}},
}

// TestStreamableHTTP_LocalhostProtection_Origin verifies that a loopback
// connection carrying a non-loopback Origin header is refused with 403, as a
// non-loopback Host is, unless a CORS allow list decides instead.
func TestStreamableHTTP_LocalhostProtection_Origin(t *testing.T) {
	for _, tt := range originProtectionTests {
		t.Run(tt.name, func(t *testing.T) {
			mcpServer := NewMCPServer("test", "1.0.0")
			opts := []StreamableHTTPOption{WithDisableLocalhostProtection(tt.disableProtection)}
			if tt.allowedOrigins != nil {
				opts = append(opts, WithStreamableHTTPCORS(WithCORSAllowedOrigins(tt.allowedOrigins...)))
			}
			httpServer := NewStreamableHTTPServer(mcpServer, opts...)
			addr, client := startLoopbackServer(t, httpServer)

			body, err := json.Marshal(initRequest)
			require.NoError(t, err)

			req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s/mcp", addr), bytes.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}

			resp, err := client.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			if tt.wantForbidden {
				assert.Equal(t, http.StatusForbidden, resp.StatusCode)
				payload, err := io.ReadAll(resp.Body)
				require.NoError(t, err)
				assert.Contains(t, string(payload), "invalid Origin header")
			} else {
				assert.Equal(t, http.StatusOK, resp.StatusCode)
			}
		})
	}
}

// BenchmarkRejectDNSRebinding measures the per-request cost of the loopback
// protection for a local server: a loopback connection, a loopback Host, and
// either no Origin (every non-browser client) or a loopback Origin.
func BenchmarkRejectDNSRebinding(b *testing.B) {
	for _, leg := range []struct {
		name   string
		origin string
	}{
		{name: "no-origin"},
		{name: "loopback-origin", origin: "http://localhost:5173"},
	} {
		b.Run(leg.name, func(b *testing.B) {
			req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/mcp", nil)
			if leg.origin != "" {
				req.Header.Set("Origin", leg.origin)
			}
			localAddr := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080}
			req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, localAddr))
			w := httptest.NewRecorder()
			if rejectDNSRebinding(w, req, true, nil) {
				b.Fatal("the loopback request was rejected")
			}
			b.ReportAllocs()
			for b.Loop() {
				rejectDNSRebinding(w, req, true, nil)
			}
		})
	}
}
