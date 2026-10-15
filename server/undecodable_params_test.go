package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// A request whose params cannot be decoded is answered invalid params with
// a fixed text naming the method. The decoder's own text names Go types and
// fields, so it goes to the hooks and the log, not to the client.
func TestUndecodableParamsAreAnsweredWithAFixedText(t *testing.T) {
	var seen error
	hooks := &Hooks{}
	hooks.AddOnError(func(_ context.Context, _ any, _ mcp.MCPMethod, _ any, err error) { seen = err })
	s := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithHooks(hooks))

	response := s.HandleMessage(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":5}}`))
	errResp, ok := response.(mcp.JSONRPCError)
	require.True(t, ok, "got %T", response)
	require.Equal(t, mcp.INVALID_PARAMS, errResp.Error.Code)
	require.Equal(t, "invalid params for tools/call", errResp.Error.Message)
	require.ErrorContains(t, seen, "cannot unmarshal", "the hooks lost the decoder's text")
}
