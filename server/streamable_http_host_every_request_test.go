package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// TestStreamableHTTP_ForeignHostIsRefusedOnEveryRequestKind pins DNS
// rebinding protection (upstream #1241, Transports.md rows 14 and 15) on every
// verb and request kind the Streamable HTTP handler serves, not only the POST
// initialize the other tests send: a foreign Host is answered 403 on a
// loopback connection by default and on a public one with WithAllowedHosts,
// for GET, DELETE, HEAD, OPTIONS, a modern POST, a subscriptions/listen and
// the protected-resource metadata, through ServeHTTP and, for the metadata,
// through the routes Start serves. The same request with an allowed Host is
// not refused, so the 403 is the Host check's.
func TestStreamableHTTP_ForeignHostIsRefusedOnEveryRequestKind(t *testing.T) {
	listen, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": mcp.MethodSubscriptionsListen,
		"params": map[string]any{"notifications": map[string]any{"toolsListChanged": true}, "_meta": modernMeta()}})
	require.NoError(t, err)
	list, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list",
		"params": map[string]any{"_meta": modernMeta()}})
	require.NoError(t, err)
	kinds := []struct {
		name, method, path string
		body               []byte
		mcpMethod          string
		// viaStart sends the request through the mux Start serves.
		viaStart bool
	}{
		{"GET", http.MethodGet, "/mcp", nil, "", false},
		{"DELETE", http.MethodDelete, "/mcp", nil, "", false},
		{"HEAD", http.MethodHead, "/mcp", nil, "", false},
		{"OPTIONS", http.MethodOptions, "/mcp", nil, "", false},
		{"POST modern tools/list", http.MethodPost, "/mcp", list, "tools/list", false},
		{"POST subscriptions/listen", http.MethodPost, "/mcp", listen, string(mcp.MethodSubscriptionsListen), false},
		{"GET protected-resource metadata", http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil, "", false},
		{"GET protected-resource metadata through Start's routes", http.MethodGet, "/.well-known/oauth-protected-resource/mcp", nil, "", true},
		{"GET through Start's routes", http.MethodGet, "/mcp", nil, "", true},
	}
	legs := []struct {
		name, localIP string
		allowed       []string
		good          string
	}{
		{"loopback default", "127.0.0.1", nil, "localhost:8080"},
		{"public with allowlist", "203.0.113.5", []string{"mcp.example.com"}, "mcp.example.com"},
	}
	const foreign = "evil.example"
	for _, leg := range legs {
		for _, host := range []string{foreign, leg.good} {
			for _, kind := range kinds {
				t.Run(leg.name+"/"+host+"/"+kind.name, func(t *testing.T) {
					httpServer := NewStreamableHTTPServer(NewMCPServer("test", "1.0.0", WithToolCapabilities(true)),
						WithAllowedHosts(leg.allowed...),
						WithProtectedResourceMetadata(ProtectedResourceMetadataConfig{
							Resource:             "https://mcp.example.com/mcp",
							AuthorizationServers: []string{"https://as.example"},
						}))
					ctx := context.WithValue(t.Context(), http.LocalAddrContextKey,
						&net.TCPAddr{IP: net.ParseIP(leg.localIP), Port: 8080})
					req := httptest.NewRequestWithContext(ctx, kind.method, "http://placeholder"+kind.path, bytes.NewReader(kind.body))
					req.Host = host
					if kind.body != nil {
						req.Header.Set("Content-Type", "application/json")
						req.Header.Set("Accept", "application/json, text/event-stream")
						req.Header.Set(mcp.HeaderProtocolVersion, mcp.ProtocolVersion20260728)
						req.Header.Set(mcp.HeaderMethod, kind.mcpMethod)
					}
					var handler http.Handler = httpServer
					if kind.viaStart {
						handler = httpServer.startMux()
					}
					recorder := httptest.NewRecorder()
					done := make(chan struct{})
					go func() {
						defer close(done)
						handler.ServeHTTP(recorder, req)
					}()
					if kind.mcpMethod == string(mcp.MethodSubscriptionsListen) {
						// The stream a listen opens, were it let through, ends
						// with the server.
						require.NoError(t, httpServer.Shutdown(t.Context()))
					}
					<-done

					if host == foreign {
						assert.Equal(t, http.StatusForbidden, recorder.Code)
						if kind.method != http.MethodHead {
							assert.Contains(t, recorder.Body.String(), "invalid Host header")
						}
					} else {
						assert.NotEqual(t, http.StatusForbidden, recorder.Code)
					}
				})
			}
		}
	}
}
