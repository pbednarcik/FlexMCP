package transport

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

// maxMetadataBodyBytes caps the size of an OAuth metadata document we are
// willing to read.
const maxMetadataBodyBytes = 1 << 20 // 1 MiB

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

// pkceMethod is the only code challenge method the client uses.
const pkceMethod = "S256"

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
)

// OAuthConfig holds the OAuth configuration for the client
type OAuthConfig struct {
	// ClientID is the OAuth client ID
	ClientID string
	// ClientURI is the URI of the client
	ClientURI string
	// ClientSecret is the OAuth client secret (for confidential clients)
	ClientSecret string
	// ClientIssuer is the issuer of the authorization server ClientID and
	// ClientSecret belong to. Client credentials are used with the server
	// that issued them and no other (MCP authorization). When it is empty,
	// they are bound to the first authorization server they are used with;
	// set it, from configuration or from GetClientIssuer after a dynamic
	// registration, to bind them from the first request.
	ClientIssuer string
	// TokenEndpointAuthMethod is how the client authenticates at the token
	// endpoint: "client_secret_basic", "client_secret_post" or "none". Set it
	// for pre-registered credentials, or for credentials saved from an
	// earlier dynamic registration (see GetTokenEndpointAuthMethod), when the
	// authorization server supports more than one method. When empty, the
	// method returned by dynamic client registration is used, or else
	// client_secret_basic if the server's metadata lists it but not
	// client_secret_post, and client_secret_post otherwise.
	TokenEndpointAuthMethod string
	// RedirectURI is the redirect URI for the OAuth flow
	RedirectURI string
	// Scopes is the list of OAuth scopes to request
	Scopes []string
	// TokenStore is the storage for OAuth tokens
	TokenStore TokenStore
	// AuthServerMetadataURL is the URL to the OAuth server metadata
	// If empty, the client will attempt to discover it from the base URL
	AuthServerMetadataURL string
	// ProtectedResourceMetadataURL is the URL to the OAuth protected resource metadata
	// per RFC9728. If set, this URL will be used to discover the authorization server.
	// This is typically extracted from the WWW-Authenticate header's resource_metadata parameter.
	ProtectedResourceMetadataURL string
	// PKCEEnabled used to switch PKCE on. PKCE is always on: MCP clients
	// must implement it, with S256, and must verify that the authorization
	// server supports it before sending the user there.
	//
	// Deprecated: the field has no effect. See PKCEDisabled.
	PKCEEnabled bool
	// PKCEDisabled turns PKCE off: no code challenge is sent, no verifier is
	// required, and the authorization server's metadata is not checked for
	// code_challenge_methods_supported. Turning it off weakens security and
	// breaks the MCP authorization rules; it is meant only for an
	// authorization server known to lack PKCE.
	PKCEDisabled bool
	// SkipIssuerMetadataValidation turns off the check that authorization
	// server metadata found through protected resource metadata declares the
	// issuer it was requested for (RFC 8414 §3.3). Turning it off weakens
	// security; it is meant only for authorization servers known to publish
	// a mismatched issuer.
	SkipIssuerMetadataValidation bool
	// HTTPClient is an optional HTTP client to use for requests.
	// If nil, a default HTTP client with a 30 second timeout will be used.
	HTTPClient *http.Client
}

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
	// record.
	Issuer string `json:"issuer,omitempty"`
}

// IsExpired returns true if the token is expired
func (t *Token) IsExpired() bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().After(t.ExpiresAt)
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

// AuthServerMetadata represents the OAuth 2.0 Authorization Server Metadata
// as defined in RFC 8414 (https://www.rfc-editor.org/rfc/rfc8414.html).
//
// URL-bearing fields are validated by validateAuthServerMetadataURLs to
// reject non-http(s) schemes (e.g. javascript:, data:, file:) that could
// otherwise be injected by a hostile authorization server.
//
// Signed metadata (the "signed_metadata" JWT field) is not supported.
type AuthServerMetadata struct {
	Issuer                                             string   `json:"issuer"`
	AuthorizationEndpoint                              string   `json:"authorization_endpoint"`
	TokenEndpoint                                      string   `json:"token_endpoint"`
	JwksURI                                            string   `json:"jwks_uri,omitempty"`
	RegistrationEndpoint                               string   `json:"registration_endpoint,omitempty"`
	ScopesSupported                                    []string `json:"scopes_supported,omitempty"`
	ResponseTypesSupported                             []string `json:"response_types_supported"`
	ResponseModesSupported                             []string `json:"response_modes_supported,omitempty"`
	GrantTypesSupported                                []string `json:"grant_types_supported,omitempty"`
	TokenEndpointAuthMethodsSupported                  []string `json:"token_endpoint_auth_methods_supported,omitempty"`
	TokenEndpointAuthSigningAlgValuesSupported         []string `json:"token_endpoint_auth_signing_alg_values_supported,omitempty"`
	ServiceDocumentation                               string   `json:"service_documentation,omitempty"`
	UILocalesSupported                                 []string `json:"ui_locales_supported,omitempty"`
	OpPolicyURI                                        string   `json:"op_policy_uri,omitempty"`
	OpTOSURI                                           string   `json:"op_tos_uri,omitempty"`
	RevocationEndpoint                                 string   `json:"revocation_endpoint,omitempty"`
	RevocationEndpointAuthMethodsSupported             []string `json:"revocation_endpoint_auth_methods_supported,omitempty"`
	RevocationEndpointAuthSigningAlgValuesSupported    []string `json:"revocation_endpoint_auth_signing_alg_values_supported,omitempty"`
	IntrospectionEndpoint                              string   `json:"introspection_endpoint,omitempty"`
	IntrospectionEndpointAuthMethodsSupported          []string `json:"introspection_endpoint_auth_methods_supported,omitempty"`
	IntrospectionEndpointAuthSigningAlgValuesSupported []string `json:"introspection_endpoint_auth_signing_alg_values_supported,omitempty"`
	CodeChallengeMethodsSupported                      []string `json:"code_challenge_methods_supported,omitempty"`
	// AuthorizationResponseIssParameterSupported says the server puts iss in
	// its authorization responses (RFC 9207 §2.3); a client then rejects a
	// response without it.
	AuthorizationResponseIssParameterSupported bool `json:"authorization_response_iss_parameter_supported,omitempty"`
}

// OAuthHandler handles OAuth authentication for HTTP requests
type OAuthHandler struct {
	config           OAuthConfig
	httpClient       *http.Client
	serverMetadata   *AuthServerMetadata
	metadataFetchErr error
	// metadataOnce gates the discovery RPCs so they run once per
	// OAuthHandler, or again after a discovery that failed. It is a pointer
	// (rather than an embedded value) so SetProtectedResourceMetadataURL
	// can swap in a fresh sync.Once without racing against a concurrently
	// running Do on the previous instance. The pointer itself is protected by metadataMu; callers
	// snapshot the pointer under the lock and then invoke Do without
	// holding the lock.
	metadataOnce *sync.Once
	baseURL      string
	// advertisedPRMURL is the protected resource metadata URL the MCP server
	// advertised in its challenge (RFC 9728 §5.1), validated against baseURL;
	// it belongs to that server, so a change of baseURL forgets it, where
	// config.ProtectedResourceMetadataURL is the caller's and stays.
	advertisedPRMURL string
	// metadataMu protects baseURL, advertisedPRMURL, serverMetadata,
	// metadataFetchErr, metadataOnce, config.ProtectedResourceMetadataURL,
	// resourceURL and resourceScopes. It MUST NOT be held across HTTP I/O —
	// see getServerMetadata for the snapshot/publish discipline that keeps
	// network round trips out of the critical section (#871).
	metadataMu  sync.Mutex
	resourceURL string // RFC 8707 resource indicator; set from protected resource metadata
	// resourceScopes is scopes_supported from the protected resource
	// metadata: what to ask for when no challenge and no configuration says.
	resourceScopes []string

	// registeredAuthMethod is the token_endpoint_auth_method the
	// authorization server registered for the client during dynamic client
	// registration, if any; registeredIssuer is that server's issuer, which
	// the credentials belong to.
	registeredAuthMethod string
	registeredIssuer     string

	// mu protects expectedState, expectedIssuer, issuerRequired,
	// expectedResource, challengedScopes, and the client credentials:
	// config.ClientID, config.ClientSecret, registeredAuthMethod and
	// registeredIssuer, which are replaced and read together.
	mu            sync.RWMutex
	expectedState string // Expected state value for CSRF protection
	// challengedScopes are the scopes the last WWW-Authenticate challenge
	// named: authoritative for the operation that was refused, so the next
	// authorization asks for them first.
	challengedScopes []string
	// expectedIssuer is the issuer of the authorization server the user was
	// sent to, recorded with the state (RFC 9207 §2.4); issuerRequired says
	// that server advertises iss, so a response without it is rejected.
	expectedIssuer string
	issuerRequired bool
	// expectedResource is the resource the authorization in progress was
	// started for, the only one its code is redeemed for (RFC 8707 §2.1).
	expectedResource string
	// target counts the changes of base URL, so an authorization that
	// started before one is not recorded after it.
	target uint64
	// testHookFlowSnapshot, nil outside tests, runs when an authorization has
	// read the server it is for and has not yet recorded itself.
	testHookFlowSnapshot func()

	// refreshMu protects refreshing, the token refresh in flight, if any.
	// Callers of this handler that find the token expired share it: a
	// refresh token may be good for a single use, so they must not each
	// send it.
	refreshMu  sync.Mutex
	refreshing *tokenRefresh
}

// clientCredentials is one snapshot of the client credentials, taken under
// mu.
type clientCredentials struct {
	id, secret, authMethod string
}

// tokenRefresh is a token refresh shared by the callers that wait for it.
type tokenRefresh struct {
	done  chan struct{}
	token *Token
	err   error
}

// NewOAuthHandler creates a new OAuth handler
func NewOAuthHandler(config OAuthConfig) *OAuthHandler {
	if config.TokenStore == nil {
		config.TokenStore = NewMemoryTokenStore()
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}

	return &OAuthHandler{
		config:           config,
		httpClient:       config.HTTPClient,
		registeredIssuer: config.ClientIssuer,
	}
}

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
	h.metadataMu.Unlock()
	if recipient != "" && recipient != base {
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

	return fmt.Sprintf("%s %s", tokenType, token.AccessToken), nil
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
	if err == nil && !token.IsExpired() && token.AccessToken != "" {
		return token, nil
	}
	if err != nil || token.RefreshToken == "" {
		// We need to get a new token through the authorization flow
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
		return nil, ErrOAuthAuthorizationRequired
	}
	return refresh.token, nil
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
	token, err := h.config.TokenStore.GetToken(ctx)
	if err != nil {
		return nil, err
	}
	if !token.IsExpired() && token.AccessToken != "" {
		return token, nil
	}
	if token.RefreshToken == "" {
		return nil, ErrOAuthAuthorizationRequired
	}
	return h.refreshToken(ctx, token)
}

// refreshToken spends the refresh token of stored, whose record, read with
// it, says where the refresh token may go: only to the authorization server
// that issued it, for the server it was issued for. A refresh token without a
// record carries none.
func (h *OAuthHandler) refreshToken(ctx context.Context, stored *Token) (*Token, error) {
	metadata, err := h.getServerMetadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get server metadata: %w", err)
	}

	// RFC 8707: every token request names the resource.
	resourceURL := h.getResourceURL()
	if resourceURL == "" {
		return nil, ErrResourceUnknown
	}
	if stored.Issuer != "" && stored.Issuer != metadata.Issuer {
		return nil, fmt.Errorf("%w: the refresh token is from %q, the token endpoint belongs to %q", ErrIssuerMismatch, stored.Issuer, metadata.Issuer)
	}
	if !h.tokenIsForThisServer(stored) {
		return nil, fmt.Errorf("%w: the refresh token is for %q", ErrOAuthAuthorizationRequired, stored.Resource)
	}
	refreshToken := stored.RefreshToken
	data := url.Values{}
	data.Set("grant_type", "refresh_token")
	data.Set("refresh_token", refreshToken)
	data.Set("resource", resourceURL)

	req, err := h.newTokenRequest(ctx, metadata, data)
	if err != nil {
		return nil, fmt.Errorf("failed to create refresh token request: %w", err)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send refresh token request: %w", err)
	}
	defer resp.Body.Close()

	// Accept any 2xx status; some authorization servers (e.g. Supabase) return
	// 201 Created for successful token responses.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return nil, extractOAuthError(body, resp.StatusCode, "refresh token request failed")
	}

	// Read the response body for parsing
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read token response body: %w", err)
	}

	// GitHub returns HTTP 200 even for errors, with error details in the JSON body
	// Check if the response contains an error field before parsing as Token
	var oauthErr OAuthError
	if err := json.Unmarshal(body, &oauthErr); err == nil && oauthErr.ErrorCode != "" {
		return nil, fmt.Errorf("refresh token request failed: %w", oauthErr)
	}

	var tokenResp Token
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to decode token response: %w", err)
	}

	// Set expiration time
	if tokenResp.ExpiresIn > 0 {
		tokenResp.ExpiresAt = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	}

	// If no new refresh token is provided, keep the old one
	if tokenResp.RefreshToken == "" {
		tokenResp.RefreshToken = refreshToken
	}
	tokenResp.Resource = resourceURL
	tokenResp.Issuer = metadata.Issuer

	// Save the token
	if err := h.config.TokenStore.SaveToken(ctx, &tokenResp); err != nil {
		return nil, fmt.Errorf("failed to save token: %w", err)
	}

	return &tokenResp, nil
}

// RefreshToken refreshes a token with refreshToken. When it is the stored
// token's, the stored record decides where it may go; one brought from
// elsewhere has none. A token store that cannot be read refuses the refresh.
func (h *OAuthHandler) RefreshToken(ctx context.Context, refreshToken string) (*Token, error) {
	token := &Token{RefreshToken: refreshToken}
	stored, err := h.config.TokenStore.GetToken(ctx)
	switch {
	case err == nil && stored.RefreshToken == refreshToken:
		token = stored
	case err != nil && !errors.Is(err, ErrNoToken):
		return nil, fmt.Errorf("failed to read the token store: %w", err)
	}
	return h.refreshToken(ctx, token)
}

// GetClientID returns the client ID
func (h *OAuthHandler) GetClientID() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.config.ClientID
}

// extractOAuthError attempts to parse an OAuth error response from the response body
func extractOAuthError(body []byte, statusCode int, context string) error {
	// Try to parse the error as an OAuth error response
	var oauthErr OAuthError
	if err := json.Unmarshal(body, &oauthErr); err == nil && oauthErr.ErrorCode != "" {
		return fmt.Errorf("%s: %w", context, oauthErr)
	}

	// If not a valid OAuth error, return the raw response
	return fmt.Errorf("%s with status %d: %s", context, statusCode, body)
}

// SetProtectedResourceMetadataURL updates the protected resource metadata URL
// and resets the cached server metadata so it will be re-discovered on the next call.
//
// This setter does not validate the URL; callers that pass values obtained
// out of band are trusted. For values parsed from a 401 WWW-Authenticate
// header, prefer HandleUnauthorizedResponse, which applies origin
// validation before storing.
func (h *OAuthHandler) SetProtectedResourceMetadataURL(u string) {
	h.metadataMu.Lock()
	defer h.metadataMu.Unlock()
	h.config.ProtectedResourceMetadataURL = u
	h.forgetDiscoveryLocked()
}

// setAdvertisedPRMURL records the metadata URL the MCP server at recipient
// advertised, already validated against the base URL, and discovers again
// from it, unless the handler no longer targets recipient (empty: its own
// target).
func (h *OAuthHandler) setAdvertisedPRMURL(u, recipient string) {
	h.metadataMu.Lock()
	defer h.metadataMu.Unlock()
	if recipient != "" && recipient != h.baseURL {
		return
	}
	h.advertisedPRMURL = u
	h.forgetDiscoveryLocked()
}

// forgetDiscoveryLocked discards what discovery found, so the next call
// discovers again. A fresh sync.Once is installed, rather than the old one
// mutated, so a Do in flight on the old one cannot publish a stale result
// (getServerMetadata checks the pointer). metadataMu is held by the caller.
func (h *OAuthHandler) forgetDiscoveryLocked() {
	h.serverMetadata = nil
	h.metadataFetchErr = nil
	h.metadataOnce = &sync.Once{}
	h.resourceURL = ""
	h.resourceScopes = nil
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
	if baseURL == "" {
		return errors.New("no base URL configured for origin validation")
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("invalid base URL %q: %w", baseURL, err)
	}
	// url.Parse accepts relative references (empty Scheme and Host)
	// without error. Reject those explicitly so two empty values do not
	// EqualFold-match each other and bypass origin validation.
	if base.Scheme == "" || base.Host == "" {
		return fmt.Errorf("base URL %q is not absolute (missing scheme or host)", baseURL)
	}
	parsed, err := url.Parse(candidate)
	if err != nil {
		return fmt.Errorf("invalid advertised PRM URL %q: %w", candidate, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("advertised PRM URL %q is not absolute (missing scheme or host)", candidate)
	}
	if !strings.EqualFold(parsed.Scheme, base.Scheme) {
		return fmt.Errorf("advertised PRM URL scheme %q does not match base %q", parsed.Scheme, base.Scheme)
	}
	if !strings.EqualFold(parsed.Host, base.Host) {
		return fmt.Errorf("advertised PRM URL host %q does not match base %q", parsed.Host, base.Host)
	}
	return nil
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

// getResourceURL returns the RFC 8707 resource indicator under metadataMu.
func (h *OAuthHandler) getResourceURL() string {
	h.metadataMu.Lock()
	defer h.metadataMu.Unlock()
	return h.resourceURL
}

// GetClientIssuer returns the issuer of the authorization server the client
// credentials are bound to, or "" while they are bound to none: what to
// persist the credentials under, beside GetClientID and GetClientSecret.
func (h *OAuthHandler) GetClientIssuer() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.registeredIssuer
}

// GetClientSecret returns the client secret
func (h *OAuthHandler) GetClientSecret() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.config.ClientSecret
}

// SetBaseURL sets the URL of the MCP server, which discovery starts from and
// which the resource parameter names. The transports set it when they are
// built; a handler used on its own needs it before authorization starts. A
// change discards what discovery found, and the metadata URL the previous
// server advertised, so the next call discovers again for the new server.
func (h *OAuthHandler) SetBaseURL(baseURL string) {
	h.metadataMu.Lock()
	if h.baseURL == baseURL {
		h.metadataMu.Unlock()
		return
	}
	h.baseURL = baseURL
	h.advertisedPRMURL = ""
	h.forgetDiscoveryLocked()
	h.metadataMu.Unlock()

	// An authorization in progress was for the previous server: it is
	// abandoned, so its callback is refused rather than redeemed here. So
	// are the scopes that server's last challenge named.
	h.mu.Lock()
	h.target++
	h.expectedState, h.expectedIssuer, h.issuerRequired, h.expectedResource = "", "", false, ""
	h.challengedScopes = nil
	h.mu.Unlock()
}

// GetExpectedState returns the expected state value (for testing purposes)
func (h *OAuthHandler) GetExpectedState() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.expectedState
}

// SetExpectedState sets the expected state value.
//
// This can be useful if you cannot maintain an OAuthHandler
// instance throughout the authentication flow; for example, if
// the initialization and callback steps are handled in different
// requests.
//
// In such cases, this should be called with the state value generated
// during the initial authentication request (e.g. by GenerateState)
// and included in the authorization URL, and SetExpectedIssuer with the
// issuer GetAuthorizationURL recorded.
func (h *OAuthHandler) SetExpectedState(expectedState string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expectedState = expectedState
}

// GetExpectedIssuer returns the issuer of the authorization server the user
// was last sent to by GetAuthorizationURL, or set by SetExpectedIssuer.
func (h *OAuthHandler) GetExpectedIssuer() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.expectedIssuer
}

// SetExpectedIssuer sets the issuer an authorization response must name, and
// whether a response without iss is rejected, for a callback handled by
// another handler instance than the one that built the authorization URL.
// Take both from the handler that did: GetExpectedIssuer and the metadata's
// authorization_response_iss_parameter_supported.
func (h *OAuthHandler) SetExpectedIssuer(issuer string, required bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expectedIssuer = issuer
	h.issuerRequired = required
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

// OAuthProtectedResource represents the response from /.well-known/oauth-protected-resource
type OAuthProtectedResource struct {
	AuthorizationServers []string `json:"authorization_servers"`
	Resource             string   `json:"resource"`
	ResourceName         string   `json:"resource_name,omitempty"`
	// ScopesSupported is the minimal set of scopes for basic use of the
	// resource, requested when nothing else says which scopes to ask for.
	ScopesSupported []string `json:"scopes_supported,omitempty"`
}

// metadataDiscoveryResult bundles the outputs of a single OAuth server
// metadata discovery attempt so that fetchServerMetadata can return them
// to getServerMetadata for publication under metadataMu, instead of
// mutating shared state directly while holding the lock across HTTP I/O.
type metadataDiscoveryResult struct {
	metadata        *AuthServerMetadata
	resourceURL     string
	scopesSupported []string // from the protected resource metadata, when found
}

// getServerMetadata fetches the OAuth server metadata.
//
// Discovery is gated by metadataOnce so the network round trips happen
// once per OAuthHandler, or again after a discovery that failed. metadataMu
// is taken only to snapshot configuration inputs before the fetch and to
// publish the result afterwards; it is intentionally NOT held while HTTP
// requests are in flight so that concurrent callers of other
// metadataMu-guarded methods (e.g. SetProtectedResourceMetadataURL,
// getResourceURL, validateAdvertisedPRMURL) are not blocked on network I/O.
// See #871.
func (h *OAuthHandler) getServerMetadata(ctx context.Context) (*AuthServerMetadata, error) {
	// Snapshot (and lazily initialize) the current sync.Once under the
	// lock. Using a pointer means SetProtectedResourceMetadataURL can
	// install a fresh once without racing with a Do already in flight on
	// the previous instance.
	h.metadataMu.Lock()
	if h.metadataOnce == nil {
		h.metadataOnce = &sync.Once{}
	}
	once := h.metadataOnce
	h.metadataMu.Unlock()

	once.Do(func() {
		result, err := h.fetchServerMetadata(ctx)
		if err == nil && result.metadata != nil {
			err = requireHTTPSEndpoints(result.metadata)
		}
		h.metadataMu.Lock()
		defer h.metadataMu.Unlock()
		// If SetProtectedResourceMetadataURL swapped in a fresh sync.Once
		// while the fetch was in flight, the configuration that produced
		// `result` is now stale. Discard the result instead of letting it
		// clobber the newer state; the next getServerMetadata call will
		// drive re-discovery through the new once.
		if h.metadataOnce != once {
			return
		}
		// A failed discovery is reported to the callers waiting on it, but
		// not kept: it may come from the first caller's context ending or
		// from a network error, so the next call discovers again. Each
		// discovery reports its own failure, not one left by the last.
		h.metadataFetchErr = nil
		defer func() {
			if h.metadataFetchErr != nil {
				h.metadataOnce = &sync.Once{}
			}
		}()
		if err != nil {
			h.metadataFetchErr = err
			return
		}
		// Only overwrite serverMetadata on a positive result. Preserving the
		// existing value on a no-op fetch (nil metadata, nil error) matches
		// the prior behavior where fetchMetadataFromURL silently returned on
		// non-200 responses without touching shared state.
		if result.metadata != nil {
			h.serverMetadata = result.metadata
			h.metadataFetchErr = nil
		}
		if result.resourceURL != "" {
			h.resourceURL = result.resourceURL
		}
		if result.scopesSupported != nil {
			h.resourceScopes = result.scopesSupported
		}
		// Discovery completed without producing metadata or an error (e.g. a
		// non-2xx response handled by fetchMetadataFromURL). Record an explicit
		// error — including the discovery target — so callers receive an
		// error instead of a nil *AuthServerMetadata, which they would
		// dereference and panic on. Skip when serverMetadata is already
		// populated: a no-op re-discovery preserves the prior value.
		if h.serverMetadata == nil && h.metadataFetchErr == nil {
			if h.config.AuthServerMetadataURL != "" {
				h.metadataFetchErr = fmt.Errorf("authorization server metadata unavailable: discovery at %q returned no metadata", h.config.AuthServerMetadataURL)
			} else {
				h.metadataFetchErr = errors.New("authorization server metadata unavailable: auto-discovery from the server base URL returned no metadata")
			}
		}
	})

	h.metadataMu.Lock()
	defer h.metadataMu.Unlock()
	if h.metadataFetchErr != nil {
		return nil, h.metadataFetchErr
	}
	// Defensive backstop: a concurrent SetProtectedResourceMetadataURL may have
	// reset state after our once ran. Never return (nil, nil) — every caller
	// dereferences the returned metadata.
	if h.serverMetadata == nil {
		return nil, errors.New("authorization server metadata unavailable")
	}
	return h.serverMetadata, nil
}

// fetchServerMetadata performs OAuth server metadata discovery without
// holding metadataMu, so concurrent callers reading other state guarded
// by the mutex are not blocked on network I/O. Configuration inputs are
// snapshotted under the lock before the first request is issued; the
// caller (getServerMetadata) is responsible for publishing the returned
// result back into the handler under metadataMu.
func (h *OAuthHandler) fetchServerMetadata(ctx context.Context) (metadataDiscoveryResult, error) {
	// Snapshot config and base URL under the lock; release before any HTTP I/O.
	h.metadataMu.Lock()
	authServerMetadataURL := h.config.AuthServerMetadataURL
	// The metadata URL the server advertised in its challenge, validated
	// against the base URL, comes first; else the one the caller configured.
	protectedResourceMetadataURL := h.advertisedPRMURL
	if protectedResourceMetadataURL == "" {
		protectedResourceMetadataURL = h.config.ProtectedResourceMetadataURL
	}
	validateIssuer := !h.config.SkipIssuerMetadataValidation
	baseURL := h.baseURL
	h.metadataMu.Unlock()

	// The base URL is the resource until protected resource metadata names
	// one, which is checked where it is taken.
	if baseURL != "" {
		if err := checkResourceIdentifier(baseURL); err != nil {
			return metadataDiscoveryResult{}, err
		}
	}

	// RFC 8707: the resource parameter names the MCP server on every
	// authorization and token request, whether or not the authorization
	// server supports it. Protected resource metadata may name the resource
	// below; until it does, and on every path where there is none, the MCP
	// server's own URL is the resource. Without one, the authorization and
	// token requests refuse with ErrResourceUnknown.
	resourceURL := baseURL

	// If AuthServerMetadataURL is explicitly provided, use it directly
	if authServerMetadataURL != "" {
		metadata, err := h.fetchMetadataFromURL(ctx, authServerMetadataURL)
		if err != nil {
			return metadataDiscoveryResult{}, err
		}
		return metadataDiscoveryResult{metadata: metadata, resourceURL: resourceURL}, nil
	}

	// Discovery from here starts from the MCP server's URL and nowhere
	// else: the redirect URI names the callback listener, which is no
	// resource, and a metadata document found at its origin must not make
	// it one.
	if baseURL == "" {
		return metadataDiscoveryResult{}, ErrResourceUnknown
	}

	// Determine the protected resource metadata URL with priority:
	// 1. Explicit: advertised in the challenge (RFC 9728 §5.1) or configured
	// 2. Constructed from base URL
	var protectedResourceURL string
	explicitMetadataURL := protectedResourceMetadataURL != ""
	if explicitMetadataURL {
		protectedResourceURL = protectedResourceMetadataURL
	} else {
		var err error
		protectedResourceURL, err = buildWellKnownURL(baseURL, "oauth-protected-resource")
		if err != nil {
			return metadataDiscoveryResult{}, fmt.Errorf("failed to build protected resource URL: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, protectedResourceURL, nil)
	if err != nil {
		return metadataDiscoveryResult{}, fmt.Errorf("failed to create protected resource request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	// Advertise the latest protocol version this client supports. Metadata
	// discovery happens before the MCP initialize handshake, so no negotiated
	// version is available yet; servers that don't recognise it can fall back
	// per the MCP spec.
	req.Header.Set("MCP-Protocol-Version", mcp.LATEST_PROTOCOL_VERSION)

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return metadataDiscoveryResult{}, fmt.Errorf("failed to send protected resource request: %w", err)
	}
	defer resp.Body.Close()

	// If we can't get the protected resource metadata, try OAuth Authorization Server discovery.
	// However, if the resource_metadata URL was explicitly provided (via RFC 9728), don't
	// fall back to baseURL-derived discovery — the server specifically indicated where to
	// find metadata, so falling back would mask the signal.
	if resp.StatusCode != http.StatusOK {
		if explicitMetadataURL {
			return metadataDiscoveryResult{}, fmt.Errorf("protected resource metadata discovery failed for explicit URL %q: status %d", protectedResourceURL, resp.StatusCode)
		}
		// A server failure is no answer: the metadata may well be there.
		if isServerFailure(resp.StatusCode) {
			return metadataDiscoveryResult{}, fmt.Errorf("protected resource metadata request to %q failed: status %d", protectedResourceURL, resp.StatusCode)
		}
		var fetchErr error
		for _, u := range authorizationServerMetadataURLs(baseURL) {
			// Any failed fetch moves on to the next candidate URL, but metadata
			// that may be there keeps the default endpoints from standing in.
			metadata, err := h.fetchMetadataFromURL(ctx, u)
			if err != nil {
				if errors.Is(err, errMetadataUnavailable) {
					fetchErr = cmp.Or(fetchErr, err)
				}
				continue
			}
			if metadata == nil {
				continue
			}
			// Without protected resource metadata the MCP server is its own
			// authorization server (the 2025-03-26 revision), so the metadata
			// found here names the server as its issuer (RFC 8414 §3.3). Naming
			// another would let the server claim an issuer it is not, and the
			// client credentials bound to that issuer.
			if validateIssuer && !isOwnIssuer(metadata.Issuer, baseURL) {
				return metadataDiscoveryResult{}, fmt.Errorf(
					"authorization server metadata from %q declares issuer %q, not the MCP server %q",
					u, metadata.Issuer, baseURL,
				)
			}
			return metadataDiscoveryResult{metadata: metadata, resourceURL: resourceURL}, nil
		}
		if fetchErr != nil {
			return metadataDiscoveryResult{}, fetchErr
		}
		// If that also fails, fall back to default endpoints
		metadata, err := h.getDefaultEndpoints(baseURL)
		if err != nil {
			return metadataDiscoveryResult{}, fmt.Errorf("failed to get default endpoints: %w", err)
		}
		return metadataDiscoveryResult{metadata: metadata, resourceURL: resourceURL}, nil
	}

	// Parse the protected resource metadata
	var protectedResource OAuthProtectedResource
	if err := json.NewDecoder(resp.Body).Decode(&protectedResource); err != nil {
		return metadataDiscoveryResult{}, fmt.Errorf("failed to decode protected resource response: %w", err)
	}

	// RFC 9728 §3.3/§7.3: the resource identifier a protected resource
	// metadata document declares must match the protected resource the
	// client addressed — otherwise the document MUST NOT be used. Without
	// the check, a malicious MCP server could declare another server as its
	// resource and have tokens minted for that victim: the RFC 8707
	// resource parameter taken from the metadata names the victim, so the
	// authorization server mints a token for the victim's API while the
	// attacker's server receives it.
	//
	// Both discovery paths enforce the binding, at different strictness.
	// A PRM URL advertised via WWW-Authenticate is untrusted input that may
	// not share an origin with the protected resource, so its document must
	// declare a resource identifier exactly equal to the base URL, and a
	// document that omits the resource field is rejected outright: with no
	// explicit identifier there is no binding at all. The well-known path is
	// constructed from the base URL, but the document it serves is still
	// server-controlled, so a declared resource must at least bind to the
	// addressed resource — same scheme and host, and a path equal to or a
	// segment-aware prefix of the addressed path (see resourceBindsToURL) —
	// so servers mounted at /mcp that declare their origin as the resource
	// keep working while a resource naming another server is rejected.
	if explicitMetadataURL {
		if protectedResource.Resource == "" {
			return metadataDiscoveryResult{}, fmt.Errorf(
				"advertised protected resource metadata from %q omits required resource field",
				protectedResourceURL,
			)
		}
		if !resourceIdentifiersEqual(protectedResource.Resource, baseURL) {
			return metadataDiscoveryResult{}, fmt.Errorf(
				"advertised protected resource metadata declares resource %q which does not match base URL %q",
				protectedResource.Resource, baseURL,
			)
		}
	} else if protectedResource.Resource != "" && !resourceBindsToURL(protectedResource.Resource, baseURL) {
		return metadataDiscoveryResult{}, fmt.Errorf(
			"protected resource metadata from %q declares resource %q which does not match base URL %q",
			protectedResourceURL, protectedResource.Resource, baseURL,
		)
	}

	// RFC 8707: the resource identifier the metadata declares names the
	// resource from here on; without one, the base URL stays, per RFC 8707
	// Section 2: "The client SHOULD use the base URI of the API as the
	// resource parameter value unless specific knowledge of the resource
	// dictates otherwise."
	if protectedResource.Resource != "" {
		// Binding above compares the parsed fragment, and an empty one ("#")
		// parses as none: the declared identifier is checked as such too.
		if err := checkResourceIdentifier(protectedResource.Resource); err != nil {
			return metadataDiscoveryResult{}, fmt.Errorf("protected resource metadata from %q: %w", protectedResourceURL, err)
		}
		resourceURL = protectedResource.Resource
	}
	scopesSupported := protectedResource.ScopesSupported

	// If no authorization servers are specified, fall back to default endpoints
	if len(protectedResource.AuthorizationServers) == 0 {
		metadata, err := h.getDefaultEndpoints(baseURL)
		if err != nil {
			return metadataDiscoveryResult{}, fmt.Errorf("failed to get default endpoints: %w", err)
		}
		return metadataDiscoveryResult{metadata: metadata, resourceURL: resourceURL, scopesSupported: scopesSupported}, nil
	}

	// Use the first authorization server
	authServerURL := protectedResource.AuthorizationServers[0]

	// Try the MCP-specified discovery URLs in order (RFC 8414 with path
	// insertion, plus OpenID Connect Discovery variants).
	var fetchErr error
	for _, u := range authorizationServerMetadataURLs(authServerURL) {
		// Any failed fetch moves on to the next candidate URL, but metadata
		// that may be there keeps the default endpoints from standing in.
		metadata, err := h.fetchMetadataFromURL(ctx, u)
		if err != nil {
			if errors.Is(err, errMetadataUnavailable) {
				fetchErr = cmp.Or(fetchErr, err)
			}
			continue
		}
		if metadata == nil {
			continue
		}
		// RFC 8414 §3.3 / OpenID Connect Discovery §4.3: the issuer in the
		// document must be identical to the issuer the well-known URL was
		// built from, or the metadata must not be used. Otherwise a server
		// could send the client to one authorization server's endpoints
		// under another's name. Only a final trailing slash is ignored, as
		// in the official Go SDK: both spellings name the same server.
		if validateIssuer && strings.TrimSuffix(metadata.Issuer, "/") != strings.TrimSuffix(authServerURL, "/") {
			return metadataDiscoveryResult{}, fmt.Errorf(
				"authorization server metadata from %q declares issuer %q, not %q",
				u, metadata.Issuer, authServerURL,
			)
		}
		return metadataDiscoveryResult{metadata: metadata, resourceURL: resourceURL, scopesSupported: scopesSupported}, nil
	}

	if fetchErr != nil {
		return metadataDiscoveryResult{}, fetchErr
	}

	// If both discovery methods fail, use default endpoints based on the authorization server URL
	metadata, err := h.getDefaultEndpoints(authServerURL)
	if err != nil {
		return metadataDiscoveryResult{}, fmt.Errorf("failed to get default endpoints: %w", err)
	}
	return metadataDiscoveryResult{metadata: metadata, resourceURL: resourceURL, scopesSupported: scopesSupported}, nil
}

// isOwnIssuer reports whether issuer names the MCP server at baseURL itself:
// its origin, the 2025-03-26 revision's authorization base URL, or the base
// URL the metadata URLs were built from. Only a final slash is ignored.
func isOwnIssuer(issuer, baseURL string) bool {
	base, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	issuer = strings.TrimSuffix(issuer, "/")
	return issuer == base.Scheme+"://"+base.Host || issuer == strings.TrimSuffix(baseURL, "/")
}

// buildWellKnownURL constructs a well-known discovery URL by inserting the
// given suffix between the authority and path of baseURL (RFC 8414 §3 /
// RFC 9728 path-insertion semantics).
func buildWellKnownURL(baseURL string, suffix string) (string, error) {
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("failed to parse base URL: %w", err)
	}

	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		return "", fmt.Errorf("invalid base URL: missing scheme or host in %q", baseURL)
	}

	path := strings.TrimSuffix(parsedURL.EscapedPath(), "/")
	root := fmt.Sprintf("%s://%s", parsedURL.Scheme, parsedURL.Host)
	if path == "" || path == "/" {
		return root + "/.well-known/" + suffix, nil
	}

	return root + "/.well-known/" + suffix + path, nil
}

// resourceComponentsEqual reports whether two parsed resource identifiers
// agree on every component an RFC 9728 §3.3 binding check compares outside
// the path: scheme and host case-insensitively per RFC 3986 §3.1 / §3.2.2,
// and query, fragment, and userinfo, which are significant.
func resourceComponentsEqual(a, b *url.URL) bool {
	if !strings.EqualFold(a.Scheme, b.Scheme) {
		return false
	}
	if !strings.EqualFold(a.Host, b.Host) {
		return false
	}
	if a.RawQuery != b.RawQuery {
		return false
	}
	if a.Fragment != b.Fragment {
		return false
	}
	return a.User.String() == b.User.String()
}

// checkResourceIdentifier refuses what cannot be a resource parameter: RFC
// 8707 §2 wants an absolute URI without a fragment, and MCP authorization
// the server's canonical URI, with a scheme and a host.
func checkResourceIdentifier(resource string) error {
	parsed, err := url.Parse(resource)
	if err != nil {
		return fmt.Errorf("%w: %q: %w", ErrInvalidResource, resource, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("%w: %q has no scheme or no host", ErrInvalidResource, resource)
	}
	if strings.Contains(resource, "#") {
		return fmt.Errorf("%w: %q has a fragment", ErrInvalidResource, resource)
	}
	return nil
}

// resourceIdentifiersEqual reports whether two OAuth protected resource
// identifiers refer to the same resource for the purposes of RFC 9728 §3.3
// equality checks. Scheme and host are compared case-insensitively per
// RFC 3986 §3.1 / §3.2.2, and a single trailing slash on either path is
// ignored because real-world OAuth deployments routinely emit the
// resource with or without it for the same URL; rejecting that variant
// would produce false positives on legitimate servers. Query, fragment,
// and userinfo components are significant. Unparseable inputs fall back
// to exact string equality.
func resourceIdentifiersEqual(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil {
		return a == b
	}
	if !resourceComponentsEqual(ua, ub) {
		return false
	}
	// Use EscapedPath rather than Path so percent-encoded reserved
	// characters stay distinct from their decoded forms (e.g. "a%2Fb"
	// must not compare equal to "a/b"), preserving RFC 3986 segment
	// semantics.
	return strings.TrimSuffix(ua.EscapedPath(), "/") == strings.TrimSuffix(ub.EscapedPath(), "/")
}

// resourceBindsToURL reports whether the resource identifier declared in a
// protected resource metadata document fetched from the RFC 9728 §3.3
// well-known path binds to the addressed URL (the client's base URL): the
// two must agree on scheme and host (case-insensitively) and on query,
// fragment, and userinfo, and the declared resource's path must equal or
// be a path-prefix of the addressed path at a segment boundary — "/a"
// binds "/a" and "/a/b" but not "/ab" — with a single trailing slash on
// either path ignored, as in resourceIdentifiersEqual.
//
// The prefix rule is deliberately looser than the exact equality required
// of a PRM URL advertised via WWW-Authenticate: the well-known URL is
// constructed from the base URL, so the document is bound to the
// resource's origin by construction, but real deployments commonly mount
// their MCP endpoint at a path (e.g. https://host/mcp) while declaring the
// bare origin (https://host) as their resource.
//
// Unparseable inputs fall back to exact string equality, as in
// resourceIdentifiersEqual.
func resourceBindsToURL(resource, addressed string) bool {
	ur, errR := url.Parse(resource)
	ub, errB := url.Parse(addressed)
	if errR != nil || errB != nil {
		return resource == addressed
	}
	if !resourceComponentsEqual(ur, ub) {
		return false
	}
	// Use EscapedPath rather than Path for the same RFC 3986 segment
	// semantics as resourceIdentifiersEqual.
	resourcePath := strings.TrimSuffix(ur.EscapedPath(), "/")
	addressedPath := strings.TrimSuffix(ub.EscapedPath(), "/")
	if resourcePath == addressedPath {
		return true
	}
	// Segment-aware prefix: the resource path must end on a segment
	// boundary of the addressed path, so "/a" must not bind "/ab". An
	// empty resource path (a bare origin) binds any path on that origin.
	return strings.HasPrefix(addressedPath, resourcePath+"/")
}

// errMetadataUnavailable marks a metadata fetch that got no answer: the
// request failed or was cancelled, or the server failed. The metadata may
// well be there, so it is not taken for absent.
var errMetadataUnavailable = errors.New("metadata unavailable")

// fetchMetadataFromURL fetches and parses OAuth server metadata from a URL.
// Returns (nil, nil) when the server says the metadata is not there (a
// non-200 status other than a server failure) so the caller can fall
// through to the next candidate URL. Network and server failures
// (errMetadataUnavailable), decode, and validation failures are returned to
// the caller.
//
// This function performs HTTP I/O and must not be called while holding
// metadataMu (see #871).
func (h *OAuthHandler) fetchMetadataFromURL(ctx context.Context, metadataURL string) (*AuthServerMetadata, error) {
	if !isHTTPS(metadataURL) {
		return nil, fmt.Errorf("%w: metadata URL %q", ErrInsecureAuthorizationServer, metadataURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create metadata request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	// Advertise the latest protocol version this client supports. Metadata
	// discovery happens before the MCP initialize handshake, so no negotiated
	// version is available yet; servers that don't recognise it can fall back
	// per the MCP spec.
	req.Header.Set("MCP-Protocol-Version", mcp.LATEST_PROTOCOL_VERSION)

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to send metadata request: %w", errMetadataUnavailable, err)
	}
	defer resp.Body.Close()

	if isServerFailure(resp.StatusCode) {
		return nil, fmt.Errorf("%w: metadata request to %q failed: status %d", errMetadataUnavailable, metadataURL, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}

	var metadata AuthServerMetadata
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxMetadataBodyBytes))
	if err := dec.Decode(&metadata); err != nil {
		return nil, fmt.Errorf("failed to decode metadata response: %w", err)
	}

	if err := validateAuthServerMetadataURLs(&metadata); err != nil {
		return nil, fmt.Errorf("invalid authorization server metadata from %s: %w", metadataURL, err)
	}

	return &metadata, nil
}

// isHTTPS reports whether rawURL is an https URL.
func isHTTPS(rawURL string) bool {
	u, err := url.Parse(rawURL)
	return err == nil && strings.EqualFold(u.Scheme, "https")
}

// requireHTTPSEndpoints refuses authorization server metadata whose issuer or
// any endpoint is not https, whether discovery found it or guessed it:
// authorization server endpoints are served over HTTPS only (MCP
// authorization, Communication Security).
func requireHTTPSEndpoints(m *AuthServerMetadata) error {
	for _, f := range []struct{ name, value string }{
		{"issuer", m.Issuer},
		{"authorization_endpoint", m.AuthorizationEndpoint},
		{"token_endpoint", m.TokenEndpoint},
		{"registration_endpoint", m.RegistrationEndpoint},
		{"jwks_uri", m.JwksURI},
		{"revocation_endpoint", m.RevocationEndpoint},
		{"introspection_endpoint", m.IntrospectionEndpoint},
	} {
		if f.value != "" && !isHTTPS(f.value) {
			return fmt.Errorf("%w: %s is %q", ErrInsecureAuthorizationServer, f.name, f.value)
		}
	}
	return nil
}

// isServerFailure reports whether status says the server failed to answer,
// rather than that what was asked for is not there.
func isServerFailure(status int) bool {
	return status >= http.StatusInternalServerError || status == http.StatusTooManyRequests
}

// validateAuthServerMetadataURLs ensures every URL-bearing field in m uses an
// http or https scheme. This prevents a hostile authorization server from
// advertising values like "javascript:..." or "file:..." that could be
// reflected into a browser or the local file system by downstream consumers.
// Empty optional fields are permitted.
func validateAuthServerMetadataURLs(m *AuthServerMetadata) error {
	fields := []struct {
		name  string
		value string
	}{
		{"issuer", m.Issuer},
		{"authorization_endpoint", m.AuthorizationEndpoint},
		{"token_endpoint", m.TokenEndpoint},
		{"jwks_uri", m.JwksURI},
		{"registration_endpoint", m.RegistrationEndpoint},
		{"service_documentation", m.ServiceDocumentation},
		{"op_policy_uri", m.OpPolicyURI},
		{"op_tos_uri", m.OpTOSURI},
		{"revocation_endpoint", m.RevocationEndpoint},
		{"introspection_endpoint", m.IntrospectionEndpoint},
	}
	for _, f := range fields {
		if f.value == "" {
			continue
		}
		u, err := url.Parse(f.value)
		if err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
		scheme := strings.ToLower(u.Scheme)
		if scheme != "http" && scheme != "https" {
			return fmt.Errorf("%s has disallowed scheme %q", f.name, u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("%s is missing host", f.name)
		}
	}
	return nil
}

// authorizationServerMetadataURLs returns the ordered list of discovery URLs to
// try for a given issuer URL, following the MCP authorization spec. This
// implements RFC 8414 §3 path-insertion semantics (the well-known segment is
// inserted between the authority and the issuer path, not appended), plus
// OpenID Connect Discovery 1.0 fallbacks.
func authorizationServerMetadataURLs(issuerURL string) []string {
	u, err := url.Parse(issuerURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil
	}
	u.RawQuery = ""
	u.Fragment = ""

	originalPath := strings.Trim(u.Path, "/")

	var urls []string
	if originalPath == "" {
		u.Path = "/.well-known/oauth-authorization-server"
		urls = append(urls, u.String())
		u.Path = "/.well-known/openid-configuration"
		urls = append(urls, u.String())
		return urls
	}
	u.Path = "/.well-known/oauth-authorization-server/" + originalPath
	urls = append(urls, u.String())
	u.Path = "/.well-known/openid-configuration/" + originalPath
	urls = append(urls, u.String())
	u.Path = "/" + originalPath + "/.well-known/openid-configuration"
	urls = append(urls, u.String())
	return urls
}

// GetServerMetadata is a public wrapper for getServerMetadata
func (h *OAuthHandler) GetServerMetadata(ctx context.Context) (*AuthServerMetadata, error) {
	return h.getServerMetadata(ctx)
}

// getDefaultEndpoints returns default OAuth endpoints based on the base URL
func (h *OAuthHandler) getDefaultEndpoints(baseURL string) (*AuthServerMetadata, error) {
	// Parse the base URL to extract the authority
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse base URL: %w", err)
	}

	// Discard any path component to get the authorization base URL
	parsedURL.Path = ""
	authBaseURL := parsedURL.String()

	// Validate that the URL has a scheme and host
	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		return nil, fmt.Errorf("invalid base URL: missing scheme or host in %q", baseURL)
	}

	return &AuthServerMetadata{
		Issuer:                authBaseURL,
		AuthorizationEndpoint: authBaseURL + "/authorize",
		TokenEndpoint:         authBaseURL + "/token",
		RegistrationEndpoint:  authBaseURL + "/register",
	}, nil
}

// RegisterClient performs dynamic client registration
func (h *OAuthHandler) RegisterClient(ctx context.Context, clientName string) error {
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

	// RFC 8707: Include resource parameter in client registration
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

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send registration request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return extractOAuthError(body, resp.StatusCode, "registration request failed")
	}

	var regResponse struct {
		ClientID                string `json:"client_id"`
		ClientSecret            string `json:"client_secret,omitempty"`
		TokenEndpointAuthMethod string `json:"token_endpoint_auth_method,omitempty"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&regResponse); err != nil {
		return fmt.Errorf("failed to decode registration response: %w", err)
	}

	// The registration replaces the whole credential set: a secret from an
	// earlier registration, or from configuration, does not stay beside the
	// new client ID. RFC 7591 §3.2.1: the response carries the method the
	// server actually registered, which may differ from the one requested.
	h.mu.Lock()
	h.config.ClientID = regResponse.ClientID
	h.config.ClientSecret = regResponse.ClientSecret
	h.registeredAuthMethod = regResponse.TokenEndpointAuthMethod
	h.registeredIssuer = metadata.Issuer
	h.mu.Unlock()

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

// ErrInvalidState is returned when the state parameter doesn't match the expected value
var ErrInvalidState = errors.New("invalid state parameter, possible CSRF attack")

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

	req, err := h.newTokenRequest(ctx, metadata, data)
	if err != nil {
		return fmt.Errorf("failed to create token request: %w", err)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send token request: %w", err)
	}
	defer resp.Body.Close()

	// Accept any 2xx status; some authorization servers (e.g. Supabase) return
	// 201 Created for successful token responses.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return extractOAuthError(body, resp.StatusCode, "token request failed")
	}

	// Read the response body for parsing
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read token response body: %w", err)
	}

	// GitHub returns HTTP 200 even for errors, with error details in the JSON body
	// Check if the response contains an error field before parsing as Token
	var oauthErr OAuthError
	if err := json.Unmarshal(body, &oauthErr); err == nil && oauthErr.ErrorCode != "" {
		return fmt.Errorf("token request failed: %w", oauthErr)
	}

	var tokenResp Token
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return fmt.Errorf("failed to decode token response: %w", err)
	}

	// Set expiration time
	if tokenResp.ExpiresIn > 0 {
		tokenResp.ExpiresAt = time.Now().Add(time.Duration(tokenResp.ExpiresIn) * time.Second)
	}
	// Record what the token is for and who issued it.
	tokenResp.Resource = resourceURL
	tokenResp.Issuer = metadata.Issuer

	// Save the token
	if err := h.config.TokenStore.SaveToken(ctx, &tokenResp); err != nil {
		return fmt.Errorf("failed to save token: %w", err)
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
// client's credentials.
func (h *OAuthHandler) newTokenRequest(ctx context.Context, metadata *AuthServerMetadata, data url.Values) (*http.Request, error) {
	creds, err := h.credentialsFor(metadata)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		// RFC 6749 §2.3.1: the client ID and secret are form-encoded before
		// they are used as the Basic user name and password.
		req.SetBasicAuth(url.QueryEscape(creds.id), url.QueryEscape(creds.secret))
	}
	return req, nil
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

	return metadata.AuthorizationEndpoint + "?" + params.Encode(), nil
}
