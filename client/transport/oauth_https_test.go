package transport

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestMain trusts the certificate every httptest TLS server shares, so the
// tests' authorization servers, which are HTTPS only, are reachable through
// the default transport the handler and the transports use.
func TestMain(m *testing.M) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	server.Close()
	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: roots}
	os.Exit(m.Run())
}

// Authorization server endpoints are served over HTTPS (MCP authorization,
// Communication Security): metadata naming an http issuer or endpoint is
// refused, and so is metadata fetched over http.
func TestOAuthHandler_AuthorizationServerEndpointsAreHTTPSOnly(t *testing.T) {
	for _, field := range []string{"issuer", "authorization_endpoint", "token_endpoint", "registration_endpoint"} {
		t.Run(field, func(t *testing.T) {
			var as *httptest.Server
			as = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				metadata := map[string]any{
					"issuer":                           as.URL,
					"authorization_endpoint":           as.URL + "/authorize",
					"token_endpoint":                   as.URL + "/token",
					"registration_endpoint":            as.URL + "/register",
					"code_challenge_methods_supported": []string{"S256"},
				}
				metadata[field] = strings.Replace(metadata[field].(string), "https://", "http://", 1)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(metadata)
			}))
			t.Cleanup(as.Close)
			handler := NewOAuthHandler(OAuthConfig{
				ClientID:              "client",
				TokenStore:            NewMemoryTokenStore(),
				AuthServerMetadataURL: as.URL + "/.well-known/oauth-authorization-server",
			})
			handler.SetBaseURL("https://mcp.example/mcp")

			_, err := handler.GetServerMetadata(t.Context())
			require.ErrorIs(t, err, ErrInsecureAuthorizationServer)
		})
	}
	t.Run("metadata URL", func(t *testing.T) {
		var requests atomic.Int32
		plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			http.NotFound(w, r)
		}))
		t.Cleanup(plain.Close)
		handler := NewOAuthHandler(OAuthConfig{
			ClientID:              "client",
			TokenStore:            NewMemoryTokenStore(),
			AuthServerMetadataURL: plain.URL + "/.well-known/oauth-authorization-server",
		})
		handler.SetBaseURL("https://mcp.example/mcp")

		_, err := handler.GetServerMetadata(t.Context())
		require.ErrorIs(t, err, ErrInsecureAuthorizationServer)
		require.Zero(t, requests.Load(), "the metadata was requested over http")
	})
}

// The HTTPS rule holds across redirects: a request carrying credentials,
// a code or a refresh token follows none, and discovery follows none to
// http, so nothing reaches a plain-http endpoint a redirect names.
func TestOAuthHandler_RedirectsDoNotLeaveHTTPS(t *testing.T) {
	var reached atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"token","token_type":"Bearer","client_id":"client"}`))
	}))
	t.Cleanup(plain.Close)
	var as *httptest.Server
	as = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metadata":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                           as.URL,
				"authorization_endpoint":           as.URL + "/authorize",
				"token_endpoint":                   as.URL + "/token",
				"registration_endpoint":            as.URL + "/register",
				"code_challenge_methods_supported": []string{"S256"},
			})
		case "/token", "/register":
			http.Redirect(w, r, plain.URL+r.URL.Path, http.StatusTemporaryRedirect)
		case "/downgraded":
			http.Redirect(w, r, plain.URL+"/metadata", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(as.Close)
	newHandler := func(metadataPath string, store TokenStore) *OAuthHandler {
		handler := NewOAuthHandler(OAuthConfig{
			ClientID:              "client",
			ClientSecret:          "secret",
			RedirectURI:           "http://localhost:8085/callback",
			TokenStore:            store,
			AuthServerMetadataURL: as.URL + metadataPath,
		})
		handler.SetBaseURL(as.URL + "/mcp")
		return handler
	}

	t.Run("code exchange", func(t *testing.T) {
		handler := newHandler("/metadata", NewMemoryTokenStore())
		handler.SetExpectedState("state")
		require.Error(t, handler.ProcessAuthorizationCallback(t.Context(), callback(as.URL, "code", "state"), "verifier"))
	})
	t.Run("refresh", func(t *testing.T) {
		store := NewMemoryTokenStore()
		require.NoError(t, store.SaveToken(t.Context(), &Token{AccessToken: "old", RefreshToken: "refresh", Resource: as.URL + "/mcp", Issuer: as.URL}))
		_, err := newHandler("/metadata", store).RefreshToken(t.Context(), "refresh")
		require.Error(t, err)
	})
	t.Run("registration", func(t *testing.T) {
		require.Error(t, newHandler("/metadata", NewMemoryTokenStore()).RegisterClient(t.Context(), "test"))
	})
	t.Run("metadata", func(t *testing.T) {
		_, err := newHandler("/downgraded", NewMemoryTokenStore()).GetServerMetadata(t.Context())
		require.ErrorIs(t, err, ErrInsecureAuthorizationServer)
	})
	require.Zero(t, reached.Load(), "a redirect sent a request to plain http")
}
