package transport

import (
	"context"
	"errors"
	"strings"
)

// tokenIsForThisServer reports whether token may be sent to the MCP server
// the handler targets (MCP authorization: clients send only tokens issued
// for the server). A token records the resource it was issued for; it is
// for this server when that equals the resource discovery found, or, before
// discovery, when it binds to the base URL as a protected resource's own
// identifier would (resourceBindsToURL). A token saved without the record,
// as one an application stored itself, is accepted as before.
func (h *OAuthHandler) tokenIsForThisServer(token *Token) bool {
	return h.tokenIsFor(token, "")
}

// tokenIsFor reports whether token may be sent to recipient, the base URL a
// transport sends its requests to: only while the handler targets that URL,
// read in the same snapshot as the target the token is checked against, and
// then as tokenIsForThisServer says. An empty recipient is the handler's own
// target.
func (h *OAuthHandler) tokenIsFor(token *Token, recipient string) bool {
	h.metadataMu.Lock()
	resource, base := h.resourceURL, h.baseURL
	issuer := h.knownIssuer
	h.metadataMu.Unlock()
	if recipient != "" && recipient != base {
		return false
	}
	// A token from another authorization server than the one discovery
	// last named belongs to a server the handler has moved away from.
	if token.Issuer != "" && issuer != "" && token.Issuer != issuer {
		return false
	}
	if token.Resource == "" {
		return true
	}
	if resource != "" {
		return resourceIdentifiersEqual(token.Resource, resource)
	}
	return base != "" && resourceBindsToURL(token.Resource, base)
}

// GetAuthorizationHeader returns the Authorization header value for a request.
//
// If the stored token has expired, it refreshes it, or waits for a refresh
// another caller already started, and returns ctx's error if ctx ends first.
func (h *OAuthHandler) GetAuthorizationHeader(ctx context.Context) (string, error) {
	return h.authorizationHeader(ctx, "")
}

// authorizationHeader is GetAuthorizationHeader for a request to recipient,
// the base URL the transport sends it to (see tokenIsFor).
func (h *OAuthHandler) authorizationHeader(ctx context.Context, recipient string) (string, error) {
	token, err := h.getValidToken(ctx, recipient)
	if err != nil {
		return "", err
	}

	// Per RFC 6749 §5.1, token_type is case-insensitive.
	// Normalize to "Bearer" for strict implementations. A token without a
	// type, for example one an application put in the store itself, is
	// sent as a bearer token too, as MCP authorization prescribes.
	tokenType := token.TokenType
	if tokenType == "" || strings.EqualFold(tokenType, "bearer") {
		tokenType = "Bearer"
	}

	return tokenType + " " + token.AccessToken, nil
}

// getValidToken returns a valid token for recipient, refreshing if necessary
func (h *OAuthHandler) getValidToken(ctx context.Context, recipient string) (*Token, error) {
	token, err := h.config.TokenStore.GetToken(ctx)
	if err != nil && !errors.Is(err, ErrNoToken) {
		return nil, err
	}
	// A token for another MCP server is not this server's to receive, and
	// its refresh token not this flow's to spend.
	if err == nil && !h.tokenIsFor(token, recipient) {
		return nil, ErrOAuthAuthorizationRequired
	}
	if err == nil && token.sendable() {
		return token, nil
	}
	if err != nil || token.RefreshToken == "" {
		// We need to get a new token through the authorization flow
		return nil, ErrOAuthAuthorizationRequired
	}
	// Called from inside this handler's own SaveToken, a refresh would wait
	// for the one that is saving.
	if ctx.Value(savingKey{h}) != nil {
		return nil, errSaveReentered
	}
	if h.wasRefused(token.RefreshToken) {
		if !token.IsExpired() && token.AccessToken != "" {
			return token, nil
		}
		return nil, ErrOAuthAuthorizationRequired
	}

	// Share one refresh among the callers that need it. With refresh token
	// rotation, which the MCP spec requires for public clients, a second
	// request with the same refresh token is rejected, and a server that
	// detects the reuse may revoke the tokens the first request got.
	refresh := h.startRefresh(ctx)
	select {
	case <-refresh.done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// A refresh failed, or handed back a token that is not the recipient's:
	// one found in the store, which another refresh may have left there, or
	// one refreshed for the server the handler targeted before a change.
	if refresh.err != nil || !h.tokenIsFor(refresh.token, recipient) {
		// A refresh made inside the expiry margin leaves a token that has
		// not expired yet, still this recipient's to use.
		if token.AccessToken != "" && !token.IsExpired() && h.tokenIsFor(token, recipient) {
			return token, nil
		}
		return nil, ErrOAuthAuthorizationRequired
	}
	return refresh.token, nil
}
