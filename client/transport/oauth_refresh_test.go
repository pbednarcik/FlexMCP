package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// refreshServer serves authorization server metadata and a token endpoint
// that answers refresh requests with token, and counts them.
type refreshServer struct {
	*httptest.Server
	refreshes atomic.Int32
}

func newRefreshServer(t *testing.T, token func(w http.ResponseWriter, refreshToken string)) *refreshServer {
	t.Helper()
	s := &refreshServer{}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                 s.URL,
				"authorization_endpoint": s.URL + "/authorize",
				"token_endpoint":         s.URL + "/token",
			})
		case "/token":
			s.refreshes.Add(1)
			_ = r.ParseForm()
			token(w, r.FormValue("refresh_token"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// singleUseTokens answers like a server whose refresh tokens are good for one
// use: it spends a refresh token as soon as the request arrives, answers
// after delay, and rejects a spent one.
func singleUseTokens(delay time.Duration) func(http.ResponseWriter, string) {
	var mu sync.Mutex
	spent := map[string]bool{}
	return func(w http.ResponseWriter, refreshToken string) {
		mu.Lock()
		reused := spent[refreshToken]
		spent[refreshToken] = true
		mu.Unlock()
		time.Sleep(delay)
		if reused {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-access",
			"token_type":    "bearer",
			"expires_in":    3600,
			"refresh_token": "next-" + refreshToken,
		})
	}
}

// newRefreshTestHandler returns a handler whose stored access token has
// expired and whose refresh token is "single-use", recorded as the
// server's.
func newRefreshTestHandler(t *testing.T, server *refreshServer) (*OAuthHandler, *MemoryTokenStore) {
	t.Helper()
	store := NewMemoryTokenStore()
	require.NoError(t, store.SaveToken(t.Context(), &Token{
		AccessToken:  "old-access",
		TokenType:    "Bearer",
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
	return handler, store
}

// Callers that find the access token expired at the same time share one
// refresh. With a refresh token that is good for one use, as GitHub's are, a
// refresh per caller fails for all but the first, and those callers were sent
// back to authorize although a new token was on its way.
func TestOAuthHandler_ConcurrentCallersRefreshOnce(t *testing.T) {
	// The token endpoint answers slowly, so that every caller finds the token
	// expired before the first refresh completes.
	server := newRefreshServer(t, singleUseTokens(100*time.Millisecond))
	handler, store := newRefreshTestHandler(t, server)

	const callers = 8
	headers := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			headers[i], errs[i] = handler.GetAuthorizationHeader(t.Context())
		})
	}
	wg.Wait()

	for i := range callers {
		require.NoError(t, errs[i], "caller %d", i)
		assert.Equal(t, "Bearer new-access", headers[i], "caller %d", i)
	}
	assert.Equal(t, int32(1), server.refreshes.Load(), "refresh requests")
	saved, err := store.GetToken(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "next-single-use", saved.RefreshToken)
}

// A refresh doesn't end with the caller that started it. Otherwise, once the
// server had spent the refresh token, the next caller would send it again and
// be refused.
func TestOAuthHandler_RefreshOutlivesTheCallerThatStartedIt(t *testing.T) {
	server := newRefreshServer(t, singleUseTokens(200*time.Millisecond))
	handler, _ := newRefreshTestHandler(t, server)

	impatient, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := handler.GetAuthorizationHeader(impatient)
	require.ErrorIs(t, err, context.DeadlineExceeded)

	header, err := handler.GetAuthorizationHeader(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "Bearer new-access", header)
	assert.Equal(t, int32(1), server.refreshes.Load(), "refresh requests")
}

// Callers waiting on a refresh that fails all get the failure from it, rather
// than each sending a refresh of its own in turn. The next call tries again.
func TestOAuthHandler_CallersShareAFailedRefresh(t *testing.T) {
	server := newRefreshServer(t, func(w http.ResponseWriter, _ string) {
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusInternalServerError)
	})
	handler, _ := newRefreshTestHandler(t, server)

	const callers = 5
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			_, errs[i] = handler.GetAuthorizationHeader(t.Context())
		})
	}
	wg.Wait()
	for i := range callers {
		assert.ErrorIs(t, errs[i], ErrOAuthAuthorizationRequired, "caller %d", i)
	}
	assert.Equal(t, int32(1), server.refreshes.Load(), "refresh requests")

	_, err := handler.GetAuthorizationHeader(t.Context())
	assert.ErrorIs(t, err, ErrOAuthAuthorizationRequired)
	assert.Equal(t, int32(2), server.refreshes.Load(), "refresh requests after another call")
}

// A caller waiting for another's refresh gives up when its context ends.
func TestOAuthHandler_RefreshWaitHonorsContext(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	server := newRefreshServer(t, func(w http.ResponseWriter, _ string) {
		entered <- struct{}{}
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "token_type": "bearer"})
	})
	handler, _ := newRefreshTestHandler(t, server)

	// The first caller's refresh hangs until the test ends.
	go func() { _, _ = handler.GetAuthorizationHeader(t.Context()) }()
	<-entered

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := handler.GetAuthorizationHeader(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting caller ignored its context")
	}
}

type staleReadKey struct{}

// staleReadStore delays the first read made with a staleReadKey context:
// it reads the token, then waits for resume before returning it, by which
// time the stored token may have changed.
type staleReadStore struct {
	TokenStore
	once   sync.Once
	taken  chan struct{}
	resume chan struct{}
}

func (s *staleReadStore) GetToken(ctx context.Context) (*Token, error) {
	if ctx.Value(staleReadKey{}) != nil {
		var token *Token
		var err error
		delayed := false
		s.once.Do(func() {
			token, err = s.TokenStore.GetToken(ctx)
			close(s.taken)
			<-s.resume
			delayed = true
		})
		if delayed {
			return token, err
		}
	}
	return s.TokenStore.GetToken(ctx)
}

// A caller that read the expired token before another caller's refresh
// finished uses the new token instead of sending the spent refresh token.
func TestOAuthHandler_RefreshAfterAStaleRead(t *testing.T) {
	server := newRefreshServer(t, singleUseTokens(0))
	handler, store := newRefreshTestHandler(t, server)
	stale := &staleReadStore{TokenStore: store, taken: make(chan struct{}), resume: make(chan struct{})}
	handler.config.TokenStore = stale

	done := make(chan error, 1)
	var header string
	go func() {
		var err error
		header, err = handler.GetAuthorizationHeader(context.WithValue(t.Context(), staleReadKey{}, true))
		done <- err
	}()
	<-stale.taken

	// Another caller refreshes while the first still holds the old token.
	_, err := handler.GetAuthorizationHeader(t.Context())
	require.NoError(t, err)
	close(stale.resume)

	require.NoError(t, <-done)
	assert.Equal(t, "Bearer new-access", header)
	assert.Equal(t, int32(1), server.refreshes.Load(), "refresh requests")
}

// A refresh response without a scope grants the scope the token had (RFC
// 6749 §5.1, §6): the recorded grant is kept, so a later step-up still asks
// for the scopes already granted.
func TestOAuthHandler_ARefreshKeepsTheGrantedScope(t *testing.T) {
	server := newRefreshServer(t, singleUseTokens(0))
	handler, store := newRefreshTestHandler(t, server)
	stored, err := store.GetToken(t.Context())
	require.NoError(t, err)
	granted := *stored
	granted.Scope = "files:read"
	require.NoError(t, store.SaveToken(t.Context(), &granted))

	_, err = handler.GetAuthorizationHeader(t.Context())
	require.NoError(t, err)
	saved, err := store.GetToken(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "new-access", saved.AccessToken, "the token was refreshed")
	assert.Equal(t, "files:read", saved.Scope)
}

// A token is refreshed shortly before it expires, not after: its expiry is
// counted from when the response arrived, later than the server's clock
// started it, and a request sent just before the instant reaches the server
// after it. The margin is at most half the token's lifetime, a token without
// a refresh token is used to the end, and a token that has not expired is
// still used when the early refresh fails.
func TestOAuthHandler_RefreshesATokenAboutToExpire(t *testing.T) {
	refused := func(w http.ResponseWriter, _ string) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
	}
	tests := []struct {
		name         string
		expiresIn    int64
		left         time.Duration
		refreshToken string
		token        func(http.ResponseWriter, string)
		wantHeader   string
		wantRefresh  int32
	}{
		{"inside the margin", 3600, 5 * time.Second, "single-use", singleUseTokens(0), "Bearer new-access", 1},
		{"outside the margin", 3600, time.Hour, "single-use", singleUseTokens(0), "Bearer old-access", 0},
		{"a short-lived token keeps half its life", 10, 8 * time.Second, "single-use", singleUseTokens(0), "Bearer old-access", 0},
		{"no refresh token", 3600, 5 * time.Second, "", singleUseTokens(0), "Bearer old-access", 0},
		{"a failed early refresh", 3600, 5 * time.Second, "single-use", refused, "Bearer old-access", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newRefreshServer(t, tt.token)
			store := NewMemoryTokenStore()
			require.NoError(t, store.SaveToken(t.Context(), &Token{
				AccessToken:  "old-access",
				TokenType:    "Bearer",
				RefreshToken: tt.refreshToken,
				ExpiresIn:    tt.expiresIn,
				ExpiresAt:    time.Now().Add(tt.left),
				Issuer:       server.URL,
			}))
			handler := NewOAuthHandler(OAuthConfig{
				ClientID:              "client",
				TokenStore:            store,
				AuthServerMetadataURL: server.URL + "/.well-known/oauth-authorization-server",
			})
			handler.SetBaseURL(server.URL + "/mcp")

			header, err := handler.GetAuthorizationHeader(t.Context())
			require.NoError(t, err)
			assert.Equal(t, tt.wantHeader, header)
			assert.Equal(t, tt.wantRefresh, server.refreshes.Load(), "refresh requests")
		})
	}
}

// The token kept when an early refresh fails is still checked against the
// server the handler targets: a base URL change while the refresh ran leaves
// it another server's token.
func TestOAuthHandler_AFailedEarlyRefreshKeepsNoTokenAcrossARetarget(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	server := newRefreshServer(t, func(w http.ResponseWriter, _ string) {
		close(arrived)
		<-release
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
	})
	store := NewMemoryTokenStore()
	require.NoError(t, store.SaveToken(t.Context(), &Token{
		AccessToken:  "old-access",
		TokenType:    "Bearer",
		RefreshToken: "single-use",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(5 * time.Second),
		Resource:     server.URL + "/mcp",
		Issuer:       server.URL,
	}))
	handler := NewOAuthHandler(OAuthConfig{
		ClientID:              "client",
		TokenStore:            store,
		AuthServerMetadataURL: server.URL + "/.well-known/oauth-authorization-server",
	})
	handler.SetBaseURL(server.URL + "/mcp")

	errs := make(chan error, 1)
	go func() {
		_, err := handler.authorizationHeader(t.Context(), server.URL+"/mcp")
		errs <- err
	}()
	<-arrived
	handler.SetBaseURL("https://other.example/mcp")
	close(release)
	assert.ErrorIs(t, <-errs, ErrOAuthAuthorizationRequired)
}

// A refresh token the authorization server refused is not sent again: every
// later request would otherwise cost a token endpoint round trip until the
// user authorizes again. A different refresh token, as a new authorization
// stores, is tried as usual.
func TestOAuthHandler_ARefusedRefreshTokenIsNotSentAgain(t *testing.T) {
	server := newRefreshServer(t, func(w http.ResponseWriter, _ string) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
	})
	handler, store := newRefreshTestHandler(t, server)

	for range 3 {
		_, err := handler.GetAuthorizationHeader(t.Context())
		require.ErrorIs(t, err, ErrOAuthAuthorizationRequired)
	}
	assert.Equal(t, int32(1), server.refreshes.Load(), "refresh requests with the refused token")

	require.NoError(t, store.SaveToken(t.Context(), &Token{
		AccessToken:  "old-access",
		RefreshToken: "another",
		ExpiresAt:    time.Now().Add(-time.Minute),
		Issuer:       server.URL,
	}))
	_, err := handler.GetAuthorizationHeader(t.Context())
	require.ErrorIs(t, err, ErrOAuthAuthorizationRequired)
	assert.Equal(t, int32(2), server.refreshes.Load(), "a new refresh token is tried")
}

// A temporary failure refuses nothing, whether the status or the error code
// says so: the same refresh token is sent again once the server recovers.
func TestOAuthHandler_ATemporaryRefreshFailureIsRetried(t *testing.T) {
	tests := []struct {
		name   string
		status int
		code   string
	}{
		{"server error status", http.StatusInternalServerError, "temporarily_unavailable"},
		{"unavailable status", http.StatusServiceUnavailable, "server_error"},
		{"too many requests", http.StatusTooManyRequests, "invalid_request"},
		{"server_error code", http.StatusBadRequest, "server_error"},
		{"temporarily_unavailable in a 200", http.StatusOK, "temporarily_unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var failed atomic.Bool
			server := newRefreshServer(t, func(w http.ResponseWriter, _ string) {
				if failed.CompareAndSwap(false, true) {
					w.WriteHeader(tt.status)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": tt.code})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "recovered", "token_type": "Bearer"})
			})
			handler, _ := newRefreshTestHandler(t, server)

			_, err := handler.GetAuthorizationHeader(t.Context())
			require.Error(t, err)
			header, err := handler.GetAuthorizationHeader(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "Bearer recovered", header)
			assert.Equal(t, int32(2), server.refreshes.Load(), "refresh requests with the same token")
		})
	}
}

// A token request that fails names itself: a refresh as the refresh token
// request, a code exchange as the token request.
func TestOAuthHandler_TokenRequestErrorsNameTheRequest(t *testing.T) {
	answers := []struct {
		name   string
		answer func(http.ResponseWriter)
		prefix string
	}{
		{"no answer", func(w http.ResponseWriter) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}, "failed to send %s: "},
		{"an error status", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
		}, "%s failed: "},
		{"an error in a 200", func(w http.ResponseWriter) {
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
		}, "%s failed: "},
		{"a body that is not a token", func(w http.ResponseWriter) {
			_, _ = w.Write([]byte("[]"))
		}, "failed to decode token response: "},
	}
	requests := []struct {
		name string
		send func(*OAuthHandler) error
	}{
		{"refresh token request", func(h *OAuthHandler) error {
			_, err := h.RefreshToken(t.Context(), "single-use")
			return err
		}},
		{"token request", func(h *OAuthHandler) error {
			return h.ProcessAuthorizationCallback(t.Context(), callback("", "code", "state"), "verifier")
		}},
	}
	for _, request := range requests {
		for _, answer := range answers {
			t.Run(request.name+"/"+answer.name, func(t *testing.T) {
				server := newRefreshServer(t, func(w http.ResponseWriter, _ string) { answer.answer(w) })
				handler, _ := newRefreshTestHandler(t, server)
				handler.SetExpectedState("state")
				handler.SetExpectedIssuer(server.URL, false)

				err := request.send(handler)
				require.Error(t, err)
				prefix := strings.ReplaceAll(answer.prefix, "%s", request.name)
				assert.True(t, strings.HasPrefix(err.Error(), prefix), "%q does not start with %q", err, prefix)
			})
		}
	}
}
