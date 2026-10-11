package transport

import (
	"errors"
	"net/url"
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

	second := pkceHandler(t, server)
	rearm(second, first.GetExpectedIssuer())

	err := second.ProcessAuthorizationCallback(t.Context(), callback("https://evil.example", "code", "state"), "verifier")
	require.ErrorIs(t, err, ErrIssuerMismatch)
	rearm(second, first.GetExpectedIssuer())
	require.NoError(t, second.ProcessAuthorizationCallback(t.Context(), callback(issuer, "code", "state"), "verifier"))
}
