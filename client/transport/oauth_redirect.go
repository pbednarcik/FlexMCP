package transport

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// newRedirectPolicies returns two copies of client that share its transport,
// leaving client itself as the application configured it. The HTTPS rule for
// authorization servers holds across redirects: a discovery request follows a
// redirect as client's own policy says, and only while the URL that policy
// leaves stays on https; a request carrying client credentials, a code or a
// refresh token follows none, so a 307 cannot resend its body elsewhere, and
// the redirect response is its failure.
func newRedirectPolicies(client *http.Client) (discovery, credential *http.Client) {
	discoveryCopy, credentialCopy := *client, *client
	follow := client.CheckRedirect
	discoveryCopy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if follow != nil {
			if err := follow(req, via); err != nil {
				return err
			}
		} else if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if strings.EqualFold(via[0].URL.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
			return fmt.Errorf("%w: redirect from %q to %q", ErrInsecureAuthorizationServer, via[len(via)-1].URL, req.URL)
		}
		return nil
	}
	credentialCopy.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &discoveryCopy, &credentialCopy
}

// originBoundClient returns a copy of client, sharing its transport, for a
// transport's requests, which carry its headers and, with OAuth, an access
// token: a redirect is followed, as client's own policy says, only while it
// stays on the request's origin, so neither reaches another server or a
// downgraded connection (MCP authorization, Token Audience Binding). The
// policy runs first, so a policy that rewrites the next request is checked
// on the URL actually followed.
func originBoundClient(client *http.Client) *http.Client {
	bound := *client
	follow := client.CheckRedirect
	bound.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if follow != nil {
			if err := follow(req, via); err != nil {
				return err
			}
		} else if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if !sameOrigin(via[0].URL, req.URL) {
			return fmt.Errorf("%w: redirect from %q to %q", ErrRedirectLeavesOrigin, via[len(via)-1].URL, req.URL)
		}
		return nil
	}
	return &bound
}

// sameOrigin reports whether a and b share scheme, host and port, a missing
// port being the scheme's default.
func sameOrigin(a, b *url.URL) bool {
	return equalFoldASCII(a.Scheme, b.Scheme) &&
		equalHostASCII(a.Hostname(), b.Hostname()) &&
		originPort(a) == originPort(b)
}

// equalFoldASCII reports whether a and b are equal once ASCII letters are
// lowered, every other byte matching exactly. Schemes and host names compare
// this way (RFC 3986 sections 3.1 and 3.2.2): Unicode case folding, as in
// strings.EqualFold, takes some distinct hosts for one, such as names with
// a final and a medial sigma, and url.Parse maps no host through IDNA.
func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if lowerASCII(a[i]) != lowerASCII(b[i]) {
			return false
		}
	}
	return true
}

// equalHostASCII reports whether hosts a and b, as a URL's Host or Hostname
// holds them, are equal: by ASCII case up to an IPv6 zone's '%', which
// url.Parse has unescaped from %25, and byte for byte from there, port
// included. A zone names an interface, and interface names can differ by
// case alone (RFC 6874). Any other '%' in a host compares exactly too.
func equalHostASCII(a, b string) bool {
	za, zb := strings.IndexByte(a, '%'), strings.IndexByte(b, '%')
	if za < 0 {
		za = len(a)
	}
	if zb < 0 {
		zb = len(b)
	}
	return equalFoldASCII(a[:za], b[:zb]) && a[za:] == b[zb:]
}

// lowerASCII is c with A to Z lowered, and any other byte as it is.
func lowerASCII(c byte) byte {
	if 'A' <= c && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// originPort is u's port, or its scheme's default when it names none.
func originPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}
