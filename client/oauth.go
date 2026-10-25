package client

import (
	"errors"
	"fmt"
	"slices"

	"github.com/pbednarcik/flexmcp/client/transport"
)

// OAuthConfig is a convenience type that wraps transport.OAuthConfig
type OAuthConfig = transport.OAuthConfig

// Token is a convenience type that wraps transport.Token
type Token = transport.Token

// TokenStore is a convenience type that wraps transport.TokenStore
type TokenStore = transport.TokenStore

// MemoryTokenStore is a convenience type that wraps transport.MemoryTokenStore
type MemoryTokenStore = transport.MemoryTokenStore

// NewMemoryTokenStore is a convenience function that wraps transport.NewMemoryTokenStore
var NewMemoryTokenStore = transport.NewMemoryTokenStore

// NewOAuthStreamableHttpClient creates a new streamable-http-based MCP client with OAuth support.
// Returns an error if the URL is invalid.
func NewOAuthStreamableHttpClient(baseURL string, oauthConfig OAuthConfig, options ...transport.StreamableHTTPCOption) (*Client, error) {
	// The OAuth option goes on a copy: the caller's slice is left alone.
	options = append(slices.Clone(options), transport.WithHTTPOAuth(oauthConfig))

	trans, err := transport.NewStreamableHTTP(baseURL, options...)
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP transport: %w", err)
	}
	return NewClient(trans), nil
}

// NewOAuthSSEClient creates a new SSE-based MCP client with OAuth support.
// Returns an error if the URL is invalid. The HTTP+SSE transport is
// Deprecated; new code uses NewOAuthStreamableHttpClient.
func NewOAuthSSEClient(baseURL string, oauthConfig OAuthConfig, options ...transport.ClientOption) (*Client, error) {
	options = append(slices.Clone(options), transport.WithOAuth(oauthConfig))

	trans, err := transport.NewSSE(baseURL, options...)
	if err != nil {
		return nil, fmt.Errorf("failed to create SSE transport: %w", err)
	}
	return NewClient(trans), nil
}

// GenerateCodeVerifier generates a code verifier for PKCE
var GenerateCodeVerifier = transport.GenerateCodeVerifier

// GenerateCodeChallenge generates a code challenge from a code verifier
var GenerateCodeChallenge = transport.GenerateCodeChallenge

// GenerateState generates a state parameter for OAuth
var GenerateState = transport.GenerateState

// AuthorizationRequiredError is returned when a 401 Unauthorized response is received
type AuthorizationRequiredError = transport.AuthorizationRequiredError

// OAuthAuthorizationRequiredError is returned when OAuth authorization is required
type OAuthAuthorizationRequiredError = transport.OAuthAuthorizationRequiredError

// IsAuthorizationRequiredError checks if an error is an AuthorizationRequiredError
func IsAuthorizationRequiredError(err error) bool {
	var target *AuthorizationRequiredError
	return errors.As(err, &target)
}

// IsOAuthAuthorizationRequiredError checks if an error is an OAuthAuthorizationRequiredError
func IsOAuthAuthorizationRequiredError(err error) bool {
	var target *OAuthAuthorizationRequiredError
	return errors.As(err, &target)
}

// GetOAuthHandler extracts the OAuthHandler from an OAuthAuthorizationRequiredError
func GetOAuthHandler(err error) *transport.OAuthHandler {
	if oauthErr, ok := errors.AsType[*OAuthAuthorizationRequiredError](err); ok {
		return oauthErr.Handler
	}
	return nil
}

// GetResourceMetadataURL extracts the protected resource metadata URL from an authorization error.
// This URL is extracted from the WWW-Authenticate header per RFC9728 Section 5.1.
// Works with both AuthorizationRequiredError and OAuthAuthorizationRequiredError.
// Returns empty string if no metadata URL was discovered.
func GetResourceMetadataURL(err error) string {
	// Try OAuthAuthorizationRequiredError first (contains AuthorizationRequiredError)
	if oauthErr, ok := errors.AsType[*OAuthAuthorizationRequiredError](err); ok {
		return oauthErr.ResourceMetadataURL
	}

	// Try base AuthorizationRequiredError
	if authErr, ok := errors.AsType[*AuthorizationRequiredError](err); ok {
		return authErr.ResourceMetadataURL
	}

	return ""
}
