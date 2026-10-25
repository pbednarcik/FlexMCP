package server

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// Decoration never writes into the handler's result: one a handler shares
// across calls stays as the handler returned it, so concurrent modern calls
// do not race on it and a legacy client is not served modern fields.
func TestDecorateResponseLeavesTheHandlerResultAlone(t *testing.T) {
	s := NewMCPServer("test", "1.0.0")
	info := &RequestProtocolInfo{Modern: true, ProtocolVersion: mcp.ProtocolVersion20260728}

	t.Run("a pointer result", func(t *testing.T) {
		shared := &mcp.CallToolResult{
			Meta:    &mcp.Meta{AdditionalFields: map[string]any{"kept": true}},
			Content: []mcp.Content{mcp.NewTextContent("ok")},
		}
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				resp := s.decorateResponse(t.Context(), info, mcp.MethodToolsCall, mcp.JSONRPCResponse{Result: shared})
				decorated, ok := resp.(mcp.JSONRPCResponse).Result.(*mcp.CallToolResult)
				require.True(t, ok)
				assert.Equal(t, mcp.ResultTypeComplete, decorated.ResultType)
				assert.NotNil(t, decorated.Meta.ServerInfo())
				assert.Equal(t, true, decorated.Meta.AdditionalFields["kept"])
				assert.Equal(t, "ok", decorated.Content[0].(mcp.TextContent).Text)
			})
		}
		wg.Wait()
		assert.Empty(t, shared.ResultType, "decoration wrote into the handler's result")
		assert.Nil(t, shared.Meta.ServerInfo(), "decoration wrote into the handler's _meta")
		assert.Len(t, shared.Meta.AdditionalFields, 1)
	})

	t.Run("a value result sharing its _meta", func(t *testing.T) {
		meta := &mcp.Meta{AdditionalFields: map[string]any{"kept": true}}
		result := mcp.ListToolsResult{Meta: meta}
		resp := s.decorateResponse(t.Context(), info, mcp.MethodToolsList, mcp.JSONRPCResponse{Result: result})
		decorated, ok := resp.(mcp.JSONRPCResponse).Result.(mcp.ListToolsResult)
		require.True(t, ok)
		assert.NotNil(t, decorated.Meta.ServerInfo())
		assert.Nil(t, meta.ServerInfo(), "decoration wrote into the handler's _meta")
		assert.Len(t, meta.AdditionalFields, 1)
	})
}
