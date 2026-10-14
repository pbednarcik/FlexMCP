package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Authorization server metadata must name its issuer (RFC 8414 §2):
// metadata without one is not used, so client credentials are never left
// bound to no issuer and taken to whichever server comes next.
func TestOAuthHandler_MetadataWithoutAnIssuerIsNotUsed(t *testing.T) {
	var tokenRequests atomic.Int32
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"authorization_endpoint":           server.URL + "/authorize",
				"token_endpoint":                   server.URL + "/token",
				"code_challenge_methods_supported": []string{"S256"},
			})
		case "/token":
			tokenRequests.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "a", "token_type": "Bearer"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	handler := NewOAuthHandler(OAuthConfig{
		ClientID:              "client",
		ClientSecret:          "secret",
		RedirectURI:           "http://localhost:8085/callback",
		TokenStore:            NewMemoryTokenStore(),
		AuthServerMetadataURL: server.URL + "/.well-known/oauth-authorization-server",
	})
	handler.SetBaseURL(server.URL + "/mcp")

	_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.Error(t, err)
	handler.SetExpectedState("state")
	require.Error(t, handler.ProcessAuthorizationResponse(t.Context(), "code", "state", "verifier"))
	assert.Empty(t, handler.GetClientIssuer())
	assert.Zero(t, tokenRequests.Load(), "a token request went to an authorization server without an issuer")
}

// A refresh token with no record of its issuer and no ClientIssuer is
// refused before any discovery: the caller is told to authorize, whatever
// discovery would have answered.
func TestOAuthHandler_AnUnknownRefreshTokenIsRefusedBeforeDiscovery(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	handler := NewOAuthHandler(OAuthConfig{
		ClientID:              "client",
		TokenStore:            NewMemoryTokenStore(),
		AuthServerMetadataURL: server.URL + "/.well-known/oauth-authorization-server",
	})
	handler.SetBaseURL(server.URL + "/mcp")

	_, err := handler.RefreshToken(t.Context(), "opaque")
	require.ErrorIs(t, err, ErrOAuthAuthorizationRequired)
	assert.Zero(t, requests.Load(), "discovery ran for a refresh token that cannot be sent anywhere")
}

// A refresh answered after the handler moved to another authorization
// server is not recorded: it does not replace the token of the server the
// handler targets now, and the caller is told to authorize.
func TestOAuthHandler_ARefreshAnsweredAfterARetargetIsNotRecorded(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var first *httptest.Server
	first = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                           first.URL,
				"authorization_endpoint":           first.URL + "/authorize",
				"token_endpoint":                   first.URL + "/token",
				"code_challenge_methods_supported": []string{"S256"},
			})
		case "/token":
			close(entered)
			<-release
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "from-first", "token_type": "Bearer", "expires_in": 3600})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(first.Close)
	second := (&tokenAuthServer{codeChallengeMethods: []string{"S256"}}).start(t)
	resource, point := switchingResource(t, first.URL)
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

	done := make(chan error, 1)
	go func() {
		_, err := handler.GetAuthorizationHeader(t.Context())
		done <- err
	}()
	<-entered
	point(second.URL)
	handler.SetProtectedResourceMetadataURL(resource.URL + "/.well-known/oauth-protected-resource")
	_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.NoError(t, err)
	close(release)

	require.ErrorIs(t, <-done, ErrOAuthAuthorizationRequired)
	stored, err := store.GetToken(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "old", stored.AccessToken, "a refresh from the previous authorization server was recorded")
}

// gatedSaveStore blocks its first save until release, after signalling
// entered; later saves go straight through, even while the first waits.
type gatedSaveStore struct {
	TokenStore
	gated            atomic.Bool
	entered, release chan struct{}
}

func (s *gatedSaveStore) SaveToken(ctx context.Context, token *Token) error {
	if s.gated.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.release
	}
	return s.TokenStore.SaveToken(ctx, token)
}

// A token is used only with the authorization server that issued it: one
// saved while the handler moved to another is not sent, to the caller that
// waited for it or to any later one.
func TestOAuthHandler_ATokenSavedAcrossARetargetIsNotUsed(t *testing.T) {
	first := (&tokenAuthServer{codeChallengeMethods: []string{"S256"}}).start(t)
	second := (&tokenAuthServer{codeChallengeMethods: []string{"S256"}}).start(t)
	resource, point := switchingResource(t, first.URL)
	store := &gatedSaveStore{TokenStore: NewMemoryTokenStore(), entered: make(chan struct{}), release: make(chan struct{})}
	require.NoError(t, store.TokenStore.SaveToken(t.Context(), &Token{
		AccessToken:  "old",
		RefreshToken: "refresh-from-first",
		ExpiresAt:    time.Now().Add(-time.Minute),
		Resource:     resource.URL,
		Issuer:       first.URL,
	}))
	handler := NewOAuthHandler(OAuthConfig{RedirectURI: "http://localhost:8085/callback", TokenStore: store})
	handler.SetBaseURL(resource.URL)

	done := make(chan error, 1)
	go func() {
		_, err := handler.GetAuthorizationHeader(t.Context())
		done <- err
	}()
	<-store.entered
	point(second.URL)
	handler.SetProtectedResourceMetadataURL(resource.URL + "/.well-known/oauth-protected-resource")
	_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.NoError(t, err)
	close(store.release)

	require.ErrorIs(t, <-done, ErrOAuthAuthorizationRequired)
	_, err = handler.GetAuthorizationHeader(t.Context())
	require.ErrorIs(t, err, ErrOAuthAuthorizationRequired)
}

// reentrantStore calls back into the handler from inside its first save,
// through refresh.
type reentrantStore struct {
	TokenStore
	handler *OAuthHandler
	refresh func(context.Context, *OAuthHandler, *Token) error
	called  atomic.Bool
	nested  error
}

func (s *reentrantStore) SaveToken(ctx context.Context, token *Token) error {
	if err := s.TokenStore.SaveToken(ctx, token); err != nil {
		return err
	}
	if s.called.CompareAndSwap(false, true) {
		s.nested = s.refresh(ctx, s.handler, token)
	}
	return nil
}

// A token store that calls back into the handler from SaveToken gets an
// error for the nested request instead of waiting for itself, and the save
// it was called from completes.
func TestOAuthHandler_ATokenStoreThatCallsBackFromSaveTokenFailsFast(t *testing.T) {
	for name, refresh := range map[string]func(context.Context, *OAuthHandler, *Token) error{
		"RefreshToken": func(ctx context.Context, h *OAuthHandler, token *Token) error {
			_, err := h.RefreshToken(ctx, token.RefreshToken)
			return err
		},
		"GetAuthorizationHeader": func(ctx context.Context, h *OAuthHandler, _ *Token) error {
			_, err := h.GetAuthorizationHeader(ctx)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			server := newRefreshServer(t, singleUseTokens(0))
			store := &reentrantStore{TokenStore: &expiringStore{TokenStore: NewMemoryTokenStore()}, refresh: refresh}
			require.NoError(t, store.TokenStore.SaveToken(t.Context(), &Token{
				AccessToken:  "old-access",
				RefreshToken: "single-use",
				ExpiresAt:    time.Now().Add(-time.Minute),
				Issuer:       server.URL,
			}))
			handler := NewOAuthHandler(OAuthConfig{
				ClientID:              "client",
				TokenStore:            store,
				AuthServerMetadataURL: server.URL + "/.well-known/oauth-authorization-server",
			})
			handler.SetBaseURL(server.URL + "/mcp")
			store.handler = handler

			done := make(chan error, 1)
			go func() {
				_, err := handler.GetAuthorizationHeader(t.Context())
				done <- err
			}()
			select {
			case err := <-done:
				require.NoError(t, err)
				require.ErrorIs(t, store.nested, errSaveReentered)
			case <-time.After(5 * time.Second):
				t.Fatal("a token store that calls back from SaveToken deadlocked the handler")
			}
		})
	}
}

// expiringStore reads every token back as expired, so a nested request
// made while saving always wants a refresh.
type expiringStore struct{ TokenStore }

func (s *expiringStore) GetToken(ctx context.Context) (*Token, error) {
	token, err := s.TokenStore.GetToken(ctx)
	if err != nil {
		return nil, err
	}
	expired := *token
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	return &expired, nil
}

// A refresh that was saving while a newer authorization completed does not
// outlive it: the newer token is the one kept.
func TestOAuthHandler_ANewerAuthorizationOutlivesALateRefresh(t *testing.T) {
	first := (&tokenAuthServer{codeChallengeMethods: []string{"S256"}}).start(t)
	secondServer := &tokenAuthServer{codeChallengeMethods: []string{"S256"}}
	second := secondServer.start(t)
	resource, point := switchingResource(t, first.URL)
	store := &gatedSaveStore{TokenStore: NewMemoryTokenStore(), entered: make(chan struct{}), release: make(chan struct{})}
	require.NoError(t, store.TokenStore.SaveToken(t.Context(), &Token{
		AccessToken:  "old",
		RefreshToken: "refresh-from-first",
		ExpiresAt:    time.Now().Add(-time.Minute),
		Resource:     resource.URL,
		Issuer:       first.URL,
	}))
	handler := NewOAuthHandler(OAuthConfig{RedirectURI: "http://localhost:8085/callback", TokenStore: store})
	handler.SetBaseURL(resource.URL)

	refreshed := make(chan error, 1)
	go func() {
		_, err := handler.GetAuthorizationHeader(t.Context())
		refreshed <- err
	}()
	<-store.entered
	point(second.URL)
	handler.SetProtectedResourceMetadataURL(resource.URL + "/.well-known/oauth-protected-resource")
	_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.NoError(t, err)
	authorized := make(chan error, 1)
	go func() { authorized <- handler.ProcessAuthorizationResponse(t.Context(), "code", "state", "verifier") }()
	// Release the late refresh only once the newer authorization has its
	// token and has had the chance to save it.
	require.Eventually(t, func() bool {
		secondServer.mu.Lock()
		defer secondServer.mu.Unlock()
		return len(secondServer.forms) == 1
	}, 5*time.Second, time.Millisecond)
	for deadline := time.Now().Add(200 * time.Millisecond); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if stored, err := store.GetToken(t.Context()); err == nil && stored.Issuer == second.URL {
			break
		}
	}
	close(store.release)

	<-refreshed
	require.NoError(t, <-authorized)
	stored, err := store.GetToken(t.Context())
	require.NoError(t, err)
	assert.Equal(t, second.URL, stored.Issuer, "the late refresh overwrote the newer authorization")
	header, err := handler.GetAuthorizationHeader(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "Bearer access-token", header)
}

// A reset of discovery does not forget which issuer the handler moved to: a
// token from the previous one stays refused until discovery names one again.
func TestOAuthHandler_AResetKeepsRefusingAnotherIssuersToken(t *testing.T) {
	first := (&tokenAuthServer{codeChallengeMethods: []string{"S256"}}).start(t)
	second := (&tokenAuthServer{codeChallengeMethods: []string{"S256"}}).start(t)
	resource, _ := switchingResource(t, second.URL)
	store := NewMemoryTokenStore()
	require.NoError(t, store.SaveToken(t.Context(), &Token{
		AccessToken:  "from-first",
		RefreshToken: "refresh-from-first",
		ExpiresAt:    time.Now().Add(time.Hour),
		Resource:     resource.URL,
		Issuer:       first.URL,
	}))
	handler := NewOAuthHandler(OAuthConfig{RedirectURI: "http://localhost:8085/callback", TokenStore: store})
	handler.SetBaseURL(resource.URL)
	_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.NoError(t, err)
	_, err = handler.GetAuthorizationHeader(t.Context())
	require.ErrorIs(t, err, ErrOAuthAuthorizationRequired)

	handler.SetProtectedResourceMetadataURL(resource.URL + "/.well-known/oauth-protected-resource")
	_, err = handler.GetAuthorizationHeader(t.Context())
	require.ErrorIs(t, err, ErrOAuthAuthorizationRequired, "a reset re-enabled another issuer's token")
}

// A refresh made with client credentials that a registration has since
// replaced is not saved: its token and refresh token belong to the old
// client.
func TestOAuthHandler_ARefreshWithReplacedCredentialsIsNotSaved(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                           server.URL,
				"authorization_endpoint":           server.URL + "/authorize",
				"token_endpoint":                   server.URL + "/token",
				"registration_endpoint":            server.URL + "/register",
				"code_challenge_methods_supported": []string{"S256"},
			})
		case "/register":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "new-client"})
		case "/token":
			close(entered)
			<-release
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "old-client-token", "token_type": "Bearer", "expires_in": 3600})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	store := NewMemoryTokenStore()
	require.NoError(t, store.SaveToken(t.Context(), &Token{
		AccessToken:  "old",
		RefreshToken: "refresh",
		ExpiresAt:    time.Now().Add(-time.Minute),
		Issuer:       server.URL,
	}))
	handler := NewOAuthHandler(OAuthConfig{
		ClientID:              "old-client",
		RedirectURI:           "http://localhost:8085/callback",
		TokenStore:            store,
		AuthServerMetadataURL: server.URL + "/.well-known/oauth-authorization-server",
	})
	handler.SetBaseURL(server.URL + "/mcp")

	done := make(chan error, 1)
	go func() {
		_, err := handler.GetAuthorizationHeader(t.Context())
		done <- err
	}()
	<-entered
	require.NoError(t, handler.RegisterClient(t.Context(), "test"))
	close(release)

	require.ErrorIs(t, <-done, ErrOAuthAuthorizationRequired)
	stored, err := store.GetToken(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "old", stored.AccessToken, "a token for the replaced client was saved")
}
