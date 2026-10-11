package transport

import (
	"net/http"
	"strings"
)

// bearerChallenge is what the Bearer challenge of a WWW-Authenticate header
// tells a client (RFC 6750 §3): the scopes the operation needs, and on a
// 403 the error, insufficient_scope, that says a token was presented but
// does not cover them.
type bearerChallenge struct {
	scopes           []string
	errorCode        string
	errorDescription string
}

// parseBearerChallenge reads the first Bearer challenge among the
// WWW-Authenticate header values. The resource_metadata parameter is read
// separately, by extractResourceMetadataURL, which looks through every
// challenge of every header.
func parseBearerChallenge(headers []string) bearerChallenge {
	for _, header := range headers {
		scheme, _, _ := strings.Cut(strings.TrimSpace(header), " ")
		if !strings.EqualFold(scheme, "Bearer") {
			continue
		}
		params := parseAuthParams(header)
		return bearerChallenge{
			scopes:           strings.Fields(params["scope"]),
			errorCode:        params["error"],
			errorDescription: params["error_description"],
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
	headers := resp.Header.Values("WWW-Authenticate")
	challenge := parseBearerChallenge(headers)
	base := AuthorizationRequiredError{
		ResourceMetadataURL: extractResourceMetadataURL(headers),
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
