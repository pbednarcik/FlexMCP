package client

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mark3labs/mcp-go/client/transport"
)

// The OAuth flavour of a 401 is still an authorization-required error: the
// base predicate, the base sentinel and the base type all match it, so a
// caller reads its scopes and error code through either path.
func TestTheOAuthErrorIsAnAuthorizationRequiredError(t *testing.T) {
	err := error(&transport.OAuthAuthorizationRequiredError{
		Handler:             transport.NewOAuthHandler(transport.OAuthConfig{}),
		ResourceMetadataURL: "https://example.com/.well-known/oauth-protected-resource",
		Scopes:              []string{"read"},
		ErrorCode:           "insufficient_scope",
	})

	assert.True(t, IsAuthorizationRequiredError(err))
	assert.True(t, IsOAuthAuthorizationRequiredError(err))
	assert.True(t, errors.Is(err, transport.ErrAuthorizationRequired))
	assert.True(t, errors.Is(err, transport.ErrOAuthAuthorizationRequired))
	var base *transport.AuthorizationRequiredError
	if assert.True(t, errors.As(err, &base)) {
		assert.Equal(t, []string{"read"}, base.Scopes)
		assert.Equal(t, "insufficient_scope", base.ErrorCode)
	}
}
