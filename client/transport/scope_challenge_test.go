package transport

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// challengingServer answers every POST with status and the WWW-Authenticate
// value given.
func challengingServer(t *testing.T, status int, challenge string) *httptest.Server {
	t.Helper()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if challenge != "" {
			w.Header().Set("WWW-Authenticate", challenge)
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	return server
}

// firstBearerChallenge is the first of bearerChallenges, or none.
func firstBearerChallenge(headers []string) bearerChallenge {
	if challenges := bearerChallenges(headers); len(challenges) > 0 {
		return challenges[0]
	}
	return bearerChallenge{}
}

func sendOn(t *testing.T, trans *StreamableHTTP) (request, notification error) {
	t.Helper()
	_, request = trans.SendRequest(t.Context(), JSONRPCRequest{
		JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Method: "tools/list",
	})
	notification = trans.SendNotification(t.Context(), mcp.JSONRPCNotification{
		JSONRPC: mcp.JSONRPC_VERSION, Method: "notifications/initialized",
	})
	return request, notification
}

// The scope parameter of a 401 challenge names the scopes the operation
// needs; the caller gets them with the error.
func TestStreamableHTTP_ChallengeScopesReachTheCaller(t *testing.T) {
	server := challengingServer(t, http.StatusUnauthorized, `Bearer resource_metadata="`+"http://example.com/.well-known/oauth-protected-resource"+`", scope="files:read files:list"`)
	trans, err := NewStreamableHTTP(server.URL)
	require.NoError(t, err)

	for _, err := range collect(sendOn(t, trans)) {
		var authErr *AuthorizationRequiredError
		require.ErrorAs(t, err, &authErr)
		assert.Equal(t, []string{"files:read", "files:list"}, authErr.Scopes)
		assert.Equal(t, "http://example.com/.well-known/oauth-protected-resource", authErr.ResourceMetadataURL)
		assert.Empty(t, authErr.ErrorCode)
	}
}

// A 403 whose challenge says insufficient_scope is an authorization error
// too, carrying the scopes to step up to; a 403 without one is not.
func TestStreamableHTTP_InsufficientScopeIsAnAuthorizationError(t *testing.T) {
	server := challengingServer(t, http.StatusForbidden, `Bearer error="insufficient_scope", scope="files:write", error_description="write access needed"`)
	trans, err := NewStreamableHTTP(server.URL)
	require.NoError(t, err)

	for _, err := range collect(sendOn(t, trans)) {
		require.ErrorIs(t, err, ErrAuthorizationRequired)
		var authErr *AuthorizationRequiredError
		require.ErrorAs(t, err, &authErr)
		assert.Equal(t, "insufficient_scope", authErr.ErrorCode)
		assert.Equal(t, "write access needed", authErr.ErrorDescription)
		assert.Equal(t, []string{"files:write"}, authErr.Scopes)
	}

	plain := challengingServer(t, http.StatusForbidden, "")
	trans, err = NewStreamableHTTP(plain.URL)
	require.NoError(t, err)
	for _, err := range collect(sendOn(t, trans)) {
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrAuthorizationRequired)
	}
}

// The legacy SSE transport reads the same challenges.
func TestSSE_InsufficientScopeIsAnAuthorizationError(t *testing.T) {
	server := challengingServer(t, http.StatusForbidden, `Bearer error="insufficient_scope", scope="files:write"`)
	trans, err := NewSSE(server.URL)
	require.NoError(t, err)
	endpoint, err := url.Parse(server.URL + "/messages")
	require.NoError(t, err)
	trans.endpoint = endpoint
	trans.started.Store(true)

	_, err = trans.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Method: "tools/list"})
	var authErr *AuthorizationRequiredError
	require.ErrorAs(t, err, &authErr)
	assert.Equal(t, []string{"files:write"}, authErr.Scopes)
	err = trans.SendNotification(t.Context(), mcp.JSONRPCNotification{JSONRPC: mcp.JSONRPC_VERSION, Method: "notifications/initialized"})
	require.ErrorAs(t, err, &authErr)
}

func collect(errs ...error) []error { return errs }

// Stepping up: the challenged scopes are authoritative for the operation and
// come first, the scopes configured and those the current token was granted
// are kept, nothing is listed twice.
func TestOAuthHandler_StepsUpWithTheChallengedScopes(t *testing.T) {
	authServer := &tokenAuthServer{codeChallengeMethods: []string{"S256"}}
	authHTTP := authServer.start(t)
	store := NewMemoryTokenStore()
	require.NoError(t, store.SaveToken(t.Context(), &Token{AccessToken: "old", TokenType: "Bearer", Scope: "mcp.read offline_access", ExpiresAt: time.Now().Add(time.Hour)}))
	mcpServer := challengingServer(t, http.StatusForbidden, `Bearer error="insufficient_scope", scope="files:write mcp.read"`)

	trans, err := NewStreamableHTTP(mcpServer.URL, WithHTTPOAuth(OAuthConfig{
		ClientID:              "client",
		RedirectURI:           "http://localhost:8085/callback",
		Scopes:                []string{"mcp.read", "mcp.write"},
		TokenStore:            store,
		AuthServerMetadataURL: authHTTP.URL + "/.well-known/oauth-authorization-server",
	}))
	require.NoError(t, err)
	request, _ := sendOn(t, trans)
	require.ErrorIs(t, request, ErrOAuthAuthorizationRequired)
	handler := trans.GetOAuthHandler()
	assert.Equal(t, []string{"files:write", "mcp.read"}, handler.ChallengedScopes())

	authURL, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
	require.NoError(t, err)
	parsed, err := url.Parse(authURL)
	require.NoError(t, err)
	assert.Equal(t, "files:write mcp.read mcp.write offline_access", parsed.Query().Get("scope"))
}

// With no challenge and nothing configured, the protected resource's
// scopes_supported are requested; with nothing anywhere, no scope at all.
func TestOAuthHandler_RequestsScopesSupportedWhenNothingElseSaysWhich(t *testing.T) {
	for name, supported := range map[string][]string{"declared": {"mcp.read", "mcp.write"}, "undeclared": nil} {
		t.Run(name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/.well-known/oauth-protected-resource":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"resource": server.URL, "authorization_servers": []string{server.URL}, "scopes_supported": supported,
					})
				case "/.well-known/oauth-authorization-server":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"issuer": server.URL, "authorization_endpoint": server.URL + "/authorize", "token_endpoint": server.URL + "/token",
						"code_challenge_methods_supported": []string{"S256"},
					})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)
			handler := NewOAuthHandler(OAuthConfig{ClientID: "client", RedirectURI: "http://localhost:8085/callback", TokenStore: NewMemoryTokenStore()})
			handler.SetBaseURL(server.URL)

			authURL, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
			require.NoError(t, err)
			parsed, err := url.Parse(authURL)
			require.NoError(t, err)
			if supported == nil {
				assert.False(t, parsed.Query().Has("scope"))
			} else {
				assert.Equal(t, "mcp.read mcp.write", parsed.Query().Get("scope"))
			}
		})
	}
}

// A header may carry several challenges on one line (RFC 9110 §11.6.1). The
// Bearer challenge is read wherever it stands, and scope, error and
// resource_metadata all come from that one challenge.
func TestStreamableHTTP_ReadsTheBearerChallengeAmongOthers(t *testing.T) {
	server := challengingServer(t, http.StatusForbidden, `Basic realm="legacy", Bearer error="insufficient_scope", scope="files:write", resource_metadata="http://example.com/.well-known/oauth-protected-resource"`)
	trans, err := NewStreamableHTTP(server.URL)
	require.NoError(t, err)

	for _, err := range collect(sendOn(t, trans)) {
		var authErr *AuthorizationRequiredError
		require.ErrorAs(t, err, &authErr)
		assert.Equal(t, "insufficient_scope", authErr.ErrorCode)
		assert.Equal(t, []string{"files:write"}, authErr.Scopes)
		assert.Equal(t, "http://example.com/.well-known/oauth-protected-resource", authErr.ResourceMetadataURL)
	}
}

// With two Bearer challenges, the first is the one acted on, whole: a later
// challenge's parameters do not replace its own.
func TestStreamableHTTP_TakesTheFirstBearerChallengeWhole(t *testing.T) {
	server := challengingServer(t, http.StatusUnauthorized, `Bearer realm="a", scope="x", resource_metadata="http://a.example/prm", Bearer realm="b", scope="y", resource_metadata="http://b.example/prm"`)
	trans, err := NewStreamableHTTP(server.URL)
	require.NoError(t, err)

	request, _ := sendOn(t, trans)
	var authErr *AuthorizationRequiredError
	require.ErrorAs(t, request, &authErr)
	assert.Equal(t, []string{"x"}, authErr.Scopes)
	assert.Equal(t, "http://a.example/prm", authErr.ResourceMetadataURL)
}

// A resource server that also supports DPoP sends both challenges (RFC 9449
// §7.2). This client presents Bearer tokens, so the Bearer challenge is the
// one acted on wherever it stands; a DPoP challenge counts only where there
// is no Bearer one.
func TestStreamableHTTP_PrefersTheBearerChallengeOverDPoP(t *testing.T) {
	server := challengingServer(t, http.StatusForbidden, `DPoP algs="ES256", Bearer error="insufficient_scope", scope="files:write"`)
	trans, err := NewStreamableHTTP(server.URL)
	require.NoError(t, err)

	for _, err := range collect(sendOn(t, trans)) {
		var authErr *AuthorizationRequiredError
		require.ErrorAs(t, err, &authErr, "the Bearer challenge's insufficient_scope is the one that counts")
		assert.Equal(t, "insufficient_scope", authErr.ErrorCode)
		assert.Equal(t, []string{"files:write"}, authErr.Scopes)
	}
	assert.Equal(t, []string{"a"}, firstBearerChallenge([]string{`DPoP scope="a"`}).scopes, "a DPoP challenge alone is still read")
}

// After the scheme comes a token68 or auth-params, and only an auth-param
// carries '=' (RFC 9110 §11.2): a bare token right after the scheme is that
// challenge's token68, padded or not, even one spelled like a scheme name.
func TestParseChallengesKeepsAToken68WithItsScheme(t *testing.T) {
	for _, header := range []string{
		`Negotiate abc, Bearer scope="x"`,
		`Negotiate abc==, Bearer scope="x"`,
		`Negotiate Bearer, Bearer scope="x"`,
		// token68 also allows '/' and '+' (RFC 9110 §11.2), which a scheme
		// name does not: the credential is read whole, not split at '/'.
		`Negotiate abc/Bearer, Bearer scope="x"`,
		`Negotiate a+b/c==, Bearer scope="x"`,
	} {
		challenges := parseChallenges([]string{header})
		require.Len(t, challenges, 2, header)
		assert.Equal(t, "Negotiate", challenges[0].scheme, header)
		assert.Empty(t, challenges[0].params, header)
		assert.Equal(t, "Bearer", challenges[1].scheme, header)
		assert.Equal(t, map[string]string{"scope": "x"}, challenges[1].params, header)
	}
}

// The handler acts on one challenge, whole: the first Bearer challenge it
// can use (one whose resource_metadata, if any, passes origin validation),
// taking the scopes and the metadata URL from that one. A later challenge's
// metadata does not join an earlier one's scopes.
func TestOAuthHandler_TakesOneChallengeWhole(t *testing.T) {
	handler := NewOAuthHandler(OAuthConfig{ClientID: "client", RedirectURI: "http://localhost:8085/callback"})
	handler.SetBaseURL("https://example.com/mcp")

	handler.HandleUnauthorizedResponse(&http.Response{Header: http.Header{"Www-Authenticate": []string{
		`Bearer scope="x"`,
		`Bearer scope="y", resource_metadata="https://example.com/mcp/prm"`,
	}}})
	assert.Equal(t, []string{"x"}, handler.ChallengedScopes())
	assert.Empty(t, handler.advertisedPRMURL, "the second challenge's metadata does not join the first's scopes")

	handler.HandleUnauthorizedResponse(&http.Response{Header: http.Header{"Www-Authenticate": []string{
		`Bearer scope="x", resource_metadata="https://attacker.example/prm"`,
		`Bearer scope="y", resource_metadata="https://example.com/mcp/prm"`,
	}}})
	assert.Equal(t, []string{"y"}, handler.ChallengedScopes(), "a challenge whose metadata fails validation is passed over whole")
	assert.Equal(t, "https://example.com/mcp/prm", handler.advertisedPRMURL)
}

// The transport and the handler act on the same challenge: the error the
// caller gets carries the scopes and metadata URL the handler recorded, and
// a challenge the handler passes over (its metadata URL fails origin
// validation) is passed over for the error too.
func TestStreamableHTTP_ErrorAndHandlerReadTheSameChallenge(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("WWW-Authenticate", `Bearer scope="a", resource_metadata="https://attacker.example/prm"`)
		w.Header().Add("WWW-Authenticate", `Bearer scope="b", resource_metadata="`+server.URL+`/prm"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(server.Close)
	store := NewMemoryTokenStore()
	require.NoError(t, store.SaveToken(t.Context(), &Token{AccessToken: "token", TokenType: "Bearer", ExpiresAt: time.Now().Add(time.Hour)}))
	trans, err := NewStreamableHTTP(server.URL, WithHTTPOAuth(OAuthConfig{ClientID: "client", TokenStore: store}))
	require.NoError(t, err)

	request, _ := sendOn(t, trans)
	var authErr *OAuthAuthorizationRequiredError
	require.ErrorAs(t, request, &authErr)
	assert.Equal(t, []string{"b"}, authErr.Scopes)
	assert.Equal(t, server.URL+"/prm", authErr.ResourceMetadataURL)
	assert.Equal(t, authErr.Scopes, authErr.Handler.ChallengedScopes())
}

// On a 403 the challenge that counts is the one saying insufficient_scope
// (RFC 6750 §3.1), wherever it stands: an earlier challenge with another
// error does not turn the step-up into an ordinary failure.
func TestStreamableHTTP_FindsTheInsufficientScopeChallengeOnA403(t *testing.T) {
	server := challengingServer(t, http.StatusForbidden, `Bearer error="invalid_token", Bearer error="insufficient_scope", scope="files:write"`)
	trans, err := NewStreamableHTTP(server.URL)
	require.NoError(t, err)

	for _, err := range collect(sendOn(t, trans)) {
		var authErr *AuthorizationRequiredError
		require.ErrorAs(t, err, &authErr)
		assert.Equal(t, "insufficient_scope", authErr.ErrorCode)
		assert.Equal(t, []string{"files:write"}, authErr.Scopes)
	}
}

// The challenged scopes are the refused operation's (MCP authorization:
// authoritative for the operation), not a standing grant: each challenge
// replaces them, one without a scope too, and a change of server clears
// them. Scopes kept from a stored token come only from a token for this
// server.
func TestOAuthHandler_ChallengedScopesBelongToTheOperation(t *testing.T) {
	as := (&tokenAuthServer{codeChallengeMethods: []string{"S256"}}).start(t)
	store := NewMemoryTokenStore()
	handler := NewOAuthHandler(OAuthConfig{
		ClientID:              "client",
		RedirectURI:           "http://localhost:8085/callback",
		TokenStore:            store,
		AuthServerMetadataURL: as.URL + "/.well-known/oauth-authorization-server",
	})
	handler.SetBaseURL(as.URL + "/a")
	challenge := func(value string) {
		handler.HandleUnauthorizedResponse(&http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Www-Authenticate": []string{value}}})
	}
	requested := func() string {
		authURL, err := handler.GetAuthorizationURL(t.Context(), "state", "challenge")
		require.NoError(t, err)
		parsed, err := url.Parse(authURL)
		require.NoError(t, err)
		return parsed.Query().Get("scope")
	}

	challenge(`Bearer scope="admin"`)
	assert.Equal(t, "admin", requested())
	challenge(`Bearer error="invalid_token"`)
	assert.Empty(t, handler.ChallengedScopes(), "a challenge without a scope replaces the last one's")
	assert.Empty(t, requested())

	challenge(`Bearer scope="admin"`)
	handler.SetBaseURL(as.URL + "/b")
	assert.Empty(t, handler.ChallengedScopes(), "server a's challenge does not carry over to server b")
	assert.Empty(t, requested())

	require.NoError(t, store.SaveToken(t.Context(), &Token{AccessToken: "a", Scope: "admin", Resource: as.URL + "/a", Issuer: as.URL}))
	assert.Empty(t, requested(), "server a's token's scopes are not asked for server b")
}

func TestParseChallenges(t *testing.T) {
	challenges := parseChallenges([]string{
		`Basic realm="legacy, old", Bearer Scope="a b", error=invalid_token, realm="say \"hi\""`,
		`Digest abc==, DPoP algs="ES256"`,
		`Bearer resource_metadata="https://truncated`,
	})
	require.Len(t, challenges, 5)
	assert.Equal(t, "Basic", challenges[0].scheme)
	assert.Equal(t, map[string]string{"realm": "legacy, old"}, challenges[0].params)
	assert.Equal(t, "Bearer", challenges[1].scheme)
	assert.Equal(t, map[string]string{"scope": "a b", "error": "invalid_token", "realm": `say "hi"`}, challenges[1].params)
	assert.Equal(t, "Digest", challenges[2].scheme)
	assert.Empty(t, challenges[2].params, "a token68 credential is no parameter")
	assert.Equal(t, "DPoP", challenges[3].scheme)
	assert.Equal(t, map[string]string{"algs": "ES256"}, challenges[3].params)
	assert.Empty(t, challenges[4].params, "a truncated quoted-string is dropped, not read in part")
	assert.Empty(t, parseChallenges([]string{"", "   "}))
}

// A challenge is the server's that sent it: one that arrives after the
// transport's handler was pointed at another server is not recorded for
// that server's authorization.
func TestStreamableHTTP_AChallengeFromThePreviousServerIsNotRecorded(t *testing.T) {
	var b *httptest.Server
	b = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                           b.URL,
			"authorization_endpoint":           b.URL + "/authorize",
			"token_endpoint":                   b.URL + "/token",
			"code_challenge_methods_supported": []string{"S256"},
		})
	}))
	t.Cleanup(b.Close)
	var tr *StreamableHTTP
	a := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tr.GetOAuthHandler().SetBaseURL(b.URL) // retargeted while A answers
		w.Header().Set("WWW-Authenticate", `Bearer scope="a-only:admin"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(a.Close)
	store := NewMemoryTokenStore()
	require.NoError(t, store.SaveToken(t.Context(), &Token{AccessToken: "app-token", ExpiresAt: time.Now().Add(time.Hour)}))
	tr, err := NewStreamableHTTP(a.URL, WithHTTPOAuth(OAuthConfig{
		ClientID:              "client",
		RedirectURI:           "http://localhost:8085/callback",
		TokenStore:            store,
		AuthServerMetadataURL: b.URL + "/.well-known/oauth-authorization-server",
	}))
	require.NoError(t, err)

	_, err = tr.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Method: "tools/list"})
	require.Error(t, err)
	assert.Empty(t, tr.GetOAuthHandler().ChallengedScopes(), "server a's challenge was recorded for server b")
}

// The advertised metadata URL is checked against the server the handler
// targets when it is recorded, not only when the challenge was read: a
// challenge read for one server and recorded after a retarget does not send
// the next server's discovery to the previous server's metadata.
func TestOAuthHandler_AnAdvertisedMetadataURLIsCheckedWhenRecorded(t *testing.T) {
	handler := NewOAuthHandler(OAuthConfig{ClientID: "client", TokenStore: NewMemoryTokenStore()})
	handler.SetBaseURL("https://b.example/mcp")

	handler.recordChallenge(bearerChallenge{resourceMetadata: "https://a.example/.well-known/oauth-protected-resource"}, "")
	handler.metadataMu.Lock()
	defer handler.metadataMu.Unlock()
	assert.Empty(t, handler.advertisedPRMURL, "server a's metadata URL was recorded for server b")
}
