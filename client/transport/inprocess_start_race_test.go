package transport

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
)

// Start and a notification sent meanwhile do not race on the session: a
// sender reads it under the transport's lock, as Start writes it.
func TestInProcessStartAndSendNotificationDoNotRace(t *testing.T) {
	tr := NewInProcessTransport(server.NewMCPServer("test", "1.0.0"))
	t.Cleanup(func() { _ = tr.Close() })
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		_ = tr.SendNotification(t.Context(), mcp.JSONRPCNotification{JSONRPC: mcp.JSONRPC_VERSION, Method: "notifications/initialized"})
	}()
	require.NoError(t, tr.Start(t.Context()))
	<-sent
}
