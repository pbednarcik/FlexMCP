package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMetadataPathMatches(t *testing.T) {
	const metadataPath = "/.well-known/oauth-protected-resource/tenant/acme%2Fcorp/mcp"
	tests := []struct {
		name        string
		requestPath string
		want        bool
	}{
		{"same spelling", metadataPath, true},
		{"escape in lower case", "/.well-known/oauth-protected-resource/tenant/acme%2fcorp/mcp", true},
		{"needless escape", "/.well-known/oauth-protected-resource/%74enant/acme%2Fcorp/mcp", true},
		{"real slash", "/.well-known/oauth-protected-resource/tenant/acme/corp/mcp", false},
		{"one segment short", "/.well-known/oauth-protected-resource/tenant/acme%2Fcorp", false},
		{"one segment long", "/.well-known/oauth-protected-resource/tenant/acme%2Fcorp/mcp/x", false},
		{"trailing slash", "/.well-known/oauth-protected-resource/tenant/acme%2Fcorp/mcp/", false},
		{"bad escape", "/.well-known/oauth-protected-resource/tenant/acme%2Gcorp/mcp", false},
		{"the MCP endpoint", "/mcp", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, metadataPathMatches(tt.requestPath, metadataPath))
		})
	}
}

// ServeHTTP runs the match on every request while metadata is configured: a
// request for another path, escaped or not, costs no allocation.
func TestMetadataPathMatches_NoAllocationOnAMiss(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	const metadataPath = "/.well-known/oauth-protected-resource/tenant/acme/mcp"
	for _, requestPath := range []string{
		"/mcp",
		"/.well-known/oauth-protected-resource/tenant/other/mcp",
	} {
		allocs := testing.AllocsPerRun(100, func() {
			_ = metadataPathMatches(requestPath, metadataPath)
		})
		assert.Zero(t, allocs, requestPath)
	}
}

// Start's mux and ServeHTTP serve the metadata for the same request paths:
// ServeMux alone would also answer a path its own unescaping makes equal, such
// as a real trailing slash for an escaped one.
func TestStartServesTheMetadataPathAsServeHTTPDoes(t *testing.T) {
	const prefix = "/.well-known/oauth-protected-resource"
	tests := []struct {
		resource string
		paths    []string
	}{
		{"https://mcp.example.com/%2F", []string{prefix + "/%2F", prefix + "/%2f", prefix + "/", prefix + "//"}},
		{"https://mcp.example.com/tenant/acme%2Fcorp/mcp", []string{
			prefix + "/tenant/acme%2Fcorp/mcp", prefix + "/tenant/acme%2fcorp/mcp", prefix + "/tenant/acme/corp/mcp",
		}},
		{"https://mcp.example.com/mcp", []string{prefix + "/mcp", prefix + "/%6Dcp", prefix + "/mcp/"}},
	}
	for _, tt := range tests {
		s := NewStreamableHTTPServer(NewMCPServer("test", "1.0.0"),
			WithProtectedResourceMetadata(ProtectedResourceMetadataConfig{Resource: tt.resource}))
		mux := s.startMux()
		for _, path := range tt.paths {
			t.Run(tt.resource+" "+path, func(t *testing.T) {
				serves := func(h http.Handler) bool {
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://mcp.example.com"+path, nil))
					return rec.Code == http.StatusOK && rec.Header().Get("Content-Type") == "application/json"
				}
				assert.Equal(t, serves(s), serves(mux))
			})
		}
	}
}
