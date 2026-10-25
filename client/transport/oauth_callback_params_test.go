package transport

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// RFC 6749 §3.1: an authorization response parameter is not repeated. A
// callback that repeats one is refused before its state is consumed, so the
// genuine callback still goes through, and its code is not redeemed.
func TestOAuthHandler_RejectsARepeatedCallbackParameter(t *testing.T) {
	for _, name := range []string{"code", "state", "iss", "error"} {
		t.Run(name, func(t *testing.T) {
			server := &tokenAuthServer{issSupported: true}
			handler, issuer := issHandler(t, server)

			params := callback(issuer, "code", "state")
			params.Add(name, params.Get(name))
			if name == "error" {
				params.Set("error", "access_denied")
				params.Add("error", "access_denied")
			}
			err := handler.ProcessAuthorizationCallback(t.Context(), params, "verifier")
			require.ErrorIs(t, err, ErrAuthorizationParameterRepeated)
			assert.EqualError(t, err, ErrAuthorizationParameterRepeated.Error()+": "+name)
			server.mu.Lock()
			assert.Empty(t, server.forms, "the code went to a token endpoint")
			server.mu.Unlock()

			require.NoError(t, handler.ProcessAuthorizationCallback(t.Context(), callback(issuer, "code", "state"), "verifier"))
		})
	}
}

// A callback that carries neither a code nor an error is refused, rather
// than an empty code being sent to the token endpoint.
func TestOAuthHandler_RejectsACallbackWithNoCode(t *testing.T) {
	for name, withEmpty := range map[string]bool{"absent": false, "empty": true} {
		t.Run(name, func(t *testing.T) {
			server := &tokenAuthServer{issSupported: true}
			handler, issuer := issHandler(t, server)

			params := callback(issuer, "", "state")
			if !withEmpty {
				params.Del("code")
			}
			err := handler.ProcessAuthorizationCallback(t.Context(), params, "verifier")
			require.ErrorIs(t, err, ErrAuthorizationCodeMissing)
			server.mu.Lock()
			defer server.mu.Unlock()
			assert.Empty(t, server.forms, "an empty code went to a token endpoint")
		})
	}
}
