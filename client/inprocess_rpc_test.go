package client

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRootListChangesReachesTheServer(t *testing.T) {
	received := make(chan mcp.JSONRPCNotification, 1)
	s := server.NewMCPServer("test-server", "1.0.0")
	s.AddNotificationHandler(mcp.MethodNotificationRootsListChanged, func(_ context.Context, n mcp.JSONRPCNotification) {
		received <- n
	})
	c := connectInProcess(t, s, mcp.LATEST_LEGACY_PROTOCOL_VERSION)

	require.NoError(t, c.RootListChanges(t.Context()))
	assert.Equal(t, mcp.MethodNotificationRootsListChanged, receive(t, received).Method)
}

// newLoggingServer counts the logging/setLevel requests it serves and offers
// a tool reporting the log level a request declared.
func newLoggingServer(setLevels *atomic.Int32) *server.MCPServer {
	hooks := &server.Hooks{}
	hooks.AddAfterSetLevel(func(context.Context, any, *mcp.SetLevelRequest, *mcp.EmptyResult) { setLevels.Add(1) })
	s := server.NewMCPServer("test-server", "1.0.0", server.WithLogging(), server.WithToolCapabilities(false), server.WithHooks(hooks))
	s.AddTool(mcp.NewTool("level"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if info := server.RequestProtocolInfoFromContext(ctx); info != nil {
			return mcp.NewToolResultText(string(info.LogLevel)), nil
		}
		return mcp.NewToolResultText(""), nil
	})
	return s
}

func TestSetLevelIsSentOnlyOnALegacyConnection(t *testing.T) {
	tests := []struct {
		name          string
		version       string
		wantSetLevels int32
		wantDeclared  string
	}{
		{name: "legacy sends logging/setLevel", version: mcp.LATEST_LEGACY_PROTOCOL_VERSION, wantSetLevels: 1},
		{name: "modern declares the level on every request", wantDeclared: "debug"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var setLevels atomic.Int32
			c := connectInProcess(t, newLoggingServer(&setLevels), tt.version)

			require.NoError(t, c.SetLevel(t.Context(), mcp.SetLevelRequest{Params: mcp.SetLevelParams{Level: mcp.LoggingLevelDebug}}))
			assert.Equal(t, tt.wantSetLevels, setLevels.Load())

			result, err := c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "level"}})
			require.NoError(t, err)
			assert.Equal(t, tt.wantDeclared, result.Content[0].(mcp.TextContent).Text)
		})
	}

	t.Run("legacy reports a server without logging", func(t *testing.T) {
		c := connectInProcess(t, server.NewMCPServer("test-server", "1.0.0"), mcp.LATEST_LEGACY_PROTOCOL_VERSION)

		err := c.SetLevel(t.Context(), mcp.SetLevelRequest{Params: mcp.SetLevelParams{Level: mcp.LoggingLevelDebug}})
		require.ErrorContains(t, err, "logging")
	})
}

func TestCompleteReportsAServerThatCannotComplete(t *testing.T) {
	s := server.NewMCPServer("test-server", "1.0.0", server.WithPromptCapabilities(true))
	c := connectInProcess(t, s, "")

	_, err := c.Complete(t.Context(), mcp.CompleteRequest{Params: mcp.CompleteParams{
		Ref:      mcp.PromptReference{Type: "ref/prompt", Name: "greeting"},
		Argument: mcp.CompleteArgument{Name: "name", Value: "M"},
	}})
	require.ErrorContains(t, err, "completions")
}
