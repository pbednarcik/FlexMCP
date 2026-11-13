package transport

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// WithHTTPTimeout goes on the transport's own copy of its client, whichever
// option comes first: a client given with WithHTTPBasicClient, which the
// caller may share, is never written into.
func TestWithHTTPTimeoutLeavesTheCallersClientAlone(t *testing.T) {
	const timeout = 5 * time.Second

	t.Run("a caller's client, then the timeout", func(t *testing.T) {
		errPolicy := errors.New("the caller's redirect policy")
		roundTripper := &http.Transport{}
		shared := &http.Client{
			Transport:     roundTripper,
			CheckRedirect: func(*http.Request, []*http.Request) error { return errPolicy },
		}
		trans, err := NewStreamableHTTP("http://mcp.example/mcp",
			WithHTTPBasicClient(shared), WithHTTPTimeout(timeout))
		require.NoError(t, err)

		require.Zero(t, shared.Timeout, "the caller's shared client now has Timeout %v", shared.Timeout)
		assert.Equal(t, timeout, trans.httpClient.Timeout, "the transport's client")
		assert.Same(t, roundTripper, trans.httpClient.Transport, "the copy shares the caller's transport")
		require.NotNil(t, trans.httpClient.CheckRedirect)
		assert.ErrorIs(t, trans.httpClient.CheckRedirect(nil, nil), errPolicy,
			"the copy keeps the caller's redirect policy")
	})
	t.Run("the timeout, then a caller's client", func(t *testing.T) {
		shared := &http.Client{}
		trans, err := NewStreamableHTTP("http://mcp.example/mcp",
			WithHTTPTimeout(timeout), WithHTTPBasicClient(shared))
		require.NoError(t, err)

		assert.Zero(t, shared.Timeout, "the caller's shared client now has Timeout %v", shared.Timeout)
		assert.Equal(t, timeout, trans.httpClient.Timeout, "the timeout given first was lost")
	})
	t.Run("the default client keeps its binding", func(t *testing.T) {
		trans, err := NewStreamableHTTP("http://mcp.example/mcp", WithHTTPTimeout(timeout))
		require.NoError(t, err)

		assert.Equal(t, timeout, trans.httpClient.Timeout)
		assert.NotNil(t, trans.httpClient.CheckRedirect, "the default client is no longer origin-bound")
	})
	t.Run("with OAuth, the bound copy of a caller's client", func(t *testing.T) {
		shared := &http.Client{}
		trans, err := NewStreamableHTTP("http://mcp.example/mcp",
			WithHTTPBasicClient(shared), WithHTTPOAuth(OAuthConfig{}), WithHTTPTimeout(timeout))
		require.NoError(t, err)

		assert.Zero(t, shared.Timeout, "the caller's shared client now has Timeout %v", shared.Timeout)
		assert.Nil(t, shared.CheckRedirect, "the caller's shared client was bound in place")
		assert.Equal(t, timeout, trans.httpClient.Timeout)
		assert.NotNil(t, trans.httpClient.CheckRedirect, "the caller's client is not origin-bound under OAuth")
	})
	t.Run("a zero timeout replaces the client's own", func(t *testing.T) {
		shared := &http.Client{Timeout: 3 * time.Second}
		trans, err := NewStreamableHTTP("http://mcp.example/mcp",
			WithHTTPBasicClient(shared), WithHTTPTimeout(0))
		require.NoError(t, err)

		assert.Equal(t, 3*time.Second, shared.Timeout, "the caller's shared client was written into")
		assert.Zero(t, trans.httpClient.Timeout, "WithHTTPTimeout(0) did not remove the client's timeout")
	})
}
