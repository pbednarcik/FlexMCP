package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// On protocol version 2026-07-28 a server answers a method it does not
// implement with 404 and a JSON-RPC -32601 body. The body is what tells it
// apart from a legacy server's 404, and it is what the caller gets.
func TestClient_UnknownMethodOnTheModernProtocolIsMethodNotFound(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&message)
		w.Header().Set("Content-Type", "application/json")
		switch message.Method {
		case string(mcp.MethodServerDiscover):
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"supportedVersions":[%q],"capabilities":{},"serverInfo":{"name":"s","version":"1"}}}`, message.ID, mcp.ProtocolVersion20260728)
		case string(mcp.MethodToolsList):
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[]}}`, message.ID)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}`, message.ID)
		}
	}))
	defer httpServer.Close()

	trans, err := transport.NewStreamableHTTP(httpServer.URL)
	require.NoError(t, err)
	c := NewClient(trans)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Start(t.Context()))
	result, err := c.Initialize(t.Context(), mcp.InitializeRequest{})
	require.NoError(t, err)
	require.Equal(t, mcp.ProtocolVersion20260728, result.ProtocolVersion)

	_, err = c.Complete(t.Context(), mcp.CompleteRequest{})
	assert.ErrorIs(t, err, mcp.ErrMethodNotFound)

	_, err = c.ListTools(t.Context(), mcp.ListToolsRequest{})
	assert.NoError(t, err, "nothing ended")
}
