package client

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
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

// A Listen call superseded by a later one returns without disturbing the
// later stream: a third call still ends the second, as the second ended the
// first.
func TestASupersededListenLeavesTheNewStreamInCharge(t *testing.T) {
	s, closed := newListenServer(server.WithToolCapabilities(true))
	c := connectInProcess(t, s, "")
	notifications := collectNotifications(c)
	filter := mcp.SubscriptionFilter{ToolsListChanged: true}

	stopFirst, err := c.ListenAsync(t.Context(), filter, nil)
	require.NoError(t, err)
	require.Equal(t, mcp.MethodNotificationSubscriptionsAcknowledged, receive(t, notifications).Method)

	second := make(chan error, 1)
	go func() { second <- c.Listen(t.Context(), filter) }()
	require.Equal(t, mcp.MethodNotificationSubscriptionsAcknowledged, receive(t, notifications).Method)
	stopFirst()
	assert.Equal(t, int32(1), closed.Load(), "the second call ended the first stream")

	stopThird, err := c.ListenAsync(t.Context(), filter, nil)
	require.NoError(t, err)
	require.Equal(t, mcp.MethodNotificationSubscriptionsAcknowledged, receive(t, notifications).Method)
	_ = receive(t, second)
	assert.Equal(t, int32(2), closed.Load(), "the third call ended the second stream")

	stopThird()
	assert.Equal(t, int32(3), closed.Load())
}

// A Listen call's own URIs are its own: a later call with a narrower filter
// sends the narrower filter (the Unsubscribe deprecation note points
// callers at this), while the URIs Subscribe recorded stay until Unsubscribe.
func TestReopeningListenWithANarrowerFilterNarrows(t *testing.T) {
	hooks := &server.Hooks{}
	filters := listenFilters(hooks)
	s := server.NewMCPServer("test-server", "1.0.0", server.WithToolCapabilities(true), server.WithHooks(hooks))
	c := connectInProcess(t, s, "")

	require.NoError(t, c.Subscribe(t.Context(), mcp.SubscribeRequest{Params: mcp.SubscribeParams{URI: "file:///recorded"}}))

	stop, err := c.ListenAsync(t.Context(), mcp.SubscriptionFilter{ToolsListChanged: true, ResourceSubscriptions: []string{"file:///a", "file:///b", "file:///a"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"file:///a", "file:///b", "file:///recorded"}, receive(t, filters).ResourceSubscriptions,
		"the first call sends its own URIs once each, plus the recorded one")
	stop()

	stop, err = c.ListenAsync(t.Context(), mcp.SubscriptionFilter{ToolsListChanged: true, ResourceSubscriptions: []string{"file:///a"}}, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"file:///a", "file:///recorded"}, receive(t, filters).ResourceSubscriptions,
		"the second call sends only its own URI, plus the recorded one")
	stop()

	assert.Equal(t, []string{"file:///recorded"}, c.PendingSubscriptionFilter().ResourceSubscriptions,
		"only Subscribe's URIs are recorded")
}

// Subscribe and Unsubscribe during a Listen do not race with the request
// being sent: the filter sent is a slice of its own.
func TestSubscribeDuringListenDoesNotRaceWithTheSend(t *testing.T) {
	s := server.NewMCPServer("test-server", "1.0.0", server.WithToolCapabilities(true))
	c := connectInProcess(t, s, "")
	require.NoError(t, c.Subscribe(t.Context(), mcp.SubscribeRequest{Params: mcp.SubscribeParams{URI: "file:///a"}}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 50 {
			_ = c.Unsubscribe(t.Context(), mcp.UnsubscribeRequest{Params: mcp.UnsubscribeParams{URI: "file:///a"}})
			_ = c.Subscribe(t.Context(), mcp.SubscribeRequest{Params: mcp.SubscribeParams{URI: "file:///a"}})
		}
	}()
	for range 20 {
		stop, err := c.ListenAsync(t.Context(), mcp.SubscriptionFilter{ToolsListChanged: true}, nil)
		require.NoError(t, err)
		stop()
	}
	receive(t, done)
}
