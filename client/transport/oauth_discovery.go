package transport

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/pbednarcik/flexmcp/mcp"
)

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

// getResourceURL returns the RFC 8707 resource indicator under metadataMu.
func (h *OAuthHandler) getResourceURL() string {
	h.metadataMu.Lock()
	defer h.metadataMu.Unlock()
	return h.resourceURL
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
			err = requireIssuer(result.metadata)
		}
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
			h.knownIssuer = result.metadata.Issuer
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

	// Determine the protected resource metadata URLs with priority:
	// 1. Explicit: advertised in the challenge (RFC 9728 §5.1) or configured
	// 2. Constructed from the base URL: at the MCP server's path, then at the
	//    host's root (MCP authorization, Protected Resource Metadata Discovery)
	explicitMetadataURL := protectedResourceMetadataURL != ""
	candidates := []string{protectedResourceMetadataURL}
	if !explicitMetadataURL {
		var err error
		candidates, err = protectedResourceMetadataURLs(baseURL)
		if err != nil {
			return metadataDiscoveryResult{}, fmt.Errorf("failed to build protected resource URL: %w", err)
		}
	}
	var protectedResourceURL string
	var resp *http.Response
	for _, candidate := range candidates {
		protectedResourceURL = candidate
		var err error
		resp, err = h.requestProtectedResource(ctx, candidate)
		if err != nil {
			return metadataDiscoveryResult{}, err
		}
		if resp.StatusCode == http.StatusOK {
			break
		}
		resp.Body.Close()
		// If the resource_metadata URL was explicitly provided (via RFC 9728),
		// don't fall back to baseURL-derived discovery: the server indicated
		// where to find the metadata, so falling back would mask the signal.
		if explicitMetadataURL {
			return metadataDiscoveryResult{}, fmt.Errorf("protected resource metadata discovery failed for explicit URL %q: status %d", protectedResourceURL, resp.StatusCode)
		}
		// A server failure is no answer: the metadata may well be there.
		if isServerFailure(resp.StatusCode) {
			return metadataDiscoveryResult{}, fmt.Errorf("protected resource metadata request to %q failed: status %d", protectedResourceURL, resp.StatusCode)
		}
		resp = nil
	}

	// Without protected resource metadata, try OAuth Authorization Server
	// discovery from the MCP server's own URL.
	if resp == nil {
		var fetchErr error
		for _, c := range legacyMetadataCandidates(baseURL) {
			// Any failed fetch moves on to the next candidate URL, but metadata
			// that may be there keeps the default endpoints from standing in.
			metadata, err := h.fetchMetadataFromURL(ctx, c.url)
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
			// authorization server (the 2025-03-26 revision), and metadata names
			// the issuer its URL was built from (RFC 8414 §3.3). Naming another
			// would let whoever serves that URL claim an issuer it is not, and
			// the client credentials bound to that issuer. Metadata under the
			// server's path that does so is not used, and the host's root, which
			// only its origin speaks for, is asked next.
			if validateIssuer && strings.TrimSuffix(metadata.Issuer, "/") != c.issuer {
				if c.underPath {
					continue
				}
				return metadataDiscoveryResult{}, fmt.Errorf(
					"authorization server metadata from %q declares issuer %q, not %q",
					c.url, metadata.Issuer, c.issuer,
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
	defer resp.Body.Close()

	// Parse the protected resource metadata
	var protectedResource OAuthProtectedResource
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxMetadataBodyBytes)).Decode(&protectedResource); err != nil {
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

// legacyCandidate is a URL the 2025-03-26 fallback fetches authorization
// server metadata from, and the issuer metadata found there must name: the
// identifier the URL was built from (RFC 8414 §3.3), without a final slash.
type legacyCandidate struct {
	url, issuer string
	// underPath marks a URL built from the MCP server's path, which speaks
	// for that server only, not for the host's root.
	underPath bool
}

// legacyMetadataCandidates returns, in order, the URLs built from the MCP
// server's URL when it has a path, then those built from its origin, the
// 2025-03-26 revision's authorization base URL.
func legacyMetadataCandidates(baseURL string) []legacyCandidate {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil
	}
	origin := base.Scheme + "://" + base.Host
	var candidates []legacyCandidate
	if strings.Trim(base.Path, "/") != "" {
		for _, u := range authorizationServerMetadataURLs(baseURL) {
			candidates = append(candidates, legacyCandidate{url: u, issuer: strings.TrimSuffix(baseURL, "/"), underPath: true})
		}
	}
	for _, u := range authorizationServerMetadataURLs(origin) {
		candidates = append(candidates, legacyCandidate{url: u, issuer: origin})
	}
	return candidates
}

// protectedResourceMetadataURLs lists where protected resource metadata is
// looked for when no URL was advertised or configured: at the MCP server's
// path (RFC 9728 §3.1 path insertion), then at the host's root, the order MCP
// authorization prescribes. A server at the root has the one URL.
func protectedResourceMetadataURLs(baseURL string) ([]string, error) {
	atPath, err := buildWellKnownURL(baseURL, "oauth-protected-resource")
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	atRoot := parsed.Scheme + "://" + parsed.Host + "/.well-known/oauth-protected-resource"
	if atRoot == atPath {
		return []string{atPath}, nil
	}
	return []string{atPath, atRoot}, nil
}

// requestProtectedResource sends the GET for a protected resource metadata
// document; the caller closes the response body.
func (h *OAuthHandler) requestProtectedResource(ctx context.Context, metadataURL string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create protected resource request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	// Advertise the latest protocol version this client supports. Metadata
	// discovery happens before the MCP initialize handshake, so no negotiated
	// version is available yet; servers that don't recognise it can fall back
	// per the MCP spec.
	req.Header.Set("MCP-Protocol-Version", mcp.LATEST_PROTOCOL_VERSION)
	resp, err := h.discoveryClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send protected resource request: %w", err)
	}
	return resp, nil
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

	resp, err := h.discoveryClient.Do(req)
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

// isServerFailure reports whether status says the server failed to answer,
// rather than that what was asked for is not there.
func isServerFailure(status int) bool {
	return status >= http.StatusInternalServerError || status == http.StatusTooManyRequests
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
