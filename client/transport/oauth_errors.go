package transport

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrNoToken is returned when no token is available in the token store
var ErrNoToken = errors.New("no token available")

var (
	// ErrPKCERequired is returned when an authorization flow is started
	// without a code challenge, or finished without the code verifier.
	ErrPKCERequired = errors.New("PKCE is required: generate a code verifier and its S256 challenge")
	// ErrPKCENotSupported is returned when the authorization server's metadata
	// does not list S256 in code_challenge_methods_supported, so PKCE cannot
	// be verified and the flow does not start.
	ErrPKCENotSupported = errors.New("PKCE is not supported by the authorization server")
)

var (
	// ErrIssuerMismatch is returned when an authorization response names an
	// issuer other than the one the client sent the user to (RFC 9207): a
	// mix-up, so the code is not redeemed and the response's error fields are
	// not acted on.
	ErrIssuerMismatch = errors.New("authorization response iss does not match the expected issuer")
	// ErrIssuerMissing is returned when an authorization server that
	// advertises authorization_response_iss_parameter_supported sends an
	// authorization response without iss.
	ErrIssuerMissing = errors.New("authorization response has no iss, which the authorization server advertises")
	// ErrClientRegisteredElsewhere is returned when the client ID came from
	// dynamic registration with one authorization server and discovery now
	// leads to another: credentials belong to the issuer that gave them, so
	// the client has to register again.
	ErrClientRegisteredElsewhere = errors.New("the client is registered with another authorization server; register again")
	// ErrResourceUnknown is returned when discovery, an authorization flow or
	// a token request would start without knowing the MCP server it is for:
	// no base URL (set by the transports, or SetBaseURL) names it, so there
	// is nothing to discover from and no resource parameter to send.
	ErrResourceUnknown = errors.New("the MCP server to authorize for is not known: set its URL before starting authorization")
	// ErrInvalidResource is returned when the MCP server's URL, or the
	// resource its protected resource metadata declares, is not a canonical
	// resource identifier: an absolute URI with a scheme and a host and no
	// fragment (RFC 8707 §2), which the resource parameter must be.
	ErrInvalidResource = errors.New("the MCP server's URL is not a valid resource identifier")
	// ErrResourceChanged is returned when an authorization response arrives
	// for a flow started for another resource than the one the handler now
	// names: the code is redeemed only for the resource it was obtained for
	// (RFC 8707 §2.1).
	ErrResourceChanged = errors.New("the authorization was started for another resource")
	// ErrInsecureAuthorizationServer is returned when the authorization
	// server's metadata would be fetched over plain http, or names an issuer
	// or endpoint that is not https: authorization server endpoints are
	// served over HTTPS only (MCP authorization, Communication Security).
	ErrInsecureAuthorizationServer = errors.New("authorization server endpoints must use https")

	// ErrRedirectLeavesOrigin is returned when a request carrying an access
	// token is redirected off its origin, its scheme, host and port: the
	// token goes only to the server it was issued for (MCP authorization,
	// Token Audience Binding), so the redirect is not followed.
	ErrRedirectLeavesOrigin = errors.New("redirect would send the access token to another origin")

	// ErrInvalidTokenResponse is returned when a token response carries no
	// access token, or one of a type other than Bearer (RFC 6749 §5.1, §7.1;
	// MCP authorization sends Bearer tokens only). Nothing from it is saved.
	ErrInvalidTokenResponse = errors.New("invalid token response")

	// ErrInvalidRegistrationResponse is returned when a dynamic client
	// registration response carries no client_id (RFC 7591 §3.2.1). The
	// credentials stay as they were.
	ErrInvalidRegistrationResponse = errors.New("invalid client registration response")
)

// extractOAuthError attempts to parse an OAuth error response from the response body
func extractOAuthError(body []byte, statusCode int, message string) error {
	// Try to parse the error as an OAuth error response
	var oauthErr OAuthError
	if err := json.Unmarshal(body, &oauthErr); err == nil && oauthErr.ErrorCode != "" {
		return fmt.Errorf("%s: %w", message, oauthErr)
	}

	// If not a valid OAuth error, return the raw response
	return fmt.Errorf("%s with status %d: %s", message, statusCode, body)
}

// OAuthError represents a standard OAuth 2.0 error response
type OAuthError struct {
	ErrorCode        string `json:"error"`
	ErrorDescription string `json:"error_description,omitempty"`
	ErrorURI         string `json:"error_uri,omitempty"`
}

// Error implements the error interface
func (e OAuthError) Error() string {
	if e.ErrorDescription != "" {
		return fmt.Sprintf("OAuth error: %s - %s", e.ErrorCode, e.ErrorDescription)
	}
	return fmt.Sprintf("OAuth error: %s", e.ErrorCode)
}

// errMetadataUnavailable marks a metadata fetch that got no answer: the
// request failed or was cancelled, or the server failed. The metadata may
// well be there, so it is not taken for absent.
var errMetadataUnavailable = errors.New("metadata unavailable")

// ErrInvalidState is returned when the state parameter doesn't match the expected value
var ErrInvalidState = errors.New("invalid state parameter, possible CSRF attack")
