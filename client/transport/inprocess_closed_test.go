package transport

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
)

// A closed in-process transport refuses to send, as the stdio transport does.
func TestInProcessSendAfterCloseIsRefused(t *testing.T) {
	tr := NewInProcessTransport(server.NewMCPServer("test-server", "1.0.0"))
	require.NoError(t, tr.Start(t.Context()))
	require.NoError(t, tr.Close())

	_, err := tr.SendRequest(t.Context(), JSONRPCRequest{
		JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(1), Method: string(mcp.MethodPing),
	})
	require.ErrorIs(t, err, ErrTransportClosed)

	err = tr.SendNotification(t.Context(), mcp.JSONRPCNotification{
		JSONRPC: mcp.JSONRPC_VERSION, Method: string(mcp.MethodNotificationInitialized),
	})
	require.ErrorIs(t, err, ErrTransportClosed)
}
