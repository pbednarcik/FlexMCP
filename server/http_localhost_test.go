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
	"net/netip"
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
		{"LOCALHOST:3000", true},
		// A fully qualified name ends in one dot, and names the same host.
		{"localhost.", true},
		{"LOCALHOST.:3000", true},
		{"localhost..", false},
		{".", false},
		{"localhoſt.", false},
		// A long s folds to an s in Unicode, not in a host name.
		{"localhoſt", false},
		{"LOCALHOſT:3000", false},
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
// before, unless WithAllowedHosts names hosts. Names compare by ASCII case
// only, so a name Unicode case folding takes for a loopback name, an allowed
// host or an http scheme names nothing.
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
		{name: "a loopback address accepts a loopback host with a trailing dot", localIP: "127.0.0.1", host: "localhost.:8080"},
		{name: "a loopback address accepts a loopback host with a trailing dot beside a list", localIP: "127.0.0.1", host: "localhost.:8080", allowed: []string{"mcp.internal"}},
		{name: "a loopback address accepts a loopback origin with a trailing dot", localIP: "127.0.0.1", host: "localhost", origin: "http://localhost.:8080"},
		{name: "a loopback address rejects a long s in a loopback host", localIP: "127.0.0.1", host: "localhoſt:8080", wantForbidden: "Host"},
		{name: "a loopback address rejects a Kelvin sign in an allowed host", localIP: "127.0.0.1", host: "mcp.\u212aube", allowed: []string{"mcp.kube"}, wantForbidden: "Host"},
		{name: "a loopback address rejects a long s in a loopback origin", localIP: "127.0.0.1", host: "localhost", origin: "http://localhoſt:5173", wantForbidden: "Origin"},
		{name: "a private address rejects a final sigma for a medial one", localIP: "192.168.1.10", host: "192.168.1.10", origin: "https://mcp-ς.internal", allowed: []string{"mcp-σ.internal"}, wantForbidden: "Origin"},
		{name: "a private address rejects a long s in the scheme", localIP: "192.168.1.10", host: "192.168.1.10", origin: "httpſ://192.168.1.10", wantForbidden: "Origin"},
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

// Once WithAllowedHosts names hosts, Host is checked on every request,
// whatever the address it arrived on, public or none known: a public address
// only the victim's network reaches, such as a firewalled global IPv6
// address, is a rebinding target too. A Host names the server as a loopback
// name, the connection's own address or a listed host, on any port. Origin
// is judged as before, and not on a public address. Without a list nothing
// changes, and disabling the protection turns the list off.
func TestStreamableHTTP_AllowedHostsCheckHostOnEveryAddress(t *testing.T) {
	listed := []string{"mcp.example.com"}
	for _, tt := range []struct {
		name          string
		localIP       string // "" leaves the connection's address unknown
		host, origin  string
		allowed       []string
		opts          []StreamableHTTPOption
		wantForbidden string // the header the 403 names, empty when served
	}{
		{name: "a private address rejects a host off the list", localIP: "192.168.1.10", host: "evil.example", allowed: listed, wantForbidden: "Host"},
		{name: "a public address rejects a host off the list", localIP: "203.0.113.5", host: "evil.example:8080", allowed: listed, wantForbidden: "Host"},
		{name: "a global IPv6 address rejects a rebound POST", localIP: "2a01:4f8::5", host: "evil.example", origin: "http://evil.example", allowed: listed, wantForbidden: "Host"},
		{name: "a link-local address rejects a host off the list", localIP: "169.254.10.10", host: "evil.example", allowed: listed, wantForbidden: "Host"},
		{name: "a public address serves a listed host on any port", localIP: "203.0.113.5", host: "mcp.example.com:9090", allowed: listed},
		{name: "a public address serves a listed host in another case with a trailing dot", localIP: "203.0.113.5", host: "MCP.Example.COM.", allowed: listed},
		{name: "a public address serves its own address", localIP: "203.0.113.5", host: "203.0.113.5:8080", allowed: listed},
		{name: "a public address serves a loopback name", localIP: "203.0.113.5", host: "localhost:8080", allowed: listed},
		{name: "a public address serves a loopback name with a trailing dot", localIP: "203.0.113.5", host: "localhost.", allowed: listed},
		{name: "a public address does not judge Origin", localIP: "203.0.113.5", host: "mcp.example.com", origin: "http://evil.example", allowed: listed},
		{name: "a private address still rejects a foreign origin", localIP: "192.168.1.10", host: "mcp.example.com", origin: "http://evil.example", allowed: listed, wantForbidden: "Origin"},
		{name: "a public address without a list serves any host", localIP: "203.0.113.5", host: "evil.example"},
		{name: "a private address without a list serves any host", localIP: "192.168.1.10", host: "evil.example"},
		{name: "an unknown address rejects a host off the list", host: "evil.example", allowed: listed, wantForbidden: "Host"},
		{name: "an unknown address serves a listed host", host: "mcp.example.com", allowed: listed},
		{name: "an unknown address rejects an IP literal off the list", host: "203.0.113.5", allowed: listed, wantForbidden: "Host"},
		{name: "an unknown address without a list serves any host", host: "evil.example"},
		{name: "an empty entry names no host", localIP: "203.0.113.5", host: "mcp.example.com", allowed: []string{""}},
		{name: "an empty entry beside a listed host leaves the check on", localIP: "203.0.113.5", host: "evil.example", allowed: []string{"", "mcp.example.com"}, wantForbidden: "Host"},
		{name: "a link-local address serves its own address with a zone", localIP: "fe80::1", host: "[fe80::1%25en0]:8080", allowed: listed},
		{name: "a public address serves a host listed with its zone", localIP: "203.0.113.5", host: "[fe80::2%25en0]", allowed: []string{"[fe80::2%25en0]"}},
		{name: "a public address serves a host listed with a zone it lacks", localIP: "203.0.113.5", host: "[fe80::2]", allowed: []string{"fe80::2%en0"}},
		{name: "disabling the protection turns the list off", localIP: "203.0.113.5", host: "evil.example", allowed: listed, opts: []StreamableHTTPOption{WithDisableLocalhostProtection(true)}},
		{name: "a CORS allow list leaves the Host check", localIP: "203.0.113.5", host: "evil.example", allowed: listed, opts: []StreamableHTTPOption{WithStreamableHTTPCORS(WithCORSAllowedOrigins("https://app.example"))}, wantForbidden: "Host"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			opts := append([]StreamableHTTPOption{WithAllowedHosts(tt.allowed...)}, tt.opts...)
			httpServer := NewStreamableHTTPServer(NewMCPServer("test", "1.0.0"), opts...)
			body, err := json.Marshal(initRequest)
			require.NoError(t, err)
			req, err := http.NewRequest(http.MethodPost, "http://placeholder/mcp", bytes.NewReader(body))
			require.NoError(t, err)
			req.Host = tt.host
			req.Header.Set("Content-Type", "application/json")
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.localIP != "" {
				localAddr := &net.TCPAddr{IP: net.ParseIP(tt.localIP), Port: 8080}
				req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, localAddr))
			}

			rr := httptest.NewRecorder()
			httpServer.ServeHTTP(rr, req)
			if tt.wantForbidden == "" {
				assert.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
				return
			}
			assert.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Body.String(), "invalid "+tt.wantForbidden+" header")
		})
	}
}

// Host names and schemes compare by ASCII case only (RFC 3986 sections 3.1
// and 3.2.2). Unicode case folding takes a long s for an s, the Kelvin sign
// for a K and a final sigma for a medial one, so it would take distinct hosts
// for one.
func TestHostsFoldASCIICaseOnly(t *testing.T) {
	for _, tt := range []struct{ origin, host string }{
		{"httpſ://localhost", ""},
		{"HTTPS://mcp.example", "mcp.example"},
	} {
		host, ok := originHost(tt.origin)
		assert.Equal(t, tt.host != "", ok, "originHost(%q)", tt.origin)
		assert.Equal(t, tt.host, host, "originHost(%q)", tt.origin)
	}
	loopback := netip.MustParseAddr("127.0.0.1")
	for _, tt := range []struct {
		host, entry string
		want        bool
	}{
		{"mcp.\u212aube", "mcp.kube", false}, // the Kelvin sign
		{"mcp-ς.internal", "mcp-σ.internal", false},
		{"MCP.internal", "mcp.internal", true},
		{"mcp.internal", "MCP.INTERNAL", true},
		{"mcp.interna", "mcp.internal", false},
	} {
		assert.Equal(t, tt.want, hostNamesServer(tt.host, loopback, []string{tt.entry}), "hostNamesServer(%q, [%q])", tt.host, tt.entry)
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

// The protection allocates nothing on a request it serves, with a list of
// allowed hosts or without one: it runs on every request ServeHTTP serves.
func TestRejectDNSRebindingAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	listed := []string{"mcp.example.com", "api.example.com", "internal.example"}
	for _, leg := range []struct {
		name, localIP, host, origin string
		allowed                     []string
	}{
		{name: "loopback without a list", localIP: "127.0.0.1", host: "127.0.0.1:8080", origin: "http://localhost:5173"},
		{name: "loopback with a list", localIP: "127.0.0.1", host: "localhost:8080", allowed: listed},
		{name: "public without a list", localIP: "203.0.113.7", host: "mcp.example.com"},
		{name: "public with a list", localIP: "203.0.113.7", host: "INTERNAL.example.", allowed: listed},
		{name: "public with a list and its own address as host", localIP: "203.0.113.7", host: "203.0.113.7:8080", allowed: listed},
		{name: "public with an IP host listed after names", localIP: "203.0.113.7", host: "198.51.100.9", allowed: []string{"mcp.example.com", "api.example.com", "internal.example", "198.51.100.9"}},
		{name: "private with a list and an origin", localIP: "10.0.0.5", host: "mcp.example.com", origin: "https://api.example.com", allowed: listed},
	} {
		t.Run(leg.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://placeholder/mcp", nil)
			req.Host = leg.host
			if leg.origin != "" {
				req.Header.Set("Origin", leg.origin)
			}
			localAddr := &net.TCPAddr{IP: net.ParseIP(leg.localIP), Port: 8080}
			req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, localAddr))
			w := httptest.NewRecorder()
			require.False(t, rejectDNSRebinding(w, req, true, leg.allowed), w.Body.String())
			assert.Zero(t, testing.AllocsPerRun(100, func() { rejectDNSRebinding(w, req, true, leg.allowed) }))
		})
	}
}
