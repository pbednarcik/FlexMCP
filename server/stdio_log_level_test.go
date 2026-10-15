package server

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// A handshake-era client's logging/setLevel sets its session's threshold
// (2025-11-25 Utilities.md row 14): a level it asked for reaches it. An
// acknowledged request is not ignored.
func TestStdioKeepsALegacyClientsLogLevel(t *testing.T) {
	mcpServer := NewMCPServer("test", "1.0.0", WithLogging())
	send, out := stdioPipes(t, mcpServer)
	initializeStdio(t, send, out)
	send(`{"jsonrpc":"2.0","id":2,"method":"logging/setLevel","params":{"level":"debug"}}`)
	awaitMessage(t, out, func(m map[string]any) bool { return m["id"] == float64(2) }, "the setLevel result")

	require.NoError(t, mcpServer.SendLogMessageToSpecificClient("stdio", mcp.LoggingMessageNotification{
		Method: "notifications/message",
		Params: mcp.LoggingMessageNotificationParams{Level: mcp.LoggingLevelDebug, Data: "hello"},
	}))
	awaitMessage(t, out, func(m map[string]any) bool { return m["method"] == "notifications/message" },
		"the debug message the client asked for")
}
