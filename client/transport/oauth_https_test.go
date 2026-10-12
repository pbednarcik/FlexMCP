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
