package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

// The metadata route judges a request's Origin as the endpoint does, through
// ServeHTTP and through Start's mux alike: with a CORS allow list an origin
// outside it is refused, an allowed one gets the list's headers, and only a
// server with no list answers every origin with Access-Control-Allow-Origin: *.
func TestMetadataRouteKeepsTheServersCORS(t *testing.T) {
	const host = "127.0.0.1:8080"
	cfg := ProtectedResourceMetadataConfig{Resource: "http://" + host + "/mcp", AuthorizationServers: []string{"https://auth.example.com"}}
	path := ProtectedResourceMetadataPath(cfg.Resource)
	type answer struct {
		code        int
		allowOrigin string
		credentials string
		methods     string
		metadata    bool
	}
	request := func(h http.Handler, method, origin string, preflight bool) answer {
		req := httptest.NewRequest(method, "http://"+host+path, nil)
		req.Host = host
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if preflight {
			req.Header.Set("Access-Control-Request-Method", http.MethodGet)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return answer{
			code:        rec.Code,
			allowOrigin: rec.Header().Get("Access-Control-Allow-Origin"),
			credentials: rec.Header().Get("Access-Control-Allow-Credentials"),
			methods:     rec.Header().Get("Access-Control-Allow-Methods"),
			metadata:    rec.Header().Get("Content-Type") == "application/json",
		}
	}
	routes := func(s *StreamableHTTPServer) map[string]http.Handler {
		return map[string]http.Handler{"ServeHTTP": s, "Start": s.startMux()}
	}

	listed := NewStreamableHTTPServer(NewMCPServer("test", "1.0.0"), WithProtectedResourceMetadata(cfg),
		WithStreamableHTTPCORS(WithCORSAllowedOrigins("https://good.example"), WithCORSAllowCredentials()))
	for name, h := range routes(listed) {
		t.Run(name+" with an allow list", func(t *testing.T) {
			assert.Equal(t, answer{code: http.StatusForbidden}, request(h, http.MethodGet, "https://evil.example", false),
				"an origin outside the list")
			assert.Equal(t, answer{code: http.StatusOK, allowOrigin: "https://good.example", credentials: "true", metadata: true},
				request(h, http.MethodGet, "https://good.example", false), "an origin on the list")
			assert.Equal(t, answer{code: http.StatusOK, metadata: true}, request(h, http.MethodGet, "", false),
				"no Origin, a client that is not a browser")
			assert.Equal(t, answer{code: http.StatusNoContent, allowOrigin: "https://good.example", credentials: "true", methods: "GET, POST, DELETE, OPTIONS"},
				request(h, http.MethodOptions, "https://good.example", true), "a preflight from an origin on the list")
			assert.Equal(t, answer{code: http.StatusForbidden}, request(h, http.MethodOptions, "https://evil.example", true),
				"a preflight from an origin outside the list")
		})
	}

	// On Start's route the checks come before the path match: ServeMux also
	// routes a real slash where the metadata path has an escaped one, and an
	// origin outside the list is refused there, not told the path is wrong.
	escaped := cfg
	escaped.Resource = "http://" + host + "/%2F"
	nearMiss := NewStreamableHTTPServer(NewMCPServer("test", "1.0.0"), WithProtectedResourceMetadata(escaped),
		WithStreamableHTTPCORS(WithCORSAllowedOrigins("https://good.example")))
	req := httptest.NewRequest(http.MethodGet, "http://"+host+"/.well-known/oauth-protected-resource/", nil)
	req.Host = host
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	nearMiss.startMux().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code, "Start's route, a near-miss path, an origin outside the list")

	open := NewStreamableHTTPServer(NewMCPServer("test", "1.0.0"), WithProtectedResourceMetadata(cfg))
	for name, h := range routes(open) {
		t.Run(name+" with no allow list", func(t *testing.T) {
			assert.Equal(t, answer{code: http.StatusOK, allowOrigin: "*", methods: "GET, HEAD, OPTIONS", metadata: true},
				request(h, http.MethodGet, "", false), "no Origin")
			assert.Equal(t, answer{code: http.StatusNoContent, allowOrigin: "*", methods: "GET, HEAD, OPTIONS"},
				request(h, http.MethodOptions, "", true), "a preflight")
		})
	}
}
