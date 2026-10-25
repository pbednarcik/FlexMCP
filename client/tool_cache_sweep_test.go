package client

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/client/transport"
	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
)

// listingScript answers tools/list with the tools named, one listing per
// call, and the previous listing's definitions are the ones replaced.
func listingScript(listings ...[]string) func(context.Context, int, transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	listed := 0
	return modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		names := listings[min(listed, len(listings)-1)]
		listed++
		tools := make([]map[string]any, 0, len(names))
		for _, name := range names {
			tools = append(tools, map[string]any{"name": name, "inputSchema": map[string]any{"type": "object"}})
		}
		encoded, err := json.Marshal(map[string]any{"tools": tools})
		if err != nil {
			return nil, err
		}
		return resultResponse(request, string(encoded)), nil
	})
}

// A complete listing replaces the cached tool definitions: a tool the
// server stopped listing is no longer known.
func TestACompleteListingReplacesTheToolCache(t *testing.T) {
	tr := &scriptedHTTPTransport{scriptedTransport{respond: listingScript([]string{"a", "b"}, []string{"b"})}}
	c := NewClient(tr)
	require.NoError(t, c.Start(t.Context()))
	_, err := c.Initialize(t.Context(), mcp.InitializeRequest{})
	require.NoError(t, err)

	_, err = c.ListTools(t.Context(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	require.NotNil(t, c.toolForCall(json.RawMessage(`{"name":"a"}`)), "a is known after the first listing")

	_, err = c.ListTools(t.Context(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	assert.Nil(t, c.toolForCall(json.RawMessage(`{"name":"a"}`)), "a is forgotten after a listing without it")
	assert.NotNil(t, c.toolForCall(json.RawMessage(`{"name":"b"}`)))
}

// A connection that mirrors no headers keeps no tool cache.
func TestATransportWithoutHeadersKeepsNoToolCache(t *testing.T) {
	s := server.NewMCPServer("test-server", "1.0.0", server.WithToolCapabilities(true))
	s.AddTool(mcp.NewTool("a"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	c := connectInProcess(t, s, "")

	_, err := c.ListTools(t.Context(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	c.toolsMu.RLock()
	defer c.toolsMu.RUnlock()
	assert.Empty(t, c.knownTools)
}

// pagedListingScript answers tools/list in pages: a cursor names the page,
// the first page is the one asked for without one.
func pagedListingScript(pages map[string]string) func(context.Context, int, transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	return modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		var params struct {
			Cursor string `json:"cursor"`
		}
		encoded, err := json.Marshal(request.Params)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(encoded, &params); err != nil {
			return nil, err
		}
		return resultResponse(request, pages[params.Cursor]), nil
	})
}

// A walk that starts at a caller's cursor is not the server's whole list:
// its pages merge into the cache, and the tools of the pages before it stay
// known.
func TestAWalkFromACursorMergesIntoTheToolCache(t *testing.T) {
	tr := &scriptedHTTPTransport{scriptedTransport{respond: pagedListingScript(map[string]string{
		"":   `{"tools":[{"name":"a","inputSchema":{"type":"object"}}],"nextCursor":"p2"}`,
		"p2": `{"tools":[{"name":"b","inputSchema":{"type":"object"}}]}`,
	})}}
	c := NewClient(tr)
	require.NoError(t, c.Start(t.Context()))
	_, err := c.Initialize(t.Context(), mcp.InitializeRequest{})
	require.NoError(t, err)

	_, err = c.ListTools(t.Context(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	require.NotNil(t, c.toolForCall(json.RawMessage(`{"name":"a"}`)), "a is known after the whole walk")

	cursor := mcp.Cursor("p2")
	request := mcp.ListToolsRequest{}
	request.Params.Cursor = &cursor
	_, err = c.ListTools(t.Context(), request)
	require.NoError(t, err)
	assert.NotNil(t, c.toolForCall(json.RawMessage(`{"name":"a"}`)), "a stays known after a walk from a later page")
	assert.NotNil(t, c.toolForCall(json.RawMessage(`{"name":"b"}`)))
}

// IterTools caches as ListTools does: a walk from the first page to the last
// is the server's whole list and replaces the cache, while a walk the caller
// stops early only merges its pages.
func TestACompleteIterationReplacesTheToolCache(t *testing.T) {
	iterate := func(c *Client, stopEarly bool) {
		for _, err := range c.IterTools(t.Context(), mcp.ListToolsRequest{}) {
			require.NoError(t, err)
			if stopEarly {
				break
			}
		}
	}
	for name, stopEarly := range map[string]bool{"complete": false, "stopped early": true} {
		t.Run(name, func(t *testing.T) {
			tr := &scriptedHTTPTransport{scriptedTransport{respond: listingScript([]string{"a", "b"}, []string{"b"})}}
			c := NewClient(tr)
			require.NoError(t, c.Start(t.Context()))
			_, err := c.Initialize(t.Context(), mcp.InitializeRequest{})
			require.NoError(t, err)

			iterate(c, false)
			require.NotNil(t, c.toolForCall(json.RawMessage(`{"name":"a"}`)), "a is known after the first listing")

			iterate(c, stopEarly)
			assert.Equal(t, stopEarly, c.toolForCall(json.RawMessage(`{"name":"a"}`)) != nil,
				"a is forgotten only after a complete walk without it")
			assert.NotNil(t, c.toolForCall(json.RawMessage(`{"name":"b"}`)))
		})
	}
}
