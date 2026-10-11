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
	resource = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
