package transport

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOAuthHandler_DefaultEndpointsDiscardURLComponents pins that the fallback
// endpoints are built from the server's origin alone: a path, query or
// fragment on the MCP URL must not leak into them.
func TestOAuthHandler_DefaultEndpointsDiscardURLComponents(t *testing.T) {
	tests := []struct {
		name   string
		suffix string
	}{
		{name: "origin"},
		{name: "path", suffix: "/mcp"},
		{name: "query", suffix: "/mcp?tenant=acme"},
		{name: "empty query", suffix: "/mcp?"},
		{name: "fragment", suffix: "/mcp#section"},
		{name: "query and fragment", suffix: "/mcp?tenant=acme#section"},
	}
	h := NewOAuthHandler(OAuthConfig{ClientID: "c", RedirectURI: "http://localhost/cb"})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			metadata, err := h.getDefaultEndpoints("https://srv.example" + tt.suffix)
			require.NoError(t, err)
			assert.Equal(t, "https://srv.example", metadata.Issuer)
			assert.Equal(t, "https://srv.example/authorize", metadata.AuthorizationEndpoint)
			assert.Equal(t, "https://srv.example/token", metadata.TokenEndpoint)
			assert.Equal(t, "https://srv.example/register", metadata.RegistrationEndpoint)
		})
	}
}

// TestOAuthHandler_LegacyPathMetadataIgnoresURLQuery pins that the issuer
// expected of metadata under the MCP server's path has no query or fragment
// (RFC 8414 §2): metadata served at the path for an MCP URL with a query is
// used, rather than passed over for the origin's. A fragment never gets this
// far: such a URL is refused as a resource identifier.
func TestOAuthHandler_LegacyPathMetadataIgnoresURLQuery(t *testing.T) {
	for _, suffix := range []string{"?tenant=acme", "?", "/?tenant=acme"} {
		t.Run(suffix, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				issuer := ""
				switch r.URL.Path {
				case "/.well-known/oauth-authorization-server/mcp":
					issuer = server.URL + "/mcp"
				case "/.well-known/oauth-authorization-server":
					issuer = server.URL
				default:
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"issuer":                           issuer,
					"authorization_endpoint":           issuer + "/authorize",
					"token_endpoint":                   issuer + "/token",
					"code_challenge_methods_supported": []string{"S256"},
				})
			}))
			t.Cleanup(server.Close)
			handler := NewOAuthHandler(OAuthConfig{ClientID: "c", RedirectURI: "http://localhost/cb", TokenStore: NewMemoryTokenStore()})
			handler.SetBaseURL(server.URL + "/mcp" + suffix)

			metadata, err := handler.GetServerMetadata(t.Context())
			require.NoError(t, err)
			assert.Equal(t, server.URL+"/mcp", metadata.Issuer)
			assert.Equal(t, server.URL+"/mcp/token", metadata.TokenEndpoint)
		})
	}
}
