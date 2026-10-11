package transport

import (
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pkceHandler returns a handler for server with nothing said about PKCE in
// its configuration.
func pkceHandler(t *testing.T, server *tokenAuthServer) *OAuthHandler {
	t.Helper()
	httpServer := server.start(t)
	handler := NewOAuthHandler(OAuthConfig{
		ClientID:              "client",
		RedirectURI:           "http://localhost:8085/callback",
		TokenStore:            NewMemoryTokenStore(),
		AuthServerMetadataURL: httpServer.URL + "/.well-known/oauth-authorization-server",
	})
	handler.SetBaseURL(httpServer.URL)
	return handler
}

// An MCP client MUST implement PKCE with S256, so the code challenge goes
// out and the verifier comes back without anyone switching it on.
func TestOAuthHandler_PKCEIsOnByDefault(t *testing.T) {
	server := &tokenAuthServer{codeChallengeMethods: []string{"S256"}}
	handler := pkceHandler(t, server)

	authURL, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.NoError(t, err)
	parsed, err := url.Parse(authURL)
	require.NoError(t, err)
	assert.Equal(t, "challenge", parsed.Query().Get("code_challenge"))
	assert.Equal(t, "S256", parsed.Query().Get("code_challenge_method"))

	require.NoError(t, handler.ProcessAuthorizationResponse(t.Context(), "code", "state", "verifier"))
	_, _, _, form := server.lastTokenRequest(t)
	assert.Equal(t, "verifier", form.Get("code_verifier"))
}

// The client MUST verify PKCE support in the authorization server's
// metadata before sending the user there: no code_challenge_methods_supported,
// or none listing S256, is a refusal.
func TestOAuthHandler_RefusesAnAuthorizationServerWithoutPKCE(t *testing.T) {
	for name, methods := range map[string][]string{
		"no methods advertised": nil,
		"only plain":            {"plain"},
	} {
		t.Run(name, func(t *testing.T) {
			handler := pkceHandler(t, &tokenAuthServer{codeChallengeMethods: methods})

			_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
			require.Error(t, err)
			assert.ErrorContains(t, err, "PKCE")
		})
	}
}

// A flow without a code challenge is not a PKCE flow, so the client does not
// start it.
func TestOAuthHandler_RequiresACodeChallenge(t *testing.T) {
	handler := pkceHandler(t, &tokenAuthServer{codeChallengeMethods: []string{"S256"}})

	_, err := handler.GetAuthorizationURL(t.Context(), "state", "")
	require.Error(t, err)
	assert.ErrorContains(t, err, "PKCE")

	handler.SetExpectedState("state")
	err = handler.ProcessAuthorizationResponse(t.Context(), "code", "state", "")
	require.Error(t, err)
	assert.ErrorContains(t, err, "PKCE")
}

// PKCEDisabled is the one way out, for an authorization server known to
// lack PKCE: nothing is checked and nothing is sent.
func TestOAuthHandler_PKCEDisabledSkipsItAll(t *testing.T) {
	server := &tokenAuthServer{}
	httpServer := server.start(t)
	handler := NewOAuthHandler(OAuthConfig{
		ClientID:              "client",
		RedirectURI:           "http://localhost:8085/callback",
		TokenStore:            NewMemoryTokenStore(),
		AuthServerMetadataURL: httpServer.URL + "/.well-known/oauth-authorization-server",
		PKCEDisabled:          true,
	})

	authURL, err := handler.GetAuthorizationURL(t.Context(), "state", "")
	require.NoError(t, err)
	parsed, err := url.Parse(authURL)
	require.NoError(t, err)
	assert.False(t, parsed.Query().Has("code_challenge"))

	require.NoError(t, handler.ProcessAuthorizationResponse(t.Context(), "code", "state", ""))
	_, _, _, form := server.lastTokenRequest(t)
	assert.False(t, form.Has("code_verifier"))
}
