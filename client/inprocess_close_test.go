package client

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Closing an in-process client ends the request in flight: the tool's
// context is cancelled and the call returns, as a dropped connection does on
// the wire transports.
func TestInProcessCloseEndsTheRequestInFlight(t *testing.T) {
	s := server.NewMCPServer("test-server", "1.0.0", server.WithToolCapabilities(true))
	running := make(chan struct{}, 1)
	s.AddTool(mcp.NewTool("block"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		running <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	c := connectInProcess(t, s, "")

	returned := make(chan error, 1)
	go func() {
		request := mcp.CallToolRequest{}
		request.Params.Name = "block"
		_, err := c.CallTool(t.Context(), request)
		returned <- err
	}()
	receive(t, running)

	require.NoError(t, c.Close())
	require.Error(t, receive(t, returned), "the call in flight at Close returned")
}

// Closing an in-process client ends its subscriptions/listen stream.
func TestInProcessCloseEndsTheListenStream(t *testing.T) {
	s, closed := newListenServer(server.WithToolCapabilities(true))
	c := connectInProcess(t, s, "")
	notifications := collectNotifications(c)

	returned := make(chan error, 1)
	go func() { returned <- c.Listen(t.Context(), mcp.SubscriptionFilter{ToolsListChanged: true}) }()
	require.Equal(t, mcp.MethodNotificationSubscriptionsAcknowledged, receive(t, notifications).Method)

	require.NoError(t, c.Close())
	_ = receive(t, returned)
	require.Equal(t, int32(1), closed.Load(), "the server closed the stream")
}
