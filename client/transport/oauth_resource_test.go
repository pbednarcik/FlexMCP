package transport

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The resource parameter (RFC 8707) names the MCP server in the
// authorization request and the token request whether or not the
// authorization server supports it, and whether or not protected resource
// metadata was there to say so: without it, the MCP server's own URL is the
// resource.
func TestOAuthHandler_SendsTheResourceWithoutProtectedResourceMetadata(t *testing.T) {
	tests := []struct {
		name   string
		config func(httpServer string) OAuthConfig
	}{
		{name: "discovery falls through to authorization server metadata", config: func(string) OAuthConfig { return OAuthConfig{} }},
		{name: "authorization server metadata URL given", config: func(httpServer string) OAuthConfig {
			return OAuthConfig{AuthServerMetadataURL: httpServer + "/.well-known/oauth-authorization-server"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &tokenAuthServer{codeChallengeMethods: []string{"S256"}}
			httpServer := server.start(t)
			config := tt.config(httpServer.URL)
			config.ClientID = "client"
			config.RedirectURI = "http://localhost:8085/callback"
			config.TokenStore = NewMemoryTokenStore()
			handler := NewOAuthHandler(config)
			handler.SetBaseURL(httpServer.URL)

			authURL, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
			require.NoError(t, err)
			parsed, err := url.Parse(authURL)
			require.NoError(t, err)
			assert.Equal(t, httpServer.URL, parsed.Query().Get("resource"))

			require.NoError(t, handler.ProcessAuthorizationCallback(t.Context(), callback("", "code", "state"), "verifier"))
			_, _, _, form := server.lastTokenRequest(t)
			assert.Equal(t, httpServer.URL, form.Get("resource"))
		})
	}
}
