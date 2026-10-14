package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
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
		"RegisterClient": func(ctx context.Context, h *OAuthHandler, _ *Token) error {
			return h.RegisterClient(ctx, "nested")
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

// grantServer is an authorization server whose tokens name the grant they
// answer: a code buys "from-code", refresh token R buys "refreshed-R" and
// "rotated-R". Registration hands out "new-client".
type grantServer struct {
	*httptest.Server
	mu        sync.Mutex
	refreshes []url.Values
}

func newGrantServer(t *testing.T) *grantServer {
	s := &grantServer{}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                           s.URL,
				"authorization_endpoint":           s.URL + "/authorize",
				"token_endpoint":                   s.URL + "/token",
				"registration_endpoint":            s.URL + "/register",
				"code_challenge_methods_supported": []string{"S256"},
			})
		case "/register":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "new-client"})
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Errorf("parsing token request: %v", err)
			}
			access, refresh := "from-code", "refresh-from-code"
			if r.PostForm.Get("grant_type") == "refresh_token" {
				s.mu.Lock()
				s.refreshes = append(s.refreshes, r.PostForm)
				s.mu.Unlock()
				access, refresh = "refreshed-"+r.PostForm.Get("refresh_token"), "rotated-"+r.PostForm.Get("refresh_token")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "token_type": "Bearer", "expires_in": 3600, "refresh_token": refresh})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// refreshForms returns the forms of the refresh requests made so far.
func (s *grantServer) refreshForms() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]url.Values(nil), s.refreshes...)
}

// gatedReadStore blocks its gate-th GetToken, after it has read the token,
// until release, signalling entered.
type gatedReadStore struct {
	TokenStore
	gate             int32
	calls            atomic.Int32
	entered, release chan struct{}
}

func (s *gatedReadStore) GetToken(ctx context.Context) (*Token, error) {
	token, err := s.TokenStore.GetToken(ctx)
	if s.calls.Add(1) == s.gate {
		close(s.entered)
		<-s.release
	}
	return token, err
}

// A token request belongs to the generation of the token it read: a refresh
// that read the stored token before a newer authorization saved its own
// does not overwrite it.
func TestOAuthHandler_ARefreshThatReadBeforeANewerAuthorizationIsNotSaved(t *testing.T) {
	for name, tc := range map[string]struct {
		// gate is the GetToken that reads the token the refresh spends.
		gate    int32
		refresh func(context.Context, *OAuthHandler) error
	}{
		"GetAuthorizationHeader": {gate: 2, refresh: func(ctx context.Context, h *OAuthHandler) error {
			_, err := h.GetAuthorizationHeader(ctx)
			return err
		}},
		"RefreshToken": {gate: 1, refresh: func(ctx context.Context, h *OAuthHandler) error {
			_, err := h.RefreshToken(ctx, "refresh-A")
			return err
		}},
	} {
		t.Run(name, func(t *testing.T) {
			server := newGrantServer(t)
			store := &gatedReadStore{TokenStore: NewMemoryTokenStore(), gate: tc.gate, entered: make(chan struct{}), release: make(chan struct{})}
			require.NoError(t, store.SaveToken(t.Context(), &Token{
				AccessToken:  "A",
				RefreshToken: "refresh-A",
				ExpiresAt:    time.Now().Add(-time.Minute),
				Issuer:       server.URL,
			}))
			handler := NewOAuthHandler(OAuthConfig{
				ClientID:              "client",
				RedirectURI:           "http://localhost:8085/callback",
				TokenStore:            store,
				AuthServerMetadataURL: server.URL + "/.well-known/oauth-authorization-server",
			})
			handler.SetBaseURL(server.URL + "/mcp")

			done := make(chan error, 1)
			go func() { done <- tc.refresh(t.Context(), handler) }()
			<-store.entered
			_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
			require.NoError(t, err)
			require.NoError(t, handler.ProcessAuthorizationResponse(t.Context(), "code", "state", "verifier"))
			close(store.release)
			<-done

			stored, err := store.TokenStore.GetToken(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "from-code", stored.AccessToken, "a refresh of the older grant overwrote the newer authorization")
		})
	}
}

// A refresh token is spent only with the client it was issued to (RFC 6749
// §6): a registration does not land while a token of the client it replaces
// is being saved, and that token is not refreshed with the new client.
func TestOAuthHandler_ARefreshTokenIsSpentOnlyByItsClient(t *testing.T) {
	server := newGrantServer(t)
	store := &gatedSaveStore{TokenStore: &expiringStore{TokenStore: NewMemoryTokenStore()}, entered: make(chan struct{}), release: make(chan struct{})}
	require.NoError(t, store.TokenStore.SaveToken(t.Context(), &Token{
		AccessToken:  "A",
		RefreshToken: "refresh-A",
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
	<-store.entered
	registered := make(chan error, 1)
	go func() { registered <- handler.RegisterClient(t.Context(), "test") }()
	select {
	case err := <-registered:
		registered <- err
		t.Error("a registration completed while a token of the client it replaces was being saved")
	case <-time.After(100 * time.Millisecond):
	}
	close(store.release)
	require.NoError(t, <-done)
	require.NoError(t, <-registered)

	_, err := handler.GetAuthorizationHeader(t.Context())
	assert.ErrorIs(t, err, ErrOAuthAuthorizationRequired)
	for _, form := range server.refreshForms() {
		assert.Equal(t, "old-client", form.Get("client_id"), "refresh token %q was sent with another client's credentials", form.Get("refresh_token"))
	}
}
