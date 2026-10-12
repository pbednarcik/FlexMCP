package transport

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A token is for the MCP server and from the authorization server it was
// issued for (MCP authorization: clients send only tokens from the server's
// authorization server). The SDK records both when it saves a token, and
// does not hand a token for one server to a request for another.
func TestOAuthHandler_ATokenIsUsedOnlyForTheServerItWasIssuedFor(t *testing.T) {
	as := &tokenAuthServer{codeChallengeMethods: []string{"S256"}}
	asHTTP := as.start(t)
	store := NewMemoryTokenStore()
	handler := NewOAuthHandler(OAuthConfig{
		ClientID:              "client",
		RedirectURI:           "http://localhost:8085/callback",
		TokenStore:            store,
		AuthServerMetadataURL: asHTTP.URL + "/.well-known/oauth-authorization-server",
	})
	handler.SetBaseURL(asHTTP.URL + "/a")
	_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.NoError(t, err)
	require.NoError(t, handler.ProcessAuthorizationCallback(t.Context(), callback("", "code", "state"), "verifier"))

	saved, err := store.GetToken(t.Context())
	require.NoError(t, err)
	assert.Equal(t, asHTTP.URL+"/a", saved.Resource, "the token records the server it is for")
	assert.Equal(t, asHTTP.URL, saved.Issuer, "the token records the issuer it is from")
	header, err := handler.GetAuthorizationHeader(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "Bearer access-token", header)

	handler.SetBaseURL(asHTTP.URL + "/b")
	_, err = handler.GetAuthorizationHeader(t.Context())
	require.ErrorIs(t, err, ErrOAuthAuthorizationRequired, "server a's token is not sent to server b")
}

// A refresh token goes only to the authorization server that issued it: when
// discovery names another, the stored token is not refreshed there.
func TestOAuthHandler_ARefreshTokenGoesOnlyToItsIssuer(t *testing.T) {
	first := (&tokenAuthServer{codeChallengeMethods: []string{"S256"}}).start(t)
	second := &tokenAuthServer{codeChallengeMethods: []string{"S256"}}
	secondHTTP := second.start(t)
	resource, _ := switchingResource(t, secondHTTP.URL)
	store := NewMemoryTokenStore()
	require.NoError(t, store.SaveToken(t.Context(), &Token{
		AccessToken:  "old",
		RefreshToken: "refresh-from-first",
		ExpiresAt:    time.Now().Add(-time.Minute),
		Resource:     resource.URL,
		Issuer:       first.URL,
	}))
	handler := NewOAuthHandler(OAuthConfig{RedirectURI: "http://localhost:8085/callback", TokenStore: store})
	handler.SetBaseURL(resource.URL)

	_, err := handler.GetAuthorizationHeader(t.Context())
	require.ErrorIs(t, err, ErrOAuthAuthorizationRequired)
	_, err = handler.RefreshToken(t.Context(), "refresh-from-first")
	require.ErrorIs(t, err, ErrIssuerMismatch)
	second.mu.Lock()
	defer second.mu.Unlock()
	assert.Empty(t, second.forms, "the refresh token went to another issuer")
}

// A token saved without a record of its server, as one an application put in
// the store itself or one saved before the record existed, is still used.
func TestOAuthHandler_ATokenWithoutARecordIsStillUsed(t *testing.T) {
	store := NewMemoryTokenStore()
	require.NoError(t, store.SaveToken(t.Context(), &Token{AccessToken: "app-token", ExpiresAt: time.Now().Add(time.Hour)}))
	handler := NewOAuthHandler(OAuthConfig{TokenStore: store})
	handler.SetBaseURL("https://mcp.example/mcp")

	header, err := handler.GetAuthorizationHeader(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "Bearer app-token", header)
}
