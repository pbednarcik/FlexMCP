package transport

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// ProcessAuthorizationResponse processes an authorization response that
// carries no iss, and exchanges the code for a token.
//
// It is [OAuthHandler.ProcessAuthorizationCallback] for a response with only
// code and state. An authorization server that advertises iss sends it on
// every response, so against one this method refuses (ErrIssuerMissing):
// hand the callback's query to ProcessAuthorizationCallback instead.
func (h *OAuthHandler) ProcessAuthorizationResponse(ctx context.Context, code, state, codeVerifier string) error {
	return h.ProcessAuthorizationCallback(ctx, url.Values{"code": {code}, "state": {state}}, codeVerifier)
}

// ProcessAuthorizationCallback processes the authorization response the
// user agent brought to the redirect URI, given as the callback's query
// parameters, and exchanges the code for a token.
//
// The state is checked first and consumed, with the issuer recorded beside
// it. Then the issuer (RFC 9207, as the MCP authorization rules apply it):
// an iss that is present must equal, as a plain string, the issuer the user
// was sent to; an iss that is absent is rejected when that authorization
// server advertises authorization_response_iss_parameter_supported. Only
// then is an error response acted on, returned as an OAuthError, and the
// code exchanged.
func (h *OAuthHandler) ProcessAuthorizationCallback(ctx context.Context, params url.Values, codeVerifier string) error {
	expectedIssuer, issuerRequired, expectedResource, err := h.consumeExpectedState(params.Get("state"))
	if err != nil {
		return err
	}
	issuer, err := h.validateIssuer(ctx, params.Get("iss"), expectedIssuer, issuerRequired)
	if err != nil {
		return err
	}
	if code := params.Get("error"); code != "" {
		return OAuthError{
			ErrorCode:        code,
			ErrorDescription: params.Get("error_description"),
			ErrorURI:         params.Get("error_uri"),
		}
	}
	return h.exchangeAuthorizationCode(ctx, params.Get("code"), codeVerifier, issuer, expectedResource)
}

// consumeExpectedState checks state against the expected one and clears the
// record, returning the issuer and the resource recorded with it: all are
// for one authorization response.
func (h *OAuthHandler) consumeExpectedState(state string) (expectedIssuer string, issuerRequired bool, expectedResource string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.expectedState == "" {
		return "", false, "", errors.New("no expected state found, authorization flow may not have been initiated properly")
	}
	if state != h.expectedState {
		return "", false, "", ErrInvalidState
	}
	expectedIssuer, issuerRequired, expectedResource = h.expectedIssuer, h.issuerRequired, h.expectedResource
	h.expectedState, h.expectedIssuer, h.issuerRequired, h.expectedResource = "", "", false, ""
	return expectedIssuer, issuerRequired, expectedResource, nil
}

// validateIssuer applies RFC 9207 §2.4 to the iss of an authorization
// response, and returns the issuer the flow is bound to. expected is the
// issuer recorded when the user was sent to the authorization server; a
// handler that recorded none, as when the callback is handled by another
// instance, takes it from the server's metadata.
func (h *OAuthHandler) validateIssuer(ctx context.Context, iss, expected string, required bool) (string, error) {
	if expected == "" {
		metadata, err := h.getServerMetadata(ctx)
		if err != nil {
			return "", fmt.Errorf("failed to get server metadata: %w", err)
		}
		expected, required = metadata.Issuer, metadata.AuthorizationResponseIssParameterSupported
	}
	switch {
	case iss == "" && required:
		return "", ErrIssuerMissing
	case iss != "" && iss != expected:
		return "", fmt.Errorf("%w: got %q, expected %q", ErrIssuerMismatch, iss, expected)
	}
	return expected, nil
}

// exchangeAuthorizationCode redeems the code at the token endpoint of
// issuer, the authorization server the user was sent to, for resource, the
// one the flow was started for (empty when the flow was started by another
// handler instance). Discovery may have led elsewhere since; the code is not
// sent there, nor redeemed for another resource.
func (h *OAuthHandler) exchangeAuthorizationCode(ctx context.Context, code, codeVerifier, issuer, resource string) error {
	if codeVerifier == "" && !h.config.PKCEDisabled {
		return ErrPKCERequired
	}
	// A new authorization supersedes every token request already in flight.
	gen := h.tokenGen.Add(1)

	metadata, err := h.getServerMetadata(ctx)
	if err != nil {
		return fmt.Errorf("failed to get server metadata: %w", err)
	}
	if metadata.Issuer != issuer {
		return fmt.Errorf("%w: the code is from %q, the token endpoint belongs to %q", ErrIssuerMismatch, issuer, metadata.Issuer)
	}

	// RFC 8707: every token request names the resource, and the code is
	// redeemed only for the one it was obtained for.
	resourceURL := h.getResourceURL()
	if resourceURL == "" {
		return ErrResourceUnknown
	}
	if resource != "" && !resourceIdentifiersEqual(resource, resourceURL) {
		return fmt.Errorf("%w: started for %q, the handler now names %q", ErrResourceChanged, resource, resourceURL)
	}
	data := url.Values{}
	data.Set("grant_type", "authorization_code")
	data.Set("code", code)
	data.Set("redirect_uri", h.config.RedirectURI)
	data.Set("resource", resourceURL)

	if codeVerifier != "" {
		data.Set("code_verifier", codeVerifier)
	}

	req, clientID, err := h.newTokenRequest(ctx, metadata, data)
	if err != nil {
		return fmt.Errorf("failed to create token request: %w", err)
	}

	tokenResp, _, err := h.doTokenRequest(req, "token request")
	if err != nil {
		return err
	}
	// Record what the token is for, who issued it and to which client.
	tokenResp.Resource = resourceURL
	tokenResp.Issuer = metadata.Issuer
	tokenResp.ClientID = clientID

	return h.saveIssuedToken(ctx, tokenResp, gen)
}

// GetAuthorizationURL returns the URL for the authorization endpoint.
//
// codeChallenge is the S256 challenge of the code verifier the caller will
// hand to ProcessAuthorizationResponse (see GenerateCodeVerifier and
// GenerateCodeChallenge). It is required, and the authorization server must
// list S256 in its metadata, unless OAuthConfig.PKCEDisabled is set.
func (h *OAuthHandler) GetAuthorizationURL(ctx context.Context, state, codeChallenge string) (string, error) {
	h.mu.RLock()
	target := h.target
	h.mu.RUnlock()
	metadata, err := h.getServerMetadata(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get server metadata: %w", err)
	}
	creds, err := h.credentialsFor(metadata)
	if err != nil {
		return "", err
	}
	// The flow is for an MCP server, which the resource parameter names on
	// this request and the token request; without one there is no flow.
	resourceURL := h.getResourceURL()
	if resourceURL == "" {
		return "", ErrResourceUnknown
	}

	if !h.config.PKCEDisabled {
		if codeChallenge == "" {
			return "", ErrPKCERequired
		}
		// OAuth 2.1 and PKCE define no other way to learn whether the server
		// supports PKCE, so the metadata decides: without the field, or
		// without S256 in it, the flow does not start.
		if !slices.Contains(metadata.CodeChallengeMethodsSupported, pkceMethod) {
			return "", fmt.Errorf("%w: %q lists %v in code_challenge_methods_supported", ErrPKCENotSupported, metadata.Issuer, metadata.CodeChallengeMethodsSupported)
		}
	}

	if h.testHookFlowSnapshot != nil {
		h.testHookFlowSnapshot()
	}
	// Record the state, and the issuer the user is sent to, for the
	// response's validation (RFC 9207: the recorded issuer comes from the
	// validated metadata, never from the response). An authorization the
	// base URL changed under is for the previous server, and abandoned as
	// SetBaseURL abandons one in progress.
	h.mu.Lock()
	if h.target != target {
		h.mu.Unlock()
		return "", fmt.Errorf("%w: the base URL changed while the authorization started", ErrResourceChanged)
	}
	h.expectedState = state
	h.expectedIssuer = metadata.Issuer
	h.issuerRequired = metadata.AuthorizationResponseIssParameterSupported
	h.expectedResource = resourceURL
	h.mu.Unlock()

	params := url.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", creds.id)
	params.Set("redirect_uri", h.config.RedirectURI)
	params.Set("state", state)

	if scopes := h.scopesToRequest(ctx); len(scopes) > 0 {
		params.Set("scope", strings.Join(scopes, " "))
	}

	if codeChallenge != "" {
		params.Set("code_challenge", codeChallenge)
		params.Set("code_challenge_method", pkceMethod)
	}

	// RFC 8707: the resource parameter names the MCP server
	params.Set("resource", resourceURL)

	// The endpoint's own query is kept (RFC 6749, section 3.1).
	separator := "?"
	if strings.Contains(metadata.AuthorizationEndpoint, "?") {
		separator = "&"
	}
	return metadata.AuthorizationEndpoint + separator + params.Encode(), nil
}
