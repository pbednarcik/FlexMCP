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
// transport whose requests carry an access token: a redirect is followed, as
// client's own policy says, only while it stays on the request's origin, so
// the token never reaches another server or a downgraded connection (MCP
// authorization, Token Audience Binding). The policy runs first, so a policy
// that rewrites the next request is checked on the URL actually followed.
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
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		originPort(a) == originPort(b)
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
