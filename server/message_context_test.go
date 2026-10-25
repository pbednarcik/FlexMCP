package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type callerKey struct{}

// The context a message is handled in holds the server from the start and the
// protocol info once it is read: an initialization hook sees the server and
// no protocol info, and still none after the request, since the context it
// was given is never changed; the handler sees both, and its caller's values.
func TestMessageContextLayers(t *testing.T) {
	var hookCtx context.Context
	hooks := &Hooks{}
	hooks.AddOnRequestInitialization(func(ctx context.Context, _ any, _ any) error {
		hookCtx = ctx
		return nil
	})
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithHooks(hooks))
	var handlerCtx context.Context
	srv.AddTool(mcp.NewTool("echo"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		handlerCtx = ctx
		return mcp.NewToolResultText("ok"), nil
	})

	ctx := context.WithValue(t.Context(), callerKey{}, "caller")
	message := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo",` + readOnceMeta + `}}`)
	require.NotNil(t, srv.HandleMessage(ctx, message))

	require.NotNil(t, hookCtx)
	assert.Same(t, srv, ServerFromContext(hookCtx))
	assert.Nil(t, RequestProtocolInfoFromContext(hookCtx), "a hook runs before the protocol info is read")

	require.NotNil(t, handlerCtx)
	assert.Same(t, srv, ServerFromContext(handlerCtx))
	info := RequestProtocolInfoFromContext(handlerCtx)
	require.NotNil(t, info)
	assert.True(t, info.Modern)
	assert.Equal(t, mcp.ProtocolVersion20260728, info.ProtocolVersion)
	assert.Equal(t, "caller", handlerCtx.Value(callerKey{}))
}
