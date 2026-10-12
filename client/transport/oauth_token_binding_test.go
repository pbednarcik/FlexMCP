package transport

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A token is for the MCP server and from the authorization server it was
// issued for (MCP authorization: clients send only tokens from the server's
// authorization server). The SDK records both when it saves a token, and
// does not hand a token for one server to a request for another.
func TestOAuthHandler_ATokenIsUsedOnlyForTheServerItWasIssuedFor(t *testing.T) {
	as := &tokenAuthServer{codeChallengeMethods: []string{"S256"}}
	asHTTP := as.start(t)
	store := NewMemoryTokenStore()
	handler := NewOAuthHandler(OAuthConfig{
		ClientID:              "client",
		RedirectURI:           "http://localhost:8085/callback",
		TokenStore:            store,
		AuthServerMetadataURL: asHTTP.URL + "/.well-known/oauth-authorization-server",
	})
	handler.SetBaseURL(asHTTP.URL + "/a")
	_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.NoError(t, err)
	require.NoError(t, handler.ProcessAuthorizationCallback(t.Context(), callback("", "code", "state"), "verifier"))

	saved, err := store.GetToken(t.Context())
	require.NoError(t, err)
	assert.Equal(t, asHTTP.URL+"/a", saved.Resource, "the token records the server it is for")
	assert.Equal(t, asHTTP.URL, saved.Issuer, "the token records the issuer it is from")
	header, err := handler.GetAuthorizationHeader(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "Bearer access-token", header)

	handler.SetBaseURL(asHTTP.URL + "/b")
	_, err = handler.GetAuthorizationHeader(t.Context())
	require.ErrorIs(t, err, ErrOAuthAuthorizationRequired, "server a's token is not sent to server b")
}

// A refresh token goes only to the authorization server that issued it: when
// discovery names another, the stored token is not refreshed there.
func TestOAuthHandler_ARefreshTokenGoesOnlyToItsIssuer(t *testing.T) {
	first := (&tokenAuthServer{codeChallengeMethods: []string{"S256"}}).start(t)
	second := &tokenAuthServer{codeChallengeMethods: []string{"S256"}}
	secondHTTP := second.start(t)
	resource, _ := switchingResource(t, secondHTTP.URL)
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

	_, err := handler.GetAuthorizationHeader(t.Context())
	require.ErrorIs(t, err, ErrOAuthAuthorizationRequired)
	_, err = handler.RefreshToken(t.Context(), "refresh-from-first")
	require.ErrorIs(t, err, ErrIssuerMismatch)
	second.mu.Lock()
	defer second.mu.Unlock()
	assert.Empty(t, second.forms, "the refresh token went to another issuer")
}

// A token saved without a record of its server, as one an application put in
// the store itself or one saved before the record existed, is still used.
func TestOAuthHandler_ATokenWithoutARecordIsStillUsed(t *testing.T) {
	store := NewMemoryTokenStore()
	require.NoError(t, store.SaveToken(t.Context(), &Token{AccessToken: "app-token", ExpiresAt: time.Now().Add(time.Hour)}))
	handler := NewOAuthHandler(OAuthConfig{TokenStore: store})
	handler.SetBaseURL("https://mcp.example/mcp")

	header, err := handler.GetAuthorizationHeader(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "Bearer app-token", header)
}

// flakyTokenStore answers its first good reads and fails every read after.
type flakyTokenStore struct {
	TokenStore
	good  int
	mu    sync.Mutex
	reads int
}

func (s *flakyTokenStore) GetToken(ctx context.Context) (*Token, error) {
	s.mu.Lock()
	s.reads++
	failing := s.reads > s.good
	s.mu.Unlock()
	if failing {
		return nil, errors.New("store unavailable")
	}
	return s.TokenStore.GetToken(ctx)
}

// A refresh token is checked against the record it was read with: a store
// that fails a later read does not let it go to another issuer unchecked.
func TestOAuthHandler_ARefreshTokenIsNotSentWhenItsRecordCannotBeRead(t *testing.T) {
	for _, tt := range []struct {
		name    string
		good    int
		refresh func(*OAuthHandler) error
	}{
		{"the shared refresh", 2, func(h *OAuthHandler) error {
			_, err := h.GetAuthorizationHeader(t.Context())
			return err
		}},
		{"RefreshToken", 0, func(h *OAuthHandler) error {
			_, err := h.RefreshToken(t.Context(), "refresh-from-first")
			return err
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			first := (&tokenAuthServer{codeChallengeMethods: []string{"S256"}}).start(t)
			second := &tokenAuthServer{codeChallengeMethods: []string{"S256"}}
			secondHTTP := second.start(t)
			resource, _ := switchingResource(t, secondHTTP.URL)
			store := &flakyTokenStore{TokenStore: NewMemoryTokenStore(), good: tt.good}
			require.NoError(t, store.SaveToken(t.Context(), &Token{
				AccessToken:  "old",
				RefreshToken: "refresh-from-first",
				ExpiresAt:    time.Now().Add(-time.Minute),
				Resource:     resource.URL,
				Issuer:       first.URL,
			}))
			handler := NewOAuthHandler(OAuthConfig{RedirectURI: "http://localhost:8085/callback", TokenStore: store})
			handler.SetBaseURL(resource.URL)

			require.Error(t, tt.refresh(handler))
			second.mu.Lock()
			defer second.mu.Unlock()
			assert.Empty(t, second.forms, "the refresh token went to another issuer")
		})
	}
}

// scriptedTokenStore answers its reads in order, the last one from then on.
type scriptedTokenStore struct {
	mu    sync.Mutex
	reads []*Token
}

func (s *scriptedTokenStore) GetToken(context.Context) (*Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	token := s.reads[0]
	if len(s.reads) > 1 {
		s.reads = s.reads[1:]
	}
	return token, nil
}

func (s *scriptedTokenStore) SaveToken(context.Context, *Token) error { return nil }

// What a refresh hands back is checked like a token read from the store: a
// token another refresh left there for another server is not sent here.
func TestOAuthHandler_ARefreshResultIsUsedOnlyForThisServer(t *testing.T) {
	store := &scriptedTokenStore{reads: []*Token{
		{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Minute), Resource: "https://mcp.example/a"},
		{AccessToken: "for-b", ExpiresAt: time.Now().Add(time.Hour), Resource: "https://mcp.example/b"},
	}}
	handler := NewOAuthHandler(OAuthConfig{TokenStore: store})
	handler.SetBaseURL("https://mcp.example/a")

	header, err := handler.GetAuthorizationHeader(t.Context())
	require.ErrorIs(t, err, ErrOAuthAuthorizationRequired, "got %q", header)
}
