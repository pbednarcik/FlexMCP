package transport

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// TokenStore is an interface for storing and retrieving OAuth tokens.
//
// Implementations must:
//   - Honor context cancellation and deadlines, returning context.Canceled
//     or context.DeadlineExceeded as appropriate
//   - Return ErrNoToken (or a sentinel error that wraps it) when no token
//     is available, rather than conflating this with other operational errors
//   - Properly propagate all other errors (database failures, I/O errors, etc.)
//   - Check ctx.Done() before performing operations and return ctx.Err() if cancelled
type TokenStore interface {
	// GetToken returns the current token.
	// Returns ErrNoToken if no token is available.
	// Returns context.Canceled or context.DeadlineExceeded if ctx is cancelled.
	// Returns other errors for operational failures (I/O, database, etc.).
	GetToken(ctx context.Context) (*Token, error)

	// SaveToken saves a token.
	// It must not call back into the OAuthHandler that is saving: a refresh
	// or code exchange started from it with ctx fails rather than waits.
	// Returns context.Canceled or context.DeadlineExceeded if ctx is cancelled.
	// Returns other errors for operational failures (I/O, database, etc.).
	SaveToken(ctx context.Context, token *Token) error
}

// Token represents an OAuth token
type Token struct {
	// AccessToken is the OAuth access token
	AccessToken string `json:"access_token"`
	// TokenType is the type of token (usually "Bearer")
	TokenType string `json:"token_type"`
	// RefreshToken is the OAuth refresh token
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpiresIn is the number of seconds until the token expires
	ExpiresIn int64 `json:"expires_in,omitempty"`
	// Scope is the scope of the token
	Scope string `json:"scope,omitempty"`
	// ExpiresAt is the time when the token expires
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	// Resource is the MCP server the token was issued for: the RFC 8707
	// resource of the token request. The handler sends the token only to
	// that server. Empty for a token saved without the record, which is sent
	// as before.
	Resource string `json:"resource,omitempty"`
	// Issuer is the authorization server that issued the token, the only one
	// its refresh token is sent to. Empty for a token saved without the
	// record, whose refresh token goes only to OAuthConfig.ClientIssuer and,
	// without it, nowhere.
	Issuer string `json:"issuer,omitempty"`
	// ClientID is the client the token was issued to, the only one its
	// refresh token is sent with (RFC 6749 §6). Empty for a token saved
	// without the record, whose refresh token is sent with the current client.
	ClientID string `json:"client_id,omitempty"`
}

// IsExpired returns true if the token is expired
func (t *Token) IsExpired() bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().After(t.ExpiresAt)
}

// maxExpiryMargin is how long before a token's expiry the handler refreshes
// it, as golang.org/x/oauth2 does: ExpiresAt is counted from when the token
// response arrived, later than the authorization server's own clock started
// it, and a request sent just before the instant reaches the server after it.
const maxExpiryMargin = 10 * time.Second

// sendable reports whether t can be sent as it is: it has an access token
// that has not expired and, when there is a refresh token to spend, will not
// within the expiry margin. The margin is at most half the token's lifetime,
// so a short-lived token is not refreshed on every request.
func (t *Token) sendable() bool {
	if t.AccessToken == "" || t.IsExpired() {
		return false
	}
	if t.RefreshToken == "" || t.ExpiresAt.IsZero() {
		return true
	}
	margin := maxExpiryMargin
	if t.ExpiresIn > 0 {
		margin = min(margin, time.Duration(t.ExpiresIn)*time.Second/2)
	}
	return time.Until(t.ExpiresAt) > margin
}

// MemoryTokenStore is a simple in-memory token store
type MemoryTokenStore struct {
	token *Token
	mu    sync.RWMutex
}

// NewMemoryTokenStore creates a new in-memory token store
func NewMemoryTokenStore() *MemoryTokenStore {
	return &MemoryTokenStore{}
}

// GetToken returns the current token.
// Returns ErrNoToken if no token is available.
// Returns context.Canceled or context.DeadlineExceeded if ctx is cancelled.
func (s *MemoryTokenStore) GetToken(ctx context.Context) (*Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.token == nil {
		return nil, ErrNoToken
	}
	return s.token, nil
}

// SaveToken saves a token.
// Returns context.Canceled or context.DeadlineExceeded if ctx is cancelled.
func (s *MemoryTokenStore) SaveToken(ctx context.Context, token *Token) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.token = token
	return nil
}

// savingKey marks the context a handler passes to TokenStore.SaveToken.
type savingKey struct{ h *OAuthHandler }

// errSaveReentered is returned to a token request made from inside the
// handler's own TokenStore.SaveToken, which would otherwise wait for itself.
var errSaveReentered = errors.New("TokenStore.SaveToken called back into the OAuth handler that is saving")

// saveIssuedToken saves token, issued for token.Resource by token.Issuer by
// a request that began at generation gen, unless something has superseded
// that request: a newer authorization or new client credentials (tokenGen
// moved), or a move of the handler to another resource or authorization
// server. The check and the save are one step under saveMu. A move that
// lands during the save is caught where tokens are used: tokenIsFor refuses
// a token from an issuer discovery no longer names.
func (h *OAuthHandler) saveIssuedToken(ctx context.Context, token *Token, gen uint64) error {
	if ctx.Value(savingKey{h}) != nil {
		return errSaveReentered
	}
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	h.metadataMu.Lock()
	current := h.serverMetadata != nil && h.serverMetadata.Issuer == token.Issuer && h.resourceURL == token.Resource
	h.metadataMu.Unlock()
	if !current || h.tokenGen.Load() != gen {
		return fmt.Errorf("%w: a newer authorization, new client credentials or a move to another server superseded the token request", ErrOAuthAuthorizationRequired)
	}
	if err := h.config.TokenStore.SaveToken(context.WithValue(ctx, savingKey{h}, true), token); err != nil {
		return fmt.Errorf("failed to save token: %w", err)
	}
	return nil
}
