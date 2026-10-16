package transport

import (
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// maxMetadataBodyBytes caps the size of an OAuth metadata document, or any
// other response from a server discovery chose, that we are willing to
// read: past it a document is refused and an error body cut.
const maxMetadataBodyBytes = 1 << 20 // 1 MiB

// pkceMethod is the only code challenge method the client uses.
const pkceMethod = "S256"

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
	// registration, to bind them from the first request. It is also the only
	// issuer a refresh token saved without Token.Issuer is sent to.
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

// OAuthHandler handles OAuth authentication for HTTP requests
type OAuthHandler struct {
	config OAuthConfig
	// discoveryClient and credentialClient are copies of the configured
	// client, sharing its transport, that differ in what a redirect may do:
	// discovery follows one only while it stays on https; a request carrying
	// credentials, a code or a refresh token follows none (see
	// newRedirectPolicies).
	discoveryClient  *http.Client
	credentialClient *http.Client
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
	// resourceURL, knownIssuer and resourceScopes. It MUST NOT be held across HTTP I/O —
	// see getServerMetadata for the snapshot/publish discipline that keeps
	// network round trips out of the critical section (#871). The token check
	// every request makes reads under RLock; everything else takes Lock.
	metadataMu  sync.RWMutex
	resourceURL string // RFC 8707 resource indicator; set from protected resource metadata
	// knownIssuer is the issuer discovery last named for the base URL. It
	// survives a reset of discovery, so a token from another issuer stays
	// refused until discovery names one again; a change of base URL forgets
	// it.
	knownIssuer string
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
	// expectedResource, challengedScopes, target, and the client credentials:
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

	// refreshMu protects refreshing, the token refresh in flight, if any, and
	// refused.
	// Callers of this handler that find the token expired share it: a
	// refresh token may be good for a single use, so they must not each
	// send it.
	refreshMu  sync.Mutex
	refreshing *tokenRefresh
	// refused, under refreshMu, is the last refresh token the authorization
	// server refused with an OAuth error; it is not sent again. A network
	// failure or a temporary one (refusesRefreshToken) refuses nothing, and
	// is tried again.
	refused string

	// saveMu makes checking and saving a token one step, so an older token
	// request cannot overwrite what a newer one saved, and a replacement of
	// the client credentials waits for a save in progress. tokenGen counts
	// what supersedes a token request in flight: each code exchange and each
	// replacement of the client credentials. A request saves only if
	// tokenGen is still what it was before the request read the token it
	// spends.
	saveMu   sync.Mutex
	tokenGen atomic.Uint64
}

// clientCredentials is one snapshot of the client credentials, taken under
// mu.
type clientCredentials struct {
	id, secret, authMethod string
}

// NewOAuthHandler creates a new OAuth handler
func NewOAuthHandler(config OAuthConfig) *OAuthHandler {
	if config.TokenStore == nil {
		config.TokenStore = NewMemoryTokenStore()
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}

	discovery, credential := newRedirectPolicies(config.HTTPClient)
	return &OAuthHandler{
		config:           config,
		discoveryClient:  discovery,
		credentialClient: credential,
		registeredIssuer: config.ClientIssuer,
	}
}

// GetClientID returns the client ID
func (h *OAuthHandler) GetClientID() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.config.ClientID
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
	h.knownIssuer = ""
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
