package transport

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	protectedResourcePath = "/.well-known/oauth-protected-resource"
	authServerPath        = "/.well-known/oauth-authorization-server"
)

// sizedOAuthServer is an MCP server that is its own authorization server. Its
// response at path is padded to size bytes; with failing, that response is an
// error whose description pads it. It counts the requests at each path.
type sizedOAuthServer struct {
	*httptest.Server
	mu   sync.Mutex
	hits map[string]int
}

func (s *sizedOAuthServer) hitsAt(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits[path]
}

func newSizedOAuthServer(t *testing.T, path string, size int, failing bool) *sizedOAuthServer {
	t.Helper()
	s := &sizedOAuthServer{hits: map[string]int{}}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits[r.URL.Path]++
		s.mu.Unlock()
		status := http.StatusOK
		var body map[string]any
		switch r.URL.Path {
		case protectedResourcePath:
			body = map[string]any{"resource": s.URL, "authorization_servers": []string{s.URL}}
		case authServerPath:
			body = map[string]any{
				"issuer":                           s.URL,
				"authorization_endpoint":           s.URL + "/authorize",
				"token_endpoint":                   s.URL + "/token",
				"registration_endpoint":            s.URL + "/register",
				"code_challenge_methods_supported": []string{"S256"},
			}
		case "/register":
			status = http.StatusCreated
			body = map[string]any{"client_id": "client"}
		case "/token":
			body = map[string]any{"access_token": "access", "token_type": "Bearer", "refresh_token": "refresh"}
		default:
			http.NotFound(w, r)
			return
		}
		pad := ""
		if r.URL.Path == path {
			pad = "padding"
			if failing {
				status = http.StatusBadRequest
				body = map[string]any{"error": "invalid_request"}
				pad = "error_description"
			}
		}
		b, err := paddedJSON(body, pad, size)
		if err != nil {
			t.Errorf("%s: %v", r.URL.Path, err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(b)
	}))
	t.Cleanup(s.Close)
	return s
}

// paddedJSON is body as JSON, exactly size bytes long by its member pad, or
// as it is when pad is empty.
func paddedJSON(body map[string]any, pad string, size int) ([]byte, error) {
	if pad == "" {
		return json.Marshal(body)
	}
	body[pad] = ""
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	if len(b) > size {
		return nil, fmt.Errorf("the body is %d bytes before padding, over %d", len(b), size)
	}
	body[pad] = strings.Repeat("x", size-len(b))
	return json.Marshal(body)
}

// What a server sends the client is read up to a bound: a response past it
// fails with ErrMessageTooLarge, and an error body past its own bound is cut,
// so a hostile server cannot make the client read without end.
func TestOAuthHandler_ResponsesAreReadUpToABound(t *testing.T) {
	newHandler := func(server *sizedOAuthServer, authServerMetadata bool) *OAuthHandler {
		config := OAuthConfig{
			ClientID:     "client",
			ClientIssuer: server.URL,
			RedirectURI:  "http://localhost:8085/callback",
			TokenStore:   NewMemoryTokenStore(),
		}
		// Discovery moves past a metadata document it cannot use to the
		// default endpoints; one that is configured is used or fails.
		if authServerMetadata {
			config.AuthServerMetadataURL = server.URL + authServerPath
		}
		handler := NewOAuthHandler(config)
		handler.SetBaseURL(server.URL)
		return handler
	}
	getMetadata := func(h *OAuthHandler, _ *sizedOAuthServer) error {
		_, err := h.GetServerMetadata(t.Context())
		return err
	}
	register := func(h *OAuthHandler, _ *sizedOAuthServer) error { return h.RegisterClient(t.Context(), "test") }
	refresh := func(h *OAuthHandler, _ *sizedOAuthServer) error {
		_, err := h.RefreshToken(t.Context(), "refresh")
		return err
	}
	exchange := func(h *OAuthHandler, server *sizedOAuthServer) error {
		if _, err := h.GetAuthorizationURL(t.Context(), "state", "challenge"); err != nil {
			return err
		}
		return h.ProcessAuthorizationCallback(t.Context(), callback(server.URL, "code", "state"), "verifier")
	}

	for _, site := range []struct {
		name               string
		path               string
		authServerMetadata bool
		call               func(*OAuthHandler, *sizedOAuthServer) error
	}{
		{"protected resource metadata", protectedResourcePath, false, getMetadata},
		{"authorization server metadata", authServerPath, true, getMetadata},
		{"registration", "/register", false, register},
		{"token, refreshed", "/token", false, refresh},
		{"token, from a code", "/token", false, exchange},
	} {
		t.Run(site.name+"/exactly the bound", func(t *testing.T) {
			server := newSizedOAuthServer(t, site.path, maxOAuthBodyBytes, false)
			require.NoError(t, site.call(newHandler(server, site.authServerMetadata), server))
			assert.Equal(t, 1, server.hitsAt(site.path), "requests at %s", site.path)
		})
		t.Run(site.name+"/over the bound", func(t *testing.T) {
			server := newSizedOAuthServer(t, site.path, maxOAuthBodyBytes+1, false)
			err := site.call(newHandler(server, site.authServerMetadata), server)
			require.ErrorIs(t, err, ErrMessageTooLarge)
			assert.Equal(t, 1, server.hitsAt(site.path), "requests at %s", site.path)
		})
	}

	for _, tt := range []struct {
		name string
		path string
		call func(*OAuthHandler, *sizedOAuthServer) error
	}{
		{"registration error", "/register", register},
		{"token error", "/token", refresh},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := newSizedOAuthServer(t, tt.path, 2*maxOAuthBodyBytes, true)
			err := tt.call(newHandler(server, false), server)
			require.Error(t, err)
			assert.Equal(t, 1, server.hitsAt(tt.path), "requests at %s", tt.path)
			assert.LessOrEqual(t, len(err.Error()), maxErrorBodyBytes+512, "the whole error body was read")
		})
	}
}
