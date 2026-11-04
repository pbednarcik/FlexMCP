package server

import (
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
