package transport

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tokenResponseExpiresAt is a server's own expires_at in Unix seconds, as
// some authorization servers add it beside, or instead of, expires_in.
const tokenResponseExpiresAt = 1794307530

// requestTokenAnswered sends a token request to an endpoint that answers
// with body, and returns what doTokenRequest made of it.
func requestTokenAnswered(t *testing.T, body string) (*Token, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	handler := NewOAuthHandler(OAuthConfig{})
	form := url.Values{"grant_type": {"refresh_token"}}.Encode()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/token", strings.NewReader(form))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	token, _, err := handler.doTokenRequest(req, "refresh token request")
	return token, err
}

// A token response's expires_in decides the expiry (RFC 6749 §5.1). Some
// servers add an expires_at of their own, in Unix seconds or as a time; it
// stands in when expires_in is absent, and one that cannot be read is
// ignored rather than failing the exchange. The token is stored as before.
func TestOAuthHandler_TokenResponseExpiry(t *testing.T) {
	const fields = `"access_token":"new-access","token_type":"Bearer"`
	numeric := time.Unix(tokenResponseExpiresAt, 0)

	t.Run("expires_in alone", func(t *testing.T) {
		token, err := requestTokenAnswered(t, `{`+fields+`,"expires_in":3600}`)
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now().Add(time.Hour), token.ExpiresAt, 5*time.Second)
	})

	t.Run("numeric expires_at alone", func(t *testing.T) {
		token, err := requestTokenAnswered(t, `{`+fields+`,"expires_at":1794307530}`)
		require.NoError(t, err)
		assert.Truef(t, token.ExpiresAt.Equal(numeric), "ExpiresAt = %v, want %v", token.ExpiresAt, numeric)
	})

	t.Run("RFC 3339 expires_at alone", func(t *testing.T) {
		token, err := requestTokenAnswered(t, `{`+fields+`,"expires_at":"2030-01-02T03:04:05+01:00"}`)
		require.NoError(t, err)
		want := time.Date(2030, time.January, 2, 2, 4, 5, 0, time.UTC)
		assert.Truef(t, token.ExpiresAt.Equal(want), "ExpiresAt = %v, want %v", token.ExpiresAt, want)
	})

	t.Run("expires_in wins over expires_at", func(t *testing.T) {
		token, err := requestTokenAnswered(t, `{`+fields+`,"expires_in":3600,"expires_at":1794307530}`)
		require.NoError(t, err)
		assert.WithinDuration(t, time.Now().Add(time.Hour), token.ExpiresAt, 5*time.Second)
	})

	for _, raw := range []string{`0`, `-5`, `1.5`, `"soon"`, `true`, `null`, `"1794307530"`} {
		t.Run("unreadable expires_at "+raw, func(t *testing.T) {
			token, err := requestTokenAnswered(t, `{`+fields+`,"expires_at":`+raw+`}`)
			require.NoError(t, err)
			assert.Equal(t, "new-access", token.AccessToken)
			assert.Truef(t, token.ExpiresAt.IsZero(), "ExpiresAt = %v, want the zero time", token.ExpiresAt)
		})
	}

	// A stored token's expires_at is an RFC 3339 time, whose year has four
	// digits, so the last second of year 9999 is the latest expires_at read.
	t.Run("expires_at at the last second of year 9999", func(t *testing.T) {
		token, err := requestTokenAnswered(t, `{`+fields+`,"expires_at":253402300799}`)
		require.NoError(t, err)
		want := time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC)
		assert.Truef(t, token.ExpiresAt.Equal(want), "ExpiresAt = %v, want %v", token.ExpiresAt, want)
		_, err = json.Marshal(token)
		assert.NoError(t, err, "the token is stored")
	})

	// Past year 9999, or with a zone offset of 24 hours, which RFC 3339
	// parsing admits but a stored time cannot carry, expires_at is ignored.
	for _, raw := range []string{`253402300800`, `9223372036854775807`, `"2030-01-02T03:04:05+24:00"`} {
		t.Run("expires_at that cannot be stored "+raw, func(t *testing.T) {
			token, err := requestTokenAnswered(t, `{`+fields+`,"expires_at":`+raw+`}`)
			require.NoError(t, err)
			assert.Truef(t, token.ExpiresAt.IsZero(), "ExpiresAt = %v, want the zero time", token.ExpiresAt)
			_, err = json.Marshal(token)
			assert.NoError(t, err, "the token is stored")
		})
	}

	t.Run("the token is stored with expires_at as a time", func(t *testing.T) {
		token, err := requestTokenAnswered(t, `{`+fields+`,"expires_at":1794307530}`)
		require.NoError(t, err)
		data, err := json.Marshal(token)
		require.NoError(t, err)
		var stored map[string]any
		require.NoError(t, json.Unmarshal(data, &stored))
		text, ok := stored["expires_at"].(string)
		require.Truef(t, ok, "expires_at stored as %T, want a string", stored["expires_at"])
		at, err := time.Parse(time.RFC3339, text)
		require.NoError(t, err)
		assert.Truef(t, at.Equal(numeric), "stored expires_at = %v, want %v", at, numeric)
	})

	t.Run("a refresh saves a numeric expires_at", func(t *testing.T) {
		server := newRefreshServer(t, func(w http.ResponseWriter, _ string) {
			_, _ = io.WriteString(w, `{`+fields+`,"refresh_token":"next","expires_at":1794307530}`)
		})
		handler, store := newRefreshTestHandler(t, server)

		refreshed, err := handler.RefreshToken(t.Context(), "single-use")
		require.NoError(t, err)
		assert.Truef(t, refreshed.ExpiresAt.Equal(numeric), "ExpiresAt = %v, want %v", refreshed.ExpiresAt, numeric)
		saved, err := store.GetToken(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "new-access", saved.AccessToken)
		assert.Truef(t, saved.ExpiresAt.Equal(numeric), "saved ExpiresAt = %v, want %v", saved.ExpiresAt, numeric)
	})
}
