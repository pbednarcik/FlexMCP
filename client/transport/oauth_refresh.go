package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// tokenRefresh is a token refresh shared by the callers that wait for it.
type tokenRefresh struct {
	done  chan struct{}
	token *Token
	err   error
}

// noteRefused records refreshToken as one the authorization server refused.
func (h *OAuthHandler) noteRefused(refreshToken string) {
	h.refreshMu.Lock()
	h.refused = refreshToken
	h.refreshMu.Unlock()
}

// wasRefused reports whether the authorization server refused refreshToken.
func (h *OAuthHandler) wasRefused(refreshToken string) bool {
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()
	return refreshToken != "" && refreshToken == h.refused
}

// refusesRefreshToken reports whether an OAuth error answered with status
// refuses the refresh token. A server failure, a rate limit, or a code that
// says the server can't answer now (RFC 6749 section 4.1.2.1) refuses
// nothing.
func refusesRefreshToken(status int, code string) bool {
	if status >= http.StatusInternalServerError || status == http.StatusTooManyRequests {
		return false
	}
	return code != "server_error" && code != "temporarily_unavailable"
}

// refreshTimeout bounds a token refresh, which doesn't end with the context
// of the caller that started it.
const refreshTimeout = 30 * time.Second

// startRefresh returns the token refresh in flight, starting one if there is
// none.
//
// The refresh runs on its own context rather than the caller's. If it ended
// with the caller, a caller that gives up after the server has used up the
// refresh token would leave the others to send the spent token again.
func (h *OAuthHandler) startRefresh(ctx context.Context) *tokenRefresh {
	h.refreshMu.Lock()
	defer h.refreshMu.Unlock()
	if h.refreshing != nil {
		return h.refreshing
	}
	refresh := &tokenRefresh{done: make(chan struct{})}
	h.refreshing = refresh
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refreshTimeout)
	go func() {
		defer cancel()
		refresh.token, refresh.err = h.refreshStoredToken(ctx)
		h.refreshMu.Lock()
		h.refreshing = nil
		h.refreshMu.Unlock()
		close(refresh.done)
	}()
	return refresh
}

// refreshStoredToken refreshes the stored token, unless a refresh that
// finished since the caller read the store has already replaced it.
func (h *OAuthHandler) refreshStoredToken(ctx context.Context) (*Token, error) {
	gen := h.tokenGen.Load()
	token, err := h.config.TokenStore.GetToken(ctx)
	if err != nil {
		return nil, err
	}
	if token.sendable() {
		return token, nil
	}
	if token.RefreshToken == "" {
		return nil, ErrOAuthAuthorizationRequired
	}
	return h.refreshToken(ctx, token, gen)
}

// refreshToken spends the refresh token of stored, whose record, read with
// it, says where the refresh token may go: only to the authorization server
// that issued it, for the server it was issued for, with the client it was
// issued to. A refresh token without a record carries none. gen is tokenGen
// as it was before stored was read, so an authorization or registration
// that lands after the read supersedes the refresh.
func (h *OAuthHandler) refreshToken(ctx context.Context, stored *Token, gen uint64) (*Token, error) {
	// A refresh token without a record goes only to the issuer ClientIssuer
	// names: the first issuer the client credentials were used with is not
	// evidence of where the refresh token came from. With neither, it can go
	// nowhere, whatever discovery would answer.
	issuer := stored.Issuer
	if issuer == "" {
		issuer = h.config.ClientIssuer
	}
	if issuer == "" {
		return nil, fmt.Errorf("%w: the refresh token has no record of its issuer, and ClientIssuer is not set", ErrOAuthAuthorizationRequired)
	}

	metadata, err := h.getServerMetadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get server metadata: %w", err)
	}

	// RFC 8707: every token request names the resource.
	resourceURL := h.getResourceURL()
	if resourceURL == "" {
		return nil, ErrResourceUnknown
	}
	if issuer != metadata.Issuer {
		return nil, fmt.Errorf("%w: the refresh token is from %q, the token endpoint belongs to %q", ErrIssuerMismatch, issuer, metadata.Issuer)
	}
	if !h.tokenIsForThisServer(stored) {
		return nil, fmt.Errorf("%w: the refresh token is for %q", ErrOAuthAuthorizationRequired, stored.Resource)
	}
	refreshToken := stored.RefreshToken
	data := url.Values{}
	data.Set("grant_type", "refresh_token")
	data.Set("refresh_token", refreshToken)
	data.Set("resource", resourceURL)

	req, clientID, err := h.newTokenRequest(ctx, metadata, data)
	if err != nil {
		return nil, fmt.Errorf("failed to create refresh token request: %w", err)
	}
	if stored.ClientID != "" && stored.ClientID != clientID {
		return nil, fmt.Errorf("%w: the refresh token was issued to client %q", ErrOAuthAuthorizationRequired, stored.ClientID)
	}

	tokenResp, status, err := h.doTokenRequest(req, "refresh token request")
	if err != nil {
		var oauthErr OAuthError
		if errors.As(err, &oauthErr) && refusesRefreshToken(status, oauthErr.ErrorCode) {
			h.noteRefused(refreshToken)
		}
		return nil, err
	}

	// If no new refresh token is provided, keep the old one
	if tokenResp.RefreshToken == "" {
		tokenResp.RefreshToken = refreshToken
	}
	// A response without a scope grants the scope the token had (RFC 6749
	// §5.1, §6), which a later step-up must keep asking for.
	if tokenResp.Scope == "" {
		tokenResp.Scope = stored.Scope
	}
	tokenResp.Resource = resourceURL
	tokenResp.Issuer = metadata.Issuer
	tokenResp.ClientID = clientID

	if err := h.saveIssuedToken(ctx, tokenResp, gen); err != nil {
		return nil, err
	}

	return tokenResp, nil
}

// RefreshToken refreshes a token with refreshToken. When it is the stored
// token's, the stored record decides where it may go; one brought from
// elsewhere has none, and goes only to OAuthConfig.ClientIssuer. A token
// store that cannot be read refuses the refresh.
func (h *OAuthHandler) RefreshToken(ctx context.Context, refreshToken string) (*Token, error) {
	token := &Token{RefreshToken: refreshToken}
	gen := h.tokenGen.Load()
	stored, err := h.config.TokenStore.GetToken(ctx)
	switch {
	case err == nil && stored.RefreshToken == refreshToken:
		token = stored
	case err != nil && !errors.Is(err, ErrNoToken):
		return nil, fmt.Errorf("failed to read the token store: %w", err)
	}
	return h.refreshToken(ctx, token, gen)
}
