package transport

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// checkAdvertisedPRMURL is validateAdvertisedPRMURL against baseURL.
func checkAdvertisedPRMURL(candidate, baseURL string) error {
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
	// Identical identifiers are equal however they parse; the common case on
	// every request skips the two parses.
	if a == b {
		return true
	}
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
	if resource == addressed {
		return true
	}
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

// isHTTPS reports whether rawURL is an https URL.
func isHTTPS(rawURL string) bool {
	u, err := url.Parse(rawURL)
	return err == nil && strings.EqualFold(u.Scheme, "https")
}

// requireIssuer refuses authorization server metadata that names no issuer,
// which RFC 8414 §2 requires: client credentials and tokens are bound to the
// issuer, so without one they would be bound to nothing.
func requireIssuer(m *AuthServerMetadata) error {
	if m.Issuer == "" {
		return errors.New("authorization server metadata names no issuer")
	}
	return nil
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
