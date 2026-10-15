package client

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// An input_required result answers tools/call, prompts/get and
// resources/read only (Patterns.md row 2): on any other request it is an
// invalid result, not an empty complete one.
func TestInputRequiredIsRefusedOutsideTheThreeRequests(t *testing.T) {
	t.Run("a listing", func(t *testing.T) {
		tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
			return resultResponse(request, `{"resultType":"input_required","tools":[],"inputRequests":{}}`), nil
		})}
		c := connectScripted(t, tr)
		_, err := c.ListToolsByPage(t.Context(), mcp.ListToolsRequest{})
		require.Error(t, err)
		assert.ErrorContains(t, err, "input_required")
	})
	t.Run("the discover probe", func(t *testing.T) {
		tr := &scriptedTransport{respond: func(_ context.Context, _ int, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
			return resultResponse(request, `{"resultType":"input_required","supportedVersions":["2026-07-28"],"capabilities":{}}`), nil
		}}
		c := NewClient(tr)
		require.NoError(t, c.Start(t.Context()))
		_, err := c.Initialize(t.Context(), mcp.InitializeRequest{})
		require.Error(t, err)
		assert.ErrorContains(t, err, "input_required")
	})
	t.Run("the discover probe answered with no discover fields", func(t *testing.T) {
		// The result type is judged before the shape: an input_required
		// answer is a modern server's, not a legacy server's unrelated
		// result, so no initialize follows it.
		tr := &scriptedTransport{respond: func(_ context.Context, _ int, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
			if request.Method == string(mcp.MethodServerDiscover) {
				return resultResponse(request, `{"resultType":"input_required","inputRequests":{},"requestState":"s"}`), nil
			}
			return resultResponse(request, legacyInitializeResult), nil
		}}
		c := NewClient(tr)
		require.NoError(t, c.Start(t.Context()))
		_, err := c.Initialize(t.Context(), mcp.InitializeRequest{})
		require.Error(t, err)
		assert.ErrorContains(t, err, "input_required")
		assert.Equal(t, []string{string(mcp.MethodServerDiscover)}, tr.sentMethods(), "no initialize was tried")
	})
}
