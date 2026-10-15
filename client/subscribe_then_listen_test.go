package client

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// listenFilters records the filter of every subscriptions/listen request a
// server receives.
func listenFilters(hooks *server.Hooks) <-chan mcp.SubscriptionFilter {
	filters := make(chan mcp.SubscriptionFilter, 4)
	hooks.AddBeforeSubscriptionsListen(func(_ context.Context, _ any, request *mcp.SubscriptionsListenRequest) {
		filters <- request.Params.Notifications
	})
	return filters
}

// Subscribe records a URI for the next Listen call, which sends it along with
// its own filter and keeps it for the call after that.
func TestListenSendsTheURIsSubscribeRecorded(t *testing.T) {
	hooks := &server.Hooks{}
	filters := listenFilters(hooks)
	s := server.NewMCPServer("test-server", "1.0.0", server.WithToolCapabilities(true), server.WithHooks(hooks))
	c := connectInProcess(t, s, "")

	require.NoError(t, c.Subscribe(t.Context(), mcp.SubscribeRequest{Params: mcp.SubscribeParams{URI: "file:///a"}}))

	stop, err := c.ListenAsync(t.Context(), mcp.SubscriptionFilter{ToolsListChanged: true}, nil)
	require.NoError(t, err)
	sent := receive(t, filters)
	stop()

	assert.Equal(t, mcp.SubscriptionFilter{ToolsListChanged: true, ResourceSubscriptions: []string{"file:///a"}}, sent,
		"the listen request carries the recorded URI beside the caller's filter")
	assert.Equal(t, []string{"file:///a"}, c.PendingSubscriptionFilter().ResourceSubscriptions,
		"the recorded URI is still pending for the next Listen")
}

// The filter Listen is given stays the caller's: Subscribe and Unsubscribe
// afterwards do not write into the caller's slice.
func TestListenDoesNotWriteIntoTheCallersFilter(t *testing.T) {
	hooks := &server.Hooks{}
	filters := listenFilters(hooks)
	s := server.NewMCPServer("test-server", "1.0.0", server.WithToolCapabilities(true), server.WithHooks(hooks))
	c := connectInProcess(t, s, "")

	mine := make([]string, 2, 8)
	copy(mine, []string{"file:///a", "file:///b"})
	stop, err := c.ListenAsync(t.Context(), mcp.SubscriptionFilter{ToolsListChanged: true, ResourceSubscriptions: mine}, nil)
	require.NoError(t, err)
	receive(t, filters)

	require.NoError(t, c.Unsubscribe(t.Context(), mcp.UnsubscribeRequest{Params: mcp.UnsubscribeParams{URI: "file:///a"}}))
	require.NoError(t, c.Subscribe(t.Context(), mcp.SubscribeRequest{Params: mcp.SubscribeParams{URI: "file:///c"}}))
	stop()

	assert.Equal(t, []string{"file:///a", "file:///b"}, mine, "the caller's slice is untouched")
	assert.Equal(t, []string{"file:///c"}, c.PendingSubscriptionFilter().ResourceSubscriptions,
		"only Subscribe's URI is recorded; Listen's own were not")
}
