package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// RegisterClient performs dynamic client registration
func (h *OAuthHandler) RegisterClient(ctx context.Context, clientName string) error {
	// Called from inside this handler's own SaveToken, the registration
	// would wait for the save it was called from.
	if ctx.Value(savingKey{h}) != nil {
		return errSaveReentered
	}
	metadata, err := h.getServerMetadata(ctx)
	if err != nil {
		return fmt.Errorf("failed to get server metadata: %w", err)
	}

	if metadata.RegistrationEndpoint == "" {
		return errors.New("server does not support dynamic client registration")
	}

	// Prepare registration request
	regRequest := map[string]any{
		"client_name":                clientName,
		"redirect_uris":              []string{h.config.RedirectURI},
		"application_type":           applicationType(h.config.RedirectURI),
		"token_endpoint_auth_method": "none", // For public clients
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"scope":                      strings.Join(h.config.Scopes, " "),
	}

	if h.config.ClientURI != "" {
		regRequest["client_uri"] = h.config.ClientURI
	}

	// Add client_secret if this is a confidential client
	if h.GetClientSecret() != "" {
		regRequest["token_endpoint_auth_method"] = "client_secret_post"
	}

	// The MCP server the client registers for. RFC 7591 defines no such
	// member; a server that does not know it ignores it (RFC 7591 §2).
	if resourceURL := h.getResourceURL(); resourceURL != "" {
		regRequest["resource"] = resourceURL
	}

	reqBody, err := json.Marshal(regRequest)
	if err != nil {
		return fmt.Errorf("failed to marshal registration request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		metadata.RegistrationEndpoint,
		bytes.NewReader(reqBody),
	)
	if err != nil {
		return fmt.Errorf("failed to create registration request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := h.credentialClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send registration request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBodyBytes))
		return extractOAuthError(body, resp.StatusCode, "registration request failed")
	}

	var regResponse struct {
		ClientID                string `json:"client_id"`
		ClientSecret            string `json:"client_secret,omitempty"`
		TokenEndpointAuthMethod string `json:"token_endpoint_auth_method,omitempty"`
	}

	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetadataBodyBytes)).Decode(&regResponse); err != nil {
		return fmt.Errorf("failed to decode registration response: %w", err)
	}
	if regResponse.ClientID == "" {
		return fmt.Errorf("%w: no client_id", ErrInvalidRegistrationResponse)
	}

	// The registration replaces the whole credential set: a secret from an
	// earlier registration, or from configuration, does not stay beside the
	// new client ID. RFC 7591 §3.2.1: the response carries the method the
	// server actually registered, which may differ from the one requested.
	// A token request in flight was made with the replaced credentials, so it
	// is superseded; one that is saving finishes first.
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	h.mu.Lock()
	h.config.ClientID = regResponse.ClientID
	h.config.ClientSecret = regResponse.ClientSecret
	h.registeredAuthMethod = regResponse.TokenEndpointAuthMethod
	h.registeredIssuer = metadata.Issuer
	h.mu.Unlock()
	h.tokenGen.Add(1)

	return nil
}

// applicationType is the OAuth application_type a client registers with
// (RFC 7591 §2): web for a client that takes its redirect over HTTPS, native
// for one that takes it on localhost or a custom scheme.
func applicationType(redirectURI string) string {
	if parsed, err := url.Parse(redirectURI); err == nil && strings.EqualFold(parsed.Scheme, "https") {
		return "web"
	}
	return "native"
}

// credentialsFor returns the client credentials for use with the
// authorization server of metadata, refusing them for any server but the one
// they belong to (MCP authorization: never reuse them with another).
// Credentials from dynamic registration belong to the server that issued
// them, configured ones to OAuthConfig.ClientIssuer, or else to the first
// server they are used with, which this call binds them to. The credentials
// returned are the ones checked: a registration cannot replace them between.
func (h *OAuthHandler) credentialsFor(metadata *AuthServerMetadata) (clientCredentials, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.registeredIssuer == "" {
		if h.config.ClientID != "" {
			h.registeredIssuer = metadata.Issuer
		}
	} else if metadata.Issuer != h.registeredIssuer {
		return clientCredentials{}, fmt.Errorf("%w: registered with %q, discovered %q", ErrClientRegisteredElsewhere, h.registeredIssuer, metadata.Issuer)
	}
	return clientCredentials{id: h.config.ClientID, secret: h.config.ClientSecret, authMethod: h.registeredAuthMethod}, nil
}

// checkTokenResponse refuses a token response without an access token, or
// with one of a type other than Bearer, before anything from it is saved
// (RFC 6749 §5.1, §7.1). A response that names no type is taken as Bearer,
// as a stored token without one is sent.
func checkTokenResponse(token *Token) error {
	if token.AccessToken == "" {
		return fmt.Errorf("%w: no access_token", ErrInvalidTokenResponse)
	}
	if token.TokenType != "" && !strings.EqualFold(token.TokenType, "bearer") {
		return fmt.Errorf("%w: token_type %q is not Bearer", ErrInvalidTokenResponse, token.TokenType)
	}
	return nil
}

// Client authentication methods at the token endpoint (RFC 6749 §2.3.1,
// RFC 7591 §2).
const (
	clientSecretBasic = "client_secret_basic"
	clientSecretPost  = "client_secret_post"
	clientAuthNone    = "none"
)

func isClientAuthMethod(method string) bool {
	return method == clientSecretBasic || method == clientSecretPost || method == clientAuthNone
}

// GetTokenEndpointAuthMethod returns the client authentication method set in
// OAuthConfig.TokenEndpointAuthMethod or, failing that, the one the
// authorization server returned from dynamic client registration. Save it
// along with the client ID and secret to reuse registered credentials.
func (h *OAuthHandler) GetTokenEndpointAuthMethod() string {
	h.mu.RLock()
	registered := h.registeredAuthMethod
	h.mu.RUnlock()
	return h.authMethod(registered)
}

// authMethod is the configured client authentication method or, failing
// that, registered.
func (h *OAuthHandler) authMethod(registered string) string {
	if isClientAuthMethod(h.config.TokenEndpointAuthMethod) {
		return h.config.TokenEndpointAuthMethod
	}
	if isClientAuthMethod(registered) {
		return registered
	}
	return ""
}

// tokenEndpointAuthMethod picks how the client authenticates at the token
// endpoint: the method configured or registered for it, or else HTTP Basic
// when that is the only one of the two secret-based methods the server's
// metadata lists. A server that says nothing keeps getting the secret in the
// request body, as before.
func (h *OAuthHandler) tokenEndpointAuthMethod(metadata *AuthServerMetadata, creds clientCredentials) string {
	if creds.secret == "" {
		return clientAuthNone
	}
	if method := h.authMethod(creds.authMethod); method != "" {
		return method
	}
	supported := metadata.TokenEndpointAuthMethodsSupported
	if slices.Contains(supported, clientSecretBasic) && !slices.Contains(supported, clientSecretPost) {
		return clientSecretBasic
	}
	return clientSecretPost
}

// newTokenRequest builds a token endpoint request carrying data and the
// client's credentials, and returns the ID of the client it authenticates.
func (h *OAuthHandler) newTokenRequest(ctx context.Context, metadata *AuthServerMetadata, data url.Values) (*http.Request, string, error) {
	creds, err := h.credentialsFor(metadata)
	if err != nil {
		return nil, "", err
	}
	method := h.tokenEndpointAuthMethod(metadata, creds)
	basic := method == clientSecretBasic
	if !basic {
		data.Set("client_id", creds.id)
		if method == clientSecretPost {
			data.Set("client_secret", creds.secret)
		}
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		metadata.TokenEndpoint,
		strings.NewReader(data.Encode()),
	)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		// RFC 6749 §2.3.1: the client ID and secret are form-encoded before
		// they are used as the Basic user name and password.
		req.SetBasicAuth(url.QueryEscape(creds.id), url.QueryEscape(creds.secret))
	}
	return req, creds.id, nil
}

// doTokenRequest sends req, the token request what names in its errors, and
// returns the token issued, with its expiry set, and the response's status,
// zero when none arrived. An OAuthError in the error is the authorization
// server's answer.
func (h *OAuthHandler) doTokenRequest(req *http.Request, what string) (*Token, int, error) {
	resp, err := h.credentialClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to send %s: %w", what, err)
	}
	defer resp.Body.Close()

	// Accept any 2xx status; some authorization servers (e.g. Supabase) return
	// 201 Created for successful token responses.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBodyBytes))
		return nil, resp.StatusCode, extractOAuthError(body, resp.StatusCode, what+" failed")
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBodyBytes))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("failed to read token response body: %w", err)
	}

	// GitHub returns HTTP 200 even for errors, with error details in the JSON body
	// Check if the response contains an error field before parsing as Token
	var oauthErr OAuthError
	if err := json.Unmarshal(body, &oauthErr); err == nil && oauthErr.ErrorCode != "" {
		return nil, resp.StatusCode, fmt.Errorf("%s failed: %w", what, oauthErr)
	}

	var token Token
	if err := json.Unmarshal(body, &token); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("failed to decode token response: %w", err)
	}
	if err := checkTokenResponse(&token); err != nil {
		return nil, resp.StatusCode, err
	}
	if token.ExpiresIn > 0 {
		token.ExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)
	}
	return &token, resp.StatusCode, nil
}
