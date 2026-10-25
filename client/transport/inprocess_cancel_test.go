package transport

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
)

// A notifications/cancelled sent over the in-process transport reaches the
// request it names: the server files requests under the session, and the
// notification carries the session too (upstream #1067).
func TestInProcessCancelReachesItsRequest(t *testing.T) {
	s := server.NewMCPServer("test", "1")
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	s.AddTool(mcp.NewTool("block"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		return mcp.NewToolResultText("done"), nil
	})
	tr := NewInProcessTransport(s)
	require.NoError(t, tr.Start(t.Context()))
	defer tr.Close()

	_, err := tr.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(1)), Method: "initialize",
		Params: map[string]any{"protocolVersion": mcp.ProtocolVersion20251125, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "c", "version": "1"}}})
	require.NoError(t, err)

	go func() {
		_, _ = tr.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(7)), Method: "tools/call",
			Params: map[string]any{"name": "block"}})
	}()
	<-entered
	require.NoError(t, tr.SendNotification(t.Context(), mcp.JSONRPCNotification{JSONRPC: "2.0",
		Method: "notifications/cancelled", Params: mcp.NotificationParams{AdditionalFields: map[string]any{"requestId": 7}}}))
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("notifications/cancelled over the in-process transport did not cancel request 7")
	}
}
