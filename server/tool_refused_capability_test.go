package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// A tool refused at registration leaves the server as it was: a server
// that recovers the panic does not advertise tools it does not have, nor
// serve tools/list for them; the first tool registered turns tools on.
func TestARefusedToolDoesNotTurnOnTools(t *testing.T) {
	refused := mcp.NewTool("bad name")
	handler := func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil }
	taskHandler := func(context.Context, mcp.CallToolRequest) (*mcp.CreateTaskResult, error) { return nil, nil }
	for name, register := range map[string]func(*MCPServer){
		"AddTool": func(s *MCPServer) { s.AddTool(refused, handler) },
		"AddTools": func(s *MCPServer) {
			s.AddTools(ServerTool{Tool: mcp.NewTool("good"), Handler: handler}, ServerTool{Tool: refused, Handler: handler})
		},
		"SetTools":    func(s *MCPServer) { s.SetTools(ServerTool{Tool: refused, Handler: handler}) },
		"AddTaskTool": func(s *MCPServer) { s.AddTaskTool(refused, taskHandler) },
	} {
		t.Run(name, func(t *testing.T) {
			srv := NewMCPServer("p", "1.0.0")
			require.Panics(t, func() { register(srv) })
			assert.Nil(t, initializedCapabilities(t, srv).Tools, "no tools advertised")
			list := srv.HandleMessage(t.Context(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
			listErr, ok := list.(mcp.JSONRPCError)
			require.True(t, ok, "tools/list is not served: %#v", list)
			assert.Equal(t, mcp.METHOD_NOT_FOUND, listErr.Error.Code)

			srv.AddTool(mcp.NewTool("good"), handler)
			require.NotNil(t, initializedCapabilities(t, srv).Tools, "the first tool registered turns tools on")
		})
	}
}

// initializedCapabilities returns the capabilities srv answers initialize
// with.
func initializedCapabilities(t *testing.T, srv *MCPServer) mcp.ServerCapabilities {
	t.Helper()
	response := srv.HandleMessage(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`))
	encoded, err := json.Marshal(response)
	require.NoError(t, err)
	var answer struct {
		Result mcp.InitializeResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(encoded, &answer))
	return answer.Result.Capabilities
}
