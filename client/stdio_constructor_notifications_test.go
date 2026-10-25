package client

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// The stdio constructors say they start the connection. A client they
// return receives the server's notifications without a further Start.
func TestStdioConstructorDeliversNotificationsWithoutStart(t *testing.T) {
	tempFile, err := os.CreateTemp(t.TempDir(), "mockstdio_server")
	require.NoError(t, err)
	require.NoError(t, tempFile.Close())
	mockServerPath := tempFile.Name() + ".exe"
	require.NoError(t, compileTestServer(mockServerPath))

	c, err := NewStdioMCPClient(mockServerPath, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	notifications := make(chan mcp.JSONRPCNotification, 1)
	c.OnNotification(func(n mcp.JSONRPCNotification) {
		select {
		case notifications <- n:
		default:
		}
	})

	_, err = c.Initialize(t.Context(), mcp.InitializeRequest{Params: mcp.InitializeParams{
		ProtocolVersion: mcp.LATEST_LEGACY_PROTOCOL_VERSION,
		ClientInfo:      mcp.Implementation{Name: "test-client", Version: "1.0.0"},
	}})
	require.NoError(t, err)

	// The mock server answers debug/echo_notification with a debug/test
	// notification before its response.
	_, err = c.sendRequest(t.Context(), "debug/echo_notification", nil, nil)
	require.NoError(t, err)

	require.Equal(t, "debug/test", receive(t, notifications).Method,
		"a notification sent after the constructor returned reaches the handler")
}
