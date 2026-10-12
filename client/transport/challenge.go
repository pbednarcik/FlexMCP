package transport

import (
	"net/http"
	"strings"
)

// challenge is one authentication challenge of a WWW-Authenticate header
// (RFC 9110 §11.6.1): a scheme and its parameters, names in lower case
// since they compare case-insensitively (§11.2).
type challenge struct {
	scheme string
	params map[string]string
}

// parseChallenges splits WWW-Authenticate header values into challenges, in
// order. A header may carry several, comma-separated, each a scheme token
// followed by a token68 credential or by name=value parameters (values
// tokens or quoted-strings, with backslash escapes). Only a parameter
// carries '=' (RFC 9110 §11.2), so a bare token right after the scheme is
// that challenge's token68, padded or not, and a bare token anywhere else
// starts the next challenge. A token68 is skipped, as no challenge the
// client acts on carries one; a parameter whose value is malformed, as a
// quoted-string with no closing quote, is dropped rather than read in part.
func parseChallenges(headers []string) []challenge {
	var challenges []challenge
	for _, header := range headers {
		i := 0
		// afterScheme: the last item read was a scheme name, with no ','
		// since, so a bare token here is its token68.
		afterScheme := false
		for i < len(header) {
			if !isAuthTokenChar(header[i]) {
				if header[i] == ',' {
					afterScheme = false
				}
				i++
				continue
			}
			start := i
			for i < len(header) && isAuthTokenChar(header[i]) {
				i++
			}
			name := header[start:i]
			j := i
			for j < len(header) && (header[j] == ' ' || header[j] == '\t') {
				j++
			}
			if j >= len(header) || header[j] != '=' {
				if afterScheme {
					afterScheme = false // the challenge's token68, skipped
				} else {
					challenges = append(challenges, challenge{scheme: name, params: make(map[string]string)})
					afterScheme = true
				}
				i = j
				continue
			}
			afterScheme = false
			k := j + 1
			for k < len(header) && (header[k] == ' ' || header[k] == '\t') {
				k++
			}
			if k >= len(header) || header[k] == '=' || header[k] == ',' {
				// A token68 credential ("abc=="), which no challenge the client
				// acts on carries: skip it.
				for k < len(header) && header[k] == '=' {
					k++
				}
				i = k
				continue
			}
			value, next, ok := parseAuthParamValue(header, k)
			i = next
			if ok && len(challenges) > 0 {
				challenges[len(challenges)-1].params[strings.ToLower(name)] = value
			}
		}
	}
	return challenges
}

// bearerChallenge is what the Bearer challenge of a WWW-Authenticate header
// tells a client: the resource_metadata URL (RFC 9728 §5.1), the scopes the
// operation needs (RFC 6750 §3), and on a 403 the error, insufficient_scope,
// that says a token was presented but does not cover them.
type bearerChallenge struct {
	resourceMetadata string
	scopes           []string
	errorCode        string
	errorDescription string
}

// bearerChallenges returns the challenges the client can act on, in the
// order to try them: the Bearer ones as they stood, wherever on their lines,
// then the DPoP ones, which carry the same parameters (RFC 9449). A resource
// server supporting both sends both (RFC 9449 §7.2); this client presents
// Bearer tokens, so the Bearer challenge is the one that counts when there
// is one.
func bearerChallenges(headers []string) []bearerChallenge {
	var bearer, dpop []bearerChallenge
	for _, c := range parseChallenges(headers) {
		var into *[]bearerChallenge
		switch {
		case strings.EqualFold(c.scheme, "Bearer"):
			into = &bearer
		case strings.EqualFold(c.scheme, "DPoP"):
			into = &dpop
		default:
			continue
		}
		*into = append(*into, bearerChallenge{
			resourceMetadata: c.params["resource_metadata"],
			scopes:           strings.Fields(c.params["scope"]),
			errorCode:        c.params["error"],
			errorDescription: c.params["error_description"],
		})
	}
	return append(bearer, dpop...)
}

// actedOnChallenge returns the challenge of resp the client acts on, the
// one selection the transports' errors, the 403 classification and the OAuth
// handler all use: the first of bearerChallenges that is usable. On a 403
// only an insufficient_scope challenge is (RFC 6750 §3.1: the token is
// valid but does not cover the operation). With a handler, a challenge
// whose resource_metadata fails the handler's origin validation is not, and
// is passed over whole, so a compromised or misconfigured resource cannot
// send discovery to an attacker's metadata endpoint.
func actedOnChallenge(resp *http.Response, handler *OAuthHandler) (bearerChallenge, bool) {
	for _, c := range bearerChallenges(resp.Header.Values("WWW-Authenticate")) {
		if resp.StatusCode == http.StatusForbidden && c.errorCode != "insufficient_scope" {
			continue
		}
		if handler != nil && c.resourceMetadata != "" && handler.validateAdvertisedPRMURL(c.resourceMetadata) != nil {
			continue
		}
		return c, true
	}
	return bearerChallenge{}, false
}

// insufficientScope reports whether resp is a 403 with a usable challenge
// saying the token lacks the scopes the operation needs (see
// actedOnChallenge), which the client answers by stepping up its
// authorization rather than by failing.
func insufficientScope(resp *http.Response, handler *OAuthHandler) bool {
	if resp.StatusCode != http.StatusForbidden {
		return false
	}
	_, ok := actedOnChallenge(resp, handler)
	return ok
}

// authorizationError builds the error for a 401, or a 403 with an
// insufficient_scope challenge, carrying what the challenge acted on said
// (see actedOnChallenge). With an OAuth handler the same challenge is
// recorded by it first, so the next authorization uses its metadata URL and
// scopes, and the error carries the handler.
func authorizationError(resp *http.Response, handler *OAuthHandler) error {
	challenge, _ := actedOnChallenge(resp, handler)
	base := AuthorizationRequiredError{
		ResourceMetadataURL: challenge.resourceMetadata,
		Scopes:              challenge.scopes,
		ErrorCode:           challenge.errorCode,
		ErrorDescription:    challenge.errorDescription,
	}
	if handler == nil {
		return &base
	}
	handler.recordChallenge(challenge)
	return &OAuthAuthorizationRequiredError{Handler: handler, AuthorizationRequiredError: base}
}
