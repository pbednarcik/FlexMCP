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
// followed by name=value parameters (values tokens or quoted-strings, with
// backslash escapes); a token not followed by '=' starts the next challenge.
// A parameter whose value is malformed, as a quoted-string with no closing
// quote, is dropped rather than read in part, and a token68 credential is
// skipped.
func parseChallenges(headers []string) []challenge {
	var challenges []challenge
	for _, header := range headers {
		i := 0
		for i < len(header) {
			if !isAuthTokenChar(header[i]) {
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
				challenges = append(challenges, challenge{scheme: name, params: make(map[string]string)})
				i = j
				continue
			}
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

// isBearerScheme reports whether a challenge's scheme is one the client acts
// on: Bearer, or DPoP, which carries the same parameters (RFC 9449).
func isBearerScheme(scheme string) bool {
	return strings.EqualFold(scheme, "Bearer") || strings.EqualFold(scheme, "DPoP")
}

// parseBearerChallenge reads the first Bearer challenge among the
// WWW-Authenticate header values, wherever on its line it stands, and takes
// every parameter from that one challenge.
func parseBearerChallenge(headers []string) bearerChallenge {
	for _, c := range parseChallenges(headers) {
		if !isBearerScheme(c.scheme) {
			continue
		}
		return bearerChallenge{
			resourceMetadata: c.params["resource_metadata"],
			scopes:           strings.Fields(c.params["scope"]),
			errorCode:        c.params["error"],
			errorDescription: c.params["error_description"],
		}
	}
	return bearerChallenge{}
}

// insufficientScope reports whether resp is a 403 whose challenge says the
// token lacks the scopes the operation needs, which the client answers by
// stepping up its authorization rather than by failing.
func insufficientScope(resp *http.Response) bool {
	return resp.StatusCode == http.StatusForbidden &&
		parseBearerChallenge(resp.Header.Values("WWW-Authenticate")).errorCode == "insufficient_scope"
}

// authorizationError builds the error for a 401, or a 403 with an
// insufficient_scope challenge, carrying what the challenge said. With an
// OAuth handler the challenge is fed to it first, so the next authorization
// uses the advertised metadata URL and the challenged scopes, and the error
// carries the handler.
func authorizationError(resp *http.Response, handler *OAuthHandler) error {
	challenge := parseBearerChallenge(resp.Header.Values("WWW-Authenticate"))
	base := AuthorizationRequiredError{
		ResourceMetadataURL: challenge.resourceMetadata,
		Scopes:              challenge.scopes,
		ErrorCode:           challenge.errorCode,
		ErrorDescription:    challenge.errorDescription,
	}
	if handler == nil {
		return &base
	}
	handler.HandleUnauthorizedResponse(resp)
	return &OAuthAuthorizationRequiredError{Handler: handler, AuthorizationRequiredError: base}
}
