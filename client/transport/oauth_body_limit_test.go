package transport

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oversizedServer is an MCP server that is its own authorization server, and
// pads the response named by oversized past what the client reads.
func oversizedServer(t *testing.T, oversized string) *httptest.Server {
	t.Helper()
	pad := strings.Repeat("x", 2*maxMetadataBodyBytes)
	padded := func(name string) string {
		if name == oversized {
			return pad
		}
		return ""
	}
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource":
			body = map[string]any{"resource": server.URL, "authorization_servers": []string{server.URL}, "resource_name": padded("protected resource metadata")}
		case "/.well-known/oauth-authorization-server":
			body = map[string]any{
				"issuer":                           server.URL,
				"authorization_endpoint":           server.URL + "/authorize",
				"token_endpoint":                   server.URL + "/token",
				"registration_endpoint":            server.URL + "/register",
				"code_challenge_methods_supported": []string{"S256"},
			}
		case "/register":
			if oversized == "registration error" {
				w.WriteHeader(http.StatusBadRequest)
				body = map[string]any{"error": "invalid_client_metadata", "error_description": pad}
				break
			}
			w.WriteHeader(http.StatusCreated)
			body = map[string]any{"client_id": "client", "client_name": padded("registration")}
		case "/token":
			if oversized == "token error" {
				w.WriteHeader(http.StatusBadRequest)
				body = map[string]any{"error": "invalid_grant", "error_description": pad}
				break
			}
			body = map[string]any{"access_token": "access", "token_type": "Bearer", "refresh_token": "refresh", "scope": padded("token")}
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(server.Close)
	return server
}

// What a server sends the client is read up to a bound, like authorization
// server metadata: a response past it is refused, and an error body past it
// is cut, so a hostile server cannot make the client read without end.
func TestOAuthHandler_ResponsesAreReadUpToABound(t *testing.T) {
	newHandler := func(server *httptest.Server) *OAuthHandler {
		handler := NewOAuthHandler(OAuthConfig{ClientID: "client", RedirectURI: "http://localhost:8085/callback", TokenStore: NewMemoryTokenStore()})
		handler.SetBaseURL(server.URL)
		return handler
	}
	t.Run("protected resource metadata", func(t *testing.T) {
		_, err := newHandler(oversizedServer(t, "protected resource metadata")).GetServerMetadata(t.Context())
		require.Error(t, err)
	})
	t.Run("registration", func(t *testing.T) {
		require.Error(t, newHandler(oversizedServer(t, "registration")).RegisterClient(t.Context(), "test"))
	})
	t.Run("token, refreshed", func(t *testing.T) {
		_, err := newHandler(oversizedServer(t, "token")).RefreshToken(t.Context(), "refresh")
		require.Error(t, err)
	})
	t.Run("token, from a code", func(t *testing.T) {
		server := oversizedServer(t, "token")
		handler := newHandler(server)
		_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
		require.NoError(t, err)
		require.Error(t, handler.ProcessAuthorizationCallback(t.Context(), callback(server.URL, "code", "state"), "verifier"))
	})
	for _, tt := range []struct {
		name string
		call func(*OAuthHandler) error
	}{
		{"registration error", func(h *OAuthHandler) error { return h.RegisterClient(t.Context(), "test") }},
		{"token error", func(h *OAuthHandler) error {
			_, err := h.RefreshToken(t.Context(), "refresh")
			return err
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call(newHandler(oversizedServer(t, tt.name)))
			require.Error(t, err)
			assert.LessOrEqual(t, len(err.Error()), maxMetadataBodyBytes+512, "the whole error body was read")
		})
	}
}
