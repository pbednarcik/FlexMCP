package client

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/client/transport"
)

// The OAuth constructors add their option to a copy of the caller's options:
// a slice with spare capacity is not written into.
func TestTheOAuthConstructorsLeaveTheCallersOptionsAlone(t *testing.T) {
	t.Run("streamable HTTP", func(t *testing.T) {
		options := make([]transport.StreamableHTTPCOption, 1, 4)
		options[0] = transport.WithHTTPTimeout(0)
		_, err := NewOAuthStreamableHttpClient("http://127.0.0.1:1/mcp", OAuthConfig{}, options...)
		require.NoError(t, err)
		assert.Nil(t, options[:cap(options)][1], "the spare slot of the caller's slice is untouched")
	})
	t.Run("SSE", func(t *testing.T) {
		options := make([]transport.ClientOption, 1, 4)
		options[0] = transport.WithHeaders(nil)
		_, err := NewOAuthSSEClient("http://127.0.0.1:1/sse", OAuthConfig{}, options...)
		require.NoError(t, err)
		assert.Nil(t, options[:cap(options)][1], "the spare slot of the caller's slice is untouched")
	})
}
