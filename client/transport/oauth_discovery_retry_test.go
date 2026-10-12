package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// flakyMetadataServer serves authorization server metadata, failing the first
// failures requests with 503, and counts the requests it gets.
func flakyMetadataServer(t *testing.T, failures int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) <= failures {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 server.URL,
			"authorization_endpoint": server.URL + "/authorize",
			"token_endpoint":         server.URL + "/token",
		})
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func newDiscoveryTestHandler(server *httptest.Server) *OAuthHandler {
	return NewOAuthHandler(OAuthConfig{
		ClientID:              "client",
		RedirectURI:           "http://localhost/callback",
		AuthServerMetadataURL: server.URL + "/.well-known/oauth-authorization-server",
	})
}

// Metadata discovery that failed is tried again by the next call, rather
// than the failure being returned for the rest of the handler's life.
func TestOAuthHandler_DiscoveryRetriesAfterFailure(t *testing.T) {
	t.Run("cancelled context", func(t *testing.T) {
		server, _ := flakyMetadataServer(t, 0)
		handler := newDiscoveryTestHandler(server)

		cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := handler.GetServerMetadata(cancelled)
		require.ErrorIs(t, err, context.Canceled)

		metadata, err := handler.GetServerMetadata(t.Context())
		require.NoError(t, err)
		assert.Equal(t, server.URL+"/token", metadata.TokenEndpoint)
	})
	t.Run("cancelled context, then no metadata", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusNotFound)
		}))
		t.Cleanup(server.Close)
		handler := newDiscoveryTestHandler(server)

		cancelled, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := handler.GetServerMetadata(cancelled)
		require.ErrorIs(t, err, context.Canceled)

		// The next discovery fails for a reason of its own.
		_, err = handler.GetServerMetadata(t.Context())
		require.Error(t, err)
		assert.NotErrorIs(t, err, context.Canceled)
		assert.Contains(t, err.Error(), "returned no metadata")
		assert.Equal(t, int32(1), requests.Load())
	})
	t.Run("server error", func(t *testing.T) {
		server, requests := flakyMetadataServer(t, 1)
		handler := newDiscoveryTestHandler(server)

		_, err := handler.GetServerMetadata(t.Context())
		require.Error(t, err)

		metadata, err := handler.GetServerMetadata(t.Context())
		require.NoError(t, err)
		assert.Equal(t, server.URL+"/token", metadata.TokenEndpoint)
		assert.Equal(t, int32(2), requests.Load())
	})
}

// Metadata that was discovered is kept.
func TestOAuthHandler_DiscoveryKeepsMetadata(t *testing.T) {
	server, requests := flakyMetadataServer(t, 0)
	handler := newDiscoveryTestHandler(server)

	for range 3 {
		_, err := handler.GetServerMetadata(t.Context())
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), requests.Load())
}

// A discovery fetch that got no answer, a server error or a request that
// ended, is reported and discovered again by the next call, not taken for
// metadata that is not there: guessed endpoints would be kept for the
// handler's life, and the credentials bound to a guessed issuer.
func TestOAuthHandler_AFailedMetadataFetchIsNotTakenForAbsentMetadata(t *testing.T) {
	for _, tt := range []struct {
		name            string
		failPRM, failAS bool
		cancelled       bool
	}{
		{"protected resource metadata, server error", true, false, false},
		{"authorization server metadata, server error", false, true, false},
		{"authorization server metadata, request ended", false, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var failing atomic.Bool
			failing.Store(true)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var as *httptest.Server
			as = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/.well-known/oauth-authorization-server" {
					http.NotFound(w, r)
					return
				}
				if tt.failAS && failing.Load() {
					if tt.cancelled {
						cancel()
						<-r.Context().Done()
						return
					}
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"issuer":                           as.URL,
					"authorization_endpoint":           as.URL + "/authorize",
					"token_endpoint":                   as.URL + "/real-token",
					"code_challenge_methods_supported": []string{"S256"},
				})
			}))
			t.Cleanup(as.Close)
			var resource *httptest.Server
			resource = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/.well-known/oauth-protected-resource" {
					http.NotFound(w, r)
					return
				}
				if tt.failPRM && failing.Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"resource": resource.URL, "authorization_servers": []string{as.URL}})
			}))
			t.Cleanup(resource.Close)
			handler := NewOAuthHandler(OAuthConfig{TokenStore: NewMemoryTokenStore()})
			handler.SetBaseURL(resource.URL)

			_, err := handler.GetServerMetadata(ctx)
			require.Error(t, err, "a failed fetch was taken for absent metadata")
			failing.Store(false)
			metadata, err := handler.GetServerMetadata(t.Context())
			require.NoError(t, err)
			assert.Equal(t, as.URL+"/real-token", metadata.TokenEndpoint)
		})
	}
}
