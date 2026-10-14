package transport

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Dynamic client registration states the application type: native for a
// client that takes its redirect on localhost or a custom scheme, web for
// one that takes it over HTTPS.
func TestOAuthHandler_RegistersWithAnApplicationType(t *testing.T) {
	for redirect, want := range map[string]string{
		"http://localhost:8085/callback": "native",
		"myapp://callback":               "native",
		"https://app.example/callback":   "web",
	} {
		t.Run(want+" "+redirect, func(t *testing.T) {
			server := &tokenAuthServer{registration: map[string]any{"client_id": "registered"}}
			httpServer := server.start(t)
			handler := NewOAuthHandler(OAuthConfig{
				RedirectURI:           redirect,
				TokenStore:            NewMemoryTokenStore(),
				AuthServerMetadataURL: httpServer.URL + "/.well-known/oauth-authorization-server",
			})

			require.NoError(t, handler.RegisterClient(t.Context(), "test"))
			server.mu.Lock()
			defer server.mu.Unlock()
			require.Len(t, server.registrations, 1)
			assert.Equal(t, want, server.registrations[0]["application_type"])
		})
	}
}

// Client credentials belong to the authorization server that issued them:
// after registering with one issuer, discovery that leads to another does
// not reuse the client ID there.
func TestOAuthHandler_DoesNotReuseRegisteredCredentialsWithAnotherIssuer(t *testing.T) {
	first := (&tokenAuthServer{registration: map[string]any{"client_id": "registered-with-first"}, codeChallengeMethods: []string{"S256"}}).start(t)
	second := (&tokenAuthServer{codeChallengeMethods: []string{"S256"}}).start(t)

	// The protected resource names first, then second, as its authorization
	// server.
	var mu sync.Mutex
	authorizationServer := first.URL
	var resource *httptest.Server
	resource = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-protected-resource" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": resource.URL, "authorization_servers": []string{authorizationServer}})
	}))
	t.Cleanup(resource.Close)

	handler := NewOAuthHandler(OAuthConfig{RedirectURI: "http://localhost:8085/callback", TokenStore: NewMemoryTokenStore()})
	handler.SetBaseURL(resource.URL)
	require.NoError(t, handler.RegisterClient(t.Context(), "test"))
	require.Equal(t, "registered-with-first", handler.GetClientID())
	_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.NoError(t, err, "the issuer the client registered with")

	mu.Lock()
	authorizationServer = second.URL
	mu.Unlock()
	handler.SetProtectedResourceMetadataURL(resource.URL + "/.well-known/oauth-protected-resource")

	_, err = handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.ErrorIs(t, err, ErrClientRegisteredElsewhere)
	// A callback for the flow started with first is refused too: the issuer
	// binding of the pending flow fires before the registration check.
	handler.SetExpectedState("state")
	err = handler.ProcessAuthorizationResponse(t.Context(), "code", "state", "verifier")
	require.ErrorIs(t, err, ErrIssuerMismatch)
	// A new flow against second cannot start with first's credentials.
	handler.SetExpectedState("state")
	handler.SetExpectedIssuer(second.URL, false)
	err = handler.ProcessAuthorizationResponse(t.Context(), "code", "state", "verifier")
	require.ErrorIs(t, err, ErrClientRegisteredElsewhere)
}

// switchingResource is an MCP server whose protected resource metadata names
// authorizationServer, until point names another one.
func switchingResource(t *testing.T, authorizationServer string) (resource *httptest.Server, point func(string)) {
	t.Helper()
	var mu sync.Mutex
	resource = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-protected-resource" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": resource.URL, "authorization_servers": []string{authorizationServer}})
	}))
	t.Cleanup(resource.Close)
	return resource, func(next string) {
		mu.Lock()
		authorizationServer = next
		mu.Unlock()
	}
}

// Preconfigured credentials belong to one authorization server as much as
// registered ones do (MCP authorization: never reuse them with another). They
// are bound to the first issuer they are used with; when discovery later
// names another, no flow starts there and no token request carries them.
func TestOAuthHandler_BindsPreconfiguredCredentialsToTheirIssuer(t *testing.T) {
	first := (&tokenAuthServer{codeChallengeMethods: []string{"S256"}}).start(t)
	second := &tokenAuthServer{codeChallengeMethods: []string{"S256"}}
	secondHTTP := second.start(t)
	resource, point := switchingResource(t, first.URL)

	store := NewMemoryTokenStore()
	handler := NewOAuthHandler(OAuthConfig{ClientID: "client", ClientSecret: "secret", RedirectURI: "http://localhost:8085/callback", TokenStore: store})
	handler.SetBaseURL(resource.URL)
	_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.NoError(t, err)
	assert.Equal(t, first.URL, handler.GetClientIssuer(), "bound to the first issuer")

	point(secondHTTP.URL)
	handler.SetProtectedResourceMetadataURL(resource.URL + "/.well-known/oauth-protected-resource")
	_, err = handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.ErrorIs(t, err, ErrClientRegisteredElsewhere)
	// A refresh token the new issuer did issue still does not take the
	// credentials there.
	require.NoError(t, store.SaveToken(t.Context(), &Token{RefreshToken: "refresh", Resource: resource.URL, Issuer: secondHTTP.URL}))
	_, err = handler.RefreshToken(t.Context(), "refresh")
	require.ErrorIs(t, err, ErrClientRegisteredElsewhere)
	second.mu.Lock()
	defer second.mu.Unlock()
	assert.Empty(t, second.forms, "the client secret went to another issuer's token endpoint")
}

// ClientIssuer binds preconfigured credentials from the first request: an
// authorization server that is not theirs never sees them.
func TestOAuthHandler_ClientIssuerBindsTheCredentialsFromTheStart(t *testing.T) {
	theirs := (&tokenAuthServer{codeChallengeMethods: []string{"S256"}}).start(t)
	other := &tokenAuthServer{codeChallengeMethods: []string{"S256"}}
	otherHTTP := other.start(t)
	resource, _ := switchingResource(t, otherHTTP.URL)

	store := NewMemoryTokenStore()
	require.NoError(t, store.SaveToken(t.Context(), &Token{RefreshToken: "refresh", Resource: resource.URL, Issuer: otherHTTP.URL}))
	handler := NewOAuthHandler(OAuthConfig{ClientID: "client", ClientSecret: "secret", ClientIssuer: theirs.URL, RedirectURI: "http://localhost:8085/callback", TokenStore: store})
	handler.SetBaseURL(resource.URL)
	_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.ErrorIs(t, err, ErrClientRegisteredElsewhere)
	_, err = handler.RefreshToken(t.Context(), "refresh")
	require.ErrorIs(t, err, ErrClientRegisteredElsewhere)
	other.mu.Lock()
	defer other.mu.Unlock()
	assert.Empty(t, other.forms)
}

// Registering again replaces the whole credential set: a secret from the
// earlier registration is not kept beside the new client ID.
func TestOAuthHandler_RegistrationReplacesTheWholeCredentialSet(t *testing.T) {
	server := &tokenAuthServer{registration: map[string]any{"client_id": "new"}}
	httpServer := server.start(t)
	handler := NewOAuthHandler(OAuthConfig{
		ClientID:              "old",
		ClientSecret:          "old-secret",
		RedirectURI:           "http://localhost:8085/callback",
		TokenStore:            NewMemoryTokenStore(),
		AuthServerMetadataURL: httpServer.URL + "/.well-known/oauth-authorization-server",
	})

	require.NoError(t, handler.RegisterClient(t.Context(), "test"))
	assert.Equal(t, "new", handler.GetClientID())
	assert.Empty(t, handler.GetClientSecret(), "the old secret is not paired with the new client")
	assert.Equal(t, httpServer.URL, handler.GetClientIssuer())
}

// The credentials are replaced and read as one record: a registration that
// runs beside authorization and token requests does not race them (run with
// -race).
func TestOAuthHandler_CredentialsAreReplacedAndReadAsOneRecord(t *testing.T) {
	server := &tokenAuthServer{registration: map[string]any{"client_id": "new", "client_secret": "new-secret", "token_endpoint_auth_method": "client_secret_basic"}, codeChallengeMethods: []string{"S256"}}
	httpServer := server.start(t)
	handler := NewOAuthHandler(OAuthConfig{
		ClientID:              "old",
		ClientSecret:          "old-secret",
		RedirectURI:           "http://localhost:8085/callback",
		TokenStore:            NewMemoryTokenStore(),
		AuthServerMetadataURL: httpServer.URL + "/.well-known/oauth-authorization-server",
	})
	handler.SetBaseURL(httpServer.URL + "/mcp")

	var wg sync.WaitGroup
	for _, use := range []func(){
		func() { _ = handler.RegisterClient(t.Context(), "test") },
		func() { _, _ = handler.GetAuthorizationURL(t.Context(), "state", "challenge") },
		func() { _, _ = handler.RefreshToken(t.Context(), "refresh") },
		func() { _ = handler.GetClientID() },
		func() { _ = handler.GetClientSecret() },
		func() { _ = handler.GetTokenEndpointAuthMethod() },
	} {
		wg.Go(use)
	}
	wg.Wait()
	assert.Equal(t, "new", handler.GetClientID())
	assert.Equal(t, "new-secret", handler.GetClientSecret())
}

// Without protected resource metadata, a server built to the 2025-03-26
// revision is its own authorization server, at its origin. Metadata found
// there that names another issuer is not used (RFC 8414 §3.3), so a server
// cannot claim the issuer the credentials are bound to and receive them.
func TestOAuthHandler_FallbackMetadataMustNameTheServersOwnOrigin(t *testing.T) {
	const theirs = "https://real-as.example"
	var mu sync.Mutex
	var secrets []string
	var evil *httptest.Server
	evil = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server", "/.well-known/oauth-authorization-server/mcp":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                           theirs,
				"authorization_endpoint":           evil.URL + "/authorize",
				"token_endpoint":                   evil.URL + "/token",
				"code_challenge_methods_supported": []string{"S256"},
			})
		case "/token":
			_ = r.ParseForm()
			_, basic, _ := r.BasicAuth()
			mu.Lock()
			secrets = append(secrets, r.PostForm.Get("client_secret")+basic)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "token", "token_type": "Bearer"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(evil.Close)
	handler := NewOAuthHandler(OAuthConfig{ClientID: "client", ClientSecret: "secret", ClientIssuer: theirs, RedirectURI: "http://localhost:8085/callback", TokenStore: NewMemoryTokenStore()})
	handler.SetBaseURL(evil.URL + "/mcp")

	_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.Error(t, err, "metadata at the server's origin claimed another issuer")
	handler.SetExpectedState("state")
	handler.SetExpectedIssuer(theirs, false)
	_ = handler.ProcessAuthorizationCallback(t.Context(), callback(theirs, "code", "state"), "verifier")
	mu.Lock()
	defer mu.Unlock()
	assert.Empty(t, secrets, "the client secret went to the MCP server's token endpoint")
}

// On a shared host, metadata under one MCP server's path cannot speak for
// the host's root authorization server (RFC 8414 §3.3 binds the issuer to
// the URL the metadata came from): a document there that claims the root
// issuer is not used, and the root's own metadata is, so a tenant cannot
// take the root issuer's client credentials.
func TestOAuthHandler_TenantMetadataCannotClaimTheRootIssuer(t *testing.T) {
	var host *httptest.Server
	host = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server/tenant/mcp":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                           host.URL,
				"authorization_endpoint":           host.URL + "/tenant/authorize",
				"token_endpoint":                   host.URL + "/tenant/token",
				"code_challenge_methods_supported": []string{"S256"},
			})
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                           host.URL,
				"authorization_endpoint":           host.URL + "/authorize",
				"token_endpoint":                   host.URL + "/token",
				"code_challenge_methods_supported": []string{"S256"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(host.Close)
	handler := NewOAuthHandler(OAuthConfig{ClientID: "client", ClientSecret: "secret", ClientIssuer: host.URL, RedirectURI: "http://localhost:8085/callback", TokenStore: NewMemoryTokenStore()})
	handler.SetBaseURL(host.URL + "/tenant/mcp")

	metadata, err := handler.GetServerMetadata(t.Context())
	require.NoError(t, err)
	assert.Equal(t, host.URL+"/token", metadata.TokenEndpoint, "the tenant's metadata spoke for the root issuer")
}
