package transport

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// issHandler returns a handler that has sent the user to server's
// authorization endpoint, so the expected issuer is recorded.
func issHandler(t *testing.T, server *tokenAuthServer) (*OAuthHandler, string) {
	t.Helper()
	server.codeChallengeMethods = []string{"S256"}
	handler := pkceHandler(t, server)
	_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.NoError(t, err)
	return handler, handler.GetExpectedIssuer()
}

// rearm records state and issuer again: the record is for one response.
func rearm(handler *OAuthHandler, issuer string) {
	handler.SetExpectedState("state")
	handler.SetExpectedIssuer(issuer, true)
}

func callback(iss, code, state string) url.Values {
	params := url.Values{"code": {code}, "state": {state}}
	if iss != "" {
		params.Set("iss", iss)
	}
	return params
}

// RFC 9207: an iss that is not the issuer the client sent the user to is a
// mix-up, and the code is not redeemed anywhere. Whether the server
// advertised iss makes no difference to a present one (the spec's third
// row), and an error in the same response is not acted on.
func TestOAuthHandler_RejectsAMismatchedIss(t *testing.T) {
	for name, advertised := range map[string]bool{"advertised": true, "not advertised": false} {
		t.Run(name, func(t *testing.T) {
			server := &tokenAuthServer{issSupported: advertised}
			handler, _ := issHandler(t, server)

			params := callback("https://evil.example", "code", "state")
			params.Set("error", "access_denied")
			err := handler.ProcessAuthorizationCallback(t.Context(), params, "verifier")
			require.ErrorIs(t, err, ErrIssuerMismatch)
			var oauthErr OAuthError
			assert.False(t, errors.As(err, &oauthErr), "the error fields are not acted on")
			server.mu.Lock()
			defer server.mu.Unlock()
			assert.Empty(t, server.forms, "the code went to a token endpoint")
		})
	}
}

// A server that advertises iss sends it on every authorization response, so
// a response without it is rejected; the old entry point, which has no iss,
// is such a response.
func TestOAuthHandler_RejectsAMissingIssWhenAdvertised(t *testing.T) {
	server := &tokenAuthServer{issSupported: true}
	handler, issuer := issHandler(t, server)

	err := handler.ProcessAuthorizationCallback(t.Context(), callback("", "code", "state"), "verifier")
	require.ErrorIs(t, err, ErrIssuerMissing)

	rearm(handler, issuer)
	err = handler.ProcessAuthorizationResponse(t.Context(), "code", "state", "verifier")
	require.ErrorIs(t, err, ErrIssuerMissing)
	server.mu.Lock()
	defer server.mu.Unlock()
	assert.Empty(t, server.forms)
}

// A matching iss, or none from a server that never promised one, lets the
// exchange go ahead.
func TestOAuthHandler_AcceptsTheExpectedIss(t *testing.T) {
	tests := []struct {
		name       string
		advertised bool
		sendIss    bool
	}{
		{name: "advertised and present", advertised: true, sendIss: true},
		{name: "not advertised and present", sendIss: true},
		{name: "not advertised and absent"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &tokenAuthServer{issSupported: tt.advertised}
			handler, issuer := issHandler(t, server)
			iss := ""
			if tt.sendIss {
				iss = issuer
			}

			require.NoError(t, handler.ProcessAuthorizationCallback(t.Context(), callback(iss, "code", "state"), "verifier"))
			_, _, _, form := server.lastTokenRequest(t)
			assert.Equal(t, "code", form.Get("code"))
		})
	}
}

// The comparison is a plain string comparison: a trailing slash, a port
// spelled out, a capital letter are another issuer.
func TestOAuthHandler_ComparesIssWithoutNormalising(t *testing.T) {
	server := &tokenAuthServer{issSupported: true}
	handler, issuer := issHandler(t, server)

	err := handler.ProcessAuthorizationCallback(t.Context(), callback(issuer+"/", "code", "state"), "verifier")
	require.ErrorIs(t, err, ErrIssuerMismatch)
}

// An error response that passes the iss check is the server's answer.
func TestOAuthHandler_ReportsTheServersErrorResponse(t *testing.T) {
	server := &tokenAuthServer{issSupported: true}
	handler, issuer := issHandler(t, server)

	params := callback(issuer, "", "state")
	params.Set("error", "access_denied")
	params.Set("error_description", "the user said no")
	err := handler.ProcessAuthorizationCallback(t.Context(), params, "verifier")
	var oauthErr OAuthError
	require.ErrorAs(t, err, &oauthErr)
	assert.Equal(t, "access_denied", oauthErr.ErrorCode)
	assert.Equal(t, "the user said no", oauthErr.ErrorDescription)
}

// A callback handled by another handler instance gets the expected issuer
// the way it gets the expected state.
func TestOAuthHandler_SetExpectedIssuerCarriesItAcrossHandlers(t *testing.T) {
	server := &tokenAuthServer{issSupported: true}
	first, issuer := issHandler(t, server)
	require.NotEmpty(t, issuer)

	// Another handler instance for the same authorization server.
	second := handlerOn(issuer)
	rearm(second, first.GetExpectedIssuer())

	err := second.ProcessAuthorizationCallback(t.Context(), callback("https://evil.example", "code", "state"), "verifier")
	require.ErrorIs(t, err, ErrIssuerMismatch)
	rearm(second, first.GetExpectedIssuer())
	require.NoError(t, second.ProcessAuthorizationCallback(t.Context(), callback(issuer, "code", "state"), "verifier"))
}

// The issuer the user was sent to is also the one the code is redeemed with.
// If discovery leads elsewhere in between, a callback that names the right
// issuer is still not redeemed at the wrong token endpoint.
func TestOAuthHandler_DoesNotRedeemTheCodeWithAnotherIssuer(t *testing.T) {
	first := &tokenAuthServer{codeChallengeMethods: []string{"S256"}}
	firstHTTP := first.start(t)
	second := &tokenAuthServer{codeChallengeMethods: []string{"S256"}}
	secondHTTP := second.start(t)

	var mu sync.Mutex
	authorizationServer := firstHTTP.URL
	var resource *httptest.Server
	resource = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-protected-resource" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"resource": resource.URL, "authorization_servers": []string{authorizationServer}})
	}))
	t.Cleanup(resource.Close)

	handler := NewOAuthHandler(OAuthConfig{ClientID: "preconfigured", RedirectURI: "http://localhost:8085/callback", TokenStore: NewMemoryTokenStore()})
	handler.SetBaseURL(resource.URL)
	_, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.NoError(t, err)
	require.Equal(t, firstHTTP.URL, handler.GetExpectedIssuer())

	mu.Lock()
	authorizationServer = secondHTTP.URL
	mu.Unlock()
	handler.SetProtectedResourceMetadataURL(resource.URL + "/.well-known/oauth-protected-resource")

	err = handler.ProcessAuthorizationCallback(t.Context(), callback(firstHTTP.URL, "code", "state"), "verifier")
	require.ErrorIs(t, err, ErrIssuerMismatch)
	second.mu.Lock()
	defer second.mu.Unlock()
	assert.Empty(t, second.forms, "the code and verifier went to another issuer's token endpoint")
}
