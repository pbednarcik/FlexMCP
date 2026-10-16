package transport

import (
	"context"
	"net/http"
	"slices"
	"strings"
)

// setAdvertisedPRMURL records the metadata URL the MCP server at recipient
// advertised, already validated against the base URL, and discovers again
// from it, unless the handler no longer targets recipient (empty: its own
// target).
func (h *OAuthHandler) setAdvertisedPRMURL(u, recipient string) {
	h.metadataMu.Lock()
	defer h.metadataMu.Unlock()
	// Checked again against the base URL it is recorded for, in the same
	// critical section: the base URL may have changed since the challenge
	// was read.
	if (recipient != "" && recipient != h.baseURL) || checkAdvertisedPRMURL(u, h.baseURL) != nil {
		return
	}
	h.advertisedPRMURL = u
	h.forgetDiscoveryLocked()
}

// HandleUnauthorizedResponse takes what a 401, or a 403 with an
// insufficient_scope challenge, tells the client for its next
// authorization: one challenge, whole, the one the transports act on too.
//
// The challenge acted on is the first usable Bearer one (every
// WWW-Authenticate header line is read): on a 403 one saying
// insufficient_scope, and one whose resource_metadata parameter, when it
// has one, shares the base URL's scheme and host, so a compromised or
// misconfigured resource cannot send discovery to an attacker's metadata
// endpoint. A challenge that fails that is passed over whole, and the next
// one tried. From the chosen challenge the scope parameter (RFC 6750 §3) is
// recorded as the scopes to ask for first (see ChallengedScopes) and the
// resource_metadata URL (RFC 9728 §5.1) as where discovery starts.
//
// It is safe to call with a nil response.
func (h *OAuthHandler) HandleUnauthorizedResponse(resp *http.Response) {
	if resp == nil {
		return
	}
	if c, ok := actedOnChallenge(resp, h); ok {
		h.recordChallenge(c, "")
	}
}

// recordChallenge records what the challenge acted on says for the next
// authorization: its scopes, which replace the last challenge's even when
// there are none (they are the refused operation's, not a standing grant),
// and its metadata URL, already validated.
//
// The challenge is the server's that sent it, recipient (empty: the
// handler's own target): it is dropped when the handler no longer targets
// recipient, or was retargeted since this call began, so a late challenge
// from the previous server is not recorded for the next.
func (h *OAuthHandler) recordChallenge(c bearerChallenge, recipient string) {
	h.mu.RLock()
	target := h.target
	h.mu.RUnlock()
	h.metadataMu.Lock()
	base := h.baseURL
	h.metadataMu.Unlock()
	if recipient != "" && recipient != base {
		return
	}
	h.mu.Lock()
	if h.target != target {
		h.mu.Unlock()
		return
	}
	h.challengedScopes = c.scopes
	h.mu.Unlock()
	if c.resourceMetadata != "" {
		h.setAdvertisedPRMURL(c.resourceMetadata, recipient)
	}
}

// validateAdvertisedPRMURL enforces that a PRM URL advertised by the
// resource (i.e. parsed from an untrusted WWW-Authenticate header) shares
// the configured base URL's scheme and host. Returns a non-nil error when
// the candidate is unparseable, carries a different scheme or host, or
// when no base URL has been configured to validate against.
//
// RFC 9728 §3.2 requires the protected resource to serve its own metadata;
// this check rejects attempts to redirect discovery to an unrelated
// origin, which would otherwise let the resource point the client at an
// attacker-controlled OAuth metadata endpoint.
func (h *OAuthHandler) validateAdvertisedPRMURL(candidate string) error {
	h.metadataMu.Lock()
	baseURL := h.baseURL
	h.metadataMu.Unlock()
	return checkAdvertisedPRMURL(candidate, baseURL)
}

// ChallengedScopes returns the scopes the last WWW-Authenticate challenge
// named, which the next authorization asks for first.
func (h *OAuthHandler) ChallengedScopes() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return slices.Clone(h.challengedScopes)
}

// scopesToRequest picks the scopes of an authorization request, following
// the MCP scope selection strategy: the challenged scopes first, as they
// are authoritative for the refused operation, then the configured ones and
// those the current token was granted, so a step-up loses nothing; failing
// all of those, the protected resource's scopes_supported; failing that,
// none.
func (h *OAuthHandler) scopesToRequest(ctx context.Context) []string {
	h.mu.RLock()
	challenged := h.challengedScopes
	h.mu.RUnlock()
	// What the current token was granted is kept only from a token for this
	// server: another server's grant is not this one's to ask for.
	var granted []string
	if token, err := h.config.TokenStore.GetToken(ctx); err == nil && h.tokenIsForThisServer(token) {
		granted = strings.Fields(token.Scope)
	}
	if scopes := unionScopes(challenged, h.config.Scopes, granted); len(scopes) > 0 {
		return scopes
	}
	h.metadataMu.Lock()
	defer h.metadataMu.Unlock()
	return slices.Clone(h.resourceScopes)
}

// unionScopes concatenates the lists, keeping each scope once, at its first
// position.
func unionScopes(lists ...[]string) []string {
	var union []string
	seen := make(map[string]struct{})
	for _, list := range lists {
		for _, scope := range list {
			if _, dup := seen[scope]; dup {
				continue
			}
			seen[scope] = struct{}{}
			union = append(union, scope)
		}
	}
	return union
}
