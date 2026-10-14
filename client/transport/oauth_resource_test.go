package transport

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
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

// The resource is the MCP server, known from the base URL or from protected
// resource metadata. A handler that knows neither does not guess from the
// redirect URI, which names the callback listener: it refuses to start.
func TestOAuthHandler_RefusesToStartWithoutAKnownResource(t *testing.T) {
	server := &tokenAuthServer{codeChallengeMethods: []string{"S256"}}
	httpServer := server.start(t)
	handler := NewOAuthHandler(OAuthConfig{
		ClientID:              "client",
		RedirectURI:           "http://localhost:8085/callback",
		TokenStore:            NewMemoryTokenStore(),
		AuthServerMetadataURL: httpServer.URL + "/.well-known/oauth-authorization-server",
	})

	_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.ErrorIs(t, err, ErrResourceUnknown)
	assert.Empty(t, handler.getResourceURL(), "the redirect origin is not the resource")

	handler.SetBaseURL(httpServer.URL + "/mcp")
	authURL, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.NoError(t, err)
	parsed, err := url.Parse(authURL)
	require.NoError(t, err)
	assert.Equal(t, httpServer.URL+"/mcp", parsed.Query().Get("resource"))
}

// resourceServer is an MCP server that serves its protected resource
// metadata, naming itself as the resource and as its authorization server
// the one given, at the well-known path and at /prm, the one it advertises.
func resourceServer(t *testing.T, authorizationServer string) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-protected-resource" && r.URL.Path != "/prm" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": server.URL, "authorization_servers": []string{authorizationServer}})
	}))
	t.Cleanup(server.Close)
	return server
}

// resourceOf returns the resource parameter of the authorization URL the
// handler builds.
func resourceOf(t *testing.T, handler *OAuthHandler) string {
	t.Helper()
	authURL, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.NoError(t, err)
	parsed, err := url.Parse(authURL)
	require.NoError(t, err)
	return parsed.Query().Get("resource")
}

// A metadata URL a server advertised in its challenge belongs to that
// server: a change of the MCP server's URL forgets it, so discovery for the
// new server starts from its own well-known path. A metadata URL the caller
// configured is the caller's and stays.
func TestOAuthHandler_SetBaseURLForgetsTheAdvertisedMetadata(t *testing.T) {
	as := (&tokenAuthServer{codeChallengeMethods: []string{"S256"}}).start(t)
	first := resourceServer(t, as.URL)
	second := resourceServer(t, as.URL)

	t.Run("advertised", func(t *testing.T) {
		handler := NewOAuthHandler(OAuthConfig{ClientID: "client", RedirectURI: "http://localhost:8085/callback", TokenStore: NewMemoryTokenStore()})
		handler.SetBaseURL(first.URL)
		handler.HandleUnauthorizedResponse(&http.Response{Header: http.Header{"Www-Authenticate": []string{`Bearer resource_metadata="` + first.URL + `/prm"`}}})
		assert.Equal(t, first.URL, resourceOf(t, handler))

		handler.SetBaseURL(second.URL)
		assert.Equal(t, second.URL, resourceOf(t, handler), "discovery for the second server does not read the first's advertised metadata")
	})

	t.Run("configured", func(t *testing.T) {
		handler := NewOAuthHandler(OAuthConfig{ClientID: "client", RedirectURI: "http://localhost:8085/callback", TokenStore: NewMemoryTokenStore(), ProtectedResourceMetadataURL: first.URL + "/prm"})
		handler.SetBaseURL(first.URL)
		assert.Equal(t, first.URL, resourceOf(t, handler))

		handler.SetBaseURL(second.URL)
		_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
		require.Error(t, err, "the configured metadata names the first server, which is not the second")
		assert.ErrorContains(t, err, "does not match base URL")
	})
}

// Discovery starts from the MCP server's URL and nowhere else: a handler
// that does not know it does not look at the redirect origin, where a
// metadata document naming the callback listener would otherwise make that
// listener the resource.
func TestOAuthHandler_DoesNotDiscoverFromTheRedirectOrigin(t *testing.T) {
	as := (&tokenAuthServer{codeChallengeMethods: []string{"S256"}}).start(t)
	callbackOrigin := resourceServer(t, as.URL)
	handler := NewOAuthHandler(OAuthConfig{ClientID: "client", RedirectURI: callbackOrigin.URL + "/callback", TokenStore: NewMemoryTokenStore()})

	_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.ErrorIs(t, err, ErrResourceUnknown)
	assert.Empty(t, handler.getResourceURL(), "the callback listener is not the resource")
}

// Every token request names the resource (RFC 8707): a refresh or a code
// exchange on a handler that does not know the MCP server is refused, not
// sent without one.
func TestOAuthHandler_TokenRequestsNameTheResource(t *testing.T) {
	server := &tokenAuthServer{codeChallengeMethods: []string{"S256"}}
	httpServer := server.start(t)
	handler := NewOAuthHandler(OAuthConfig{
		ClientID:              "client",
		ClientIssuer:          httpServer.URL,
		RedirectURI:           "http://localhost:8085/callback",
		TokenStore:            NewMemoryTokenStore(),
		AuthServerMetadataURL: httpServer.URL + "/.well-known/oauth-authorization-server",
	})

	_, err := handler.RefreshToken(t.Context(), "refresh")
	require.ErrorIs(t, err, ErrResourceUnknown)
	handler.SetExpectedState("state")
	err = handler.ProcessAuthorizationResponse(t.Context(), "code", "state", "verifier")
	require.ErrorIs(t, err, ErrResourceUnknown)
	server.mu.Lock()
	assert.Empty(t, server.forms, "no token request went out without the resource")
	server.mu.Unlock()

	handler.SetBaseURL(httpServer.URL + "/mcp")
	_, err = handler.RefreshToken(t.Context(), "refresh")
	require.NoError(t, err)
	_, _, _, form := server.lastTokenRequest(t)
	assert.Equal(t, httpServer.URL+"/mcp", form.Get("resource"))
}

// The resource is the MCP server's canonical URI (RFC 8707 §2: an absolute
// URI without a fragment; MCP authorization: scheme and host). A base URL
// that is not one is refused before any request names it, on the path with
// a configured authorization server as on the discovery path.
func TestOAuthHandler_RefusesAResourceThatIsNotACanonicalURI(t *testing.T) {
	server := &tokenAuthServer{codeChallengeMethods: []string{"S256"}}
	httpServer := server.start(t)
	for _, tt := range []struct {
		name   string
		base   string
		config OAuthConfig
	}{
		{name: "relative, with a configured authorization server", base: "/mcp", config: OAuthConfig{AuthServerMetadataURL: httpServer.URL + "/.well-known/oauth-authorization-server"}},
		{name: "with a fragment, with a configured authorization server", base: httpServer.URL + "/mcp#fragment", config: OAuthConfig{AuthServerMetadataURL: httpServer.URL + "/.well-known/oauth-authorization-server"}},
		{name: "with an empty fragment", base: httpServer.URL + "/mcp#", config: OAuthConfig{AuthServerMetadataURL: httpServer.URL + "/.well-known/oauth-authorization-server"}},
		{name: "with a fragment, discovered", base: httpServer.URL + "/mcp#fragment"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			config := tt.config
			config.ClientID = "client"
			// A raw refresh token reaches the resource check only with
			// ClientIssuer set.
			config.ClientIssuer = httpServer.URL
			config.RedirectURI = "http://localhost:8085/callback"
			config.TokenStore = NewMemoryTokenStore()
			handler := NewOAuthHandler(config)
			handler.SetBaseURL(tt.base)

			_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
			require.ErrorIs(t, err, ErrInvalidResource)
			_, err = handler.RefreshToken(t.Context(), "refresh")
			require.ErrorIs(t, err, ErrInvalidResource)
		})
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	assert.Empty(t, server.forms, "no token request named an invalid resource")
}

// Protected resource metadata may declare the resource, which binding holds
// to the base URL's scheme, host and fragment; an empty fragment ("#") parses
// as none and binds, so the declared resource is checked too.
func TestOAuthHandler_RefusesADeclaredResourceWithAFragment(t *testing.T) {
	as := (&tokenAuthServer{codeChallengeMethods: []string{"S256"}}).start(t)
	var resource *httptest.Server
	resource = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-protected-resource" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": resource.URL + "#", "authorization_servers": []string{as.URL}})
	}))
	t.Cleanup(resource.Close)
	handler := NewOAuthHandler(OAuthConfig{ClientID: "client", RedirectURI: "http://localhost:8085/callback", TokenStore: NewMemoryTokenStore()})
	handler.SetBaseURL(resource.URL)

	_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.ErrorIs(t, err, ErrInvalidResource)
}

// An authorization in progress belongs to the MCP server it was started for
// (RFC 8707 §2.1: the code is redeemed for the resource it was granted for).
// Pointing the handler at another server abandons it: the late callback is
// refused and the code is not redeemed for the new server.
func TestOAuthHandler_RetargetingAbandonsTheAuthorizationInProgress(t *testing.T) {
	as := &tokenAuthServer{codeChallengeMethods: []string{"S256"}}
	asHTTP := as.start(t)
	handler := NewOAuthHandler(OAuthConfig{
		ClientID:              "client",
		RedirectURI:           "http://localhost:8085/callback",
		TokenStore:            NewMemoryTokenStore(),
		AuthServerMetadataURL: asHTTP.URL + "/.well-known/oauth-authorization-server",
	})
	handler.SetBaseURL(asHTTP.URL + "/a")
	_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.NoError(t, err)

	handler.SetBaseURL(asHTTP.URL + "/b")
	err = handler.ProcessAuthorizationCallback(t.Context(), callback("", "code", "state"), "verifier")
	require.Error(t, err, "server a's callback is not redeemed once the handler serves server b")
	assert.Empty(t, handler.GetExpectedState())
	as.mu.Lock()
	defer as.mu.Unlock()
	assert.Empty(t, as.forms)
}

// An authorization that the base URL changes under while it starts is for
// the previous server: it is refused, and nothing of it is recorded after
// the retarget cleared what was pending.
func TestOAuthHandler_RetargetingAbandonsAnAuthorizationStartingBesideIt(t *testing.T) {
	asHTTP := (&tokenAuthServer{codeChallengeMethods: []string{"S256"}}).start(t)
	handler := NewOAuthHandler(OAuthConfig{
		ClientID:              "client",
		RedirectURI:           "http://localhost:8085/callback",
		TokenStore:            NewMemoryTokenStore(),
		AuthServerMetadataURL: asHTTP.URL + "/.well-known/oauth-authorization-server",
	})
	handler.SetBaseURL(asHTTP.URL + "/a")
	handler.testHookFlowSnapshot = func() { handler.SetBaseURL(asHTTP.URL + "/b") }

	authURL, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.ErrorIs(t, err, ErrResourceChanged)
	assert.Empty(t, authURL)
	assert.Empty(t, handler.GetExpectedState(), "server a's state outlived the retarget")
	assert.Empty(t, handler.GetExpectedIssuer(), "server a's issuer outlived the retarget")
	handler.mu.RLock()
	defer handler.mu.RUnlock()
	assert.Empty(t, handler.expectedResource, "server a's resource outlived the retarget")
}

// The resource can change without a new base URL, when discovery runs again
// and the server's metadata declares another one. The code is not redeemed
// for a resource other than the one it was obtained for.
func TestOAuthHandler_RedeemsTheCodeOnlyForTheResourceItWasObtainedFor(t *testing.T) {
	as := &tokenAuthServer{codeChallengeMethods: []string{"S256"}}
	asHTTP := as.start(t)
	// Found at the well-known path, the metadata may declare the origin as
	// the resource; found at an advertised URL it must declare the base URL
	// itself. So the resource changes from the origin to /mcp.
	var mu sync.Mutex
	path := ""
	var resource *httptest.Server
	resource = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-protected-resource/mcp" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		declared := resource.URL + path
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": declared, "authorization_servers": []string{asHTTP.URL}})
	}))
	t.Cleanup(resource.Close)
	handler := NewOAuthHandler(OAuthConfig{ClientID: "client", RedirectURI: "http://localhost:8085/callback", TokenStore: NewMemoryTokenStore()})
	handler.SetBaseURL(resource.URL + "/mcp")
	_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.NoError(t, err)

	mu.Lock()
	path = "/mcp"
	mu.Unlock()
	handler.SetProtectedResourceMetadataURL(resource.URL + "/.well-known/oauth-protected-resource/mcp")
	err = handler.ProcessAuthorizationCallback(t.Context(), callback("", "code", "state"), "verifier")
	require.ErrorIs(t, err, ErrResourceChanged)
	as.mu.Lock()
	defer as.mu.Unlock()
	assert.Empty(t, as.forms)
}
