package client

import (
	"context"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nilElicitationHandler models a handler that reports neither a result nor an
// error, which would panic if the result were dereferenced unchecked.
type nilElicitationHandler struct{}

func (nilElicitationHandler) Elicit(context.Context, mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
	return nil, nil
}

type nilSamplingHandler struct{}

func (nilSamplingHandler) CreateMessage(context.Context, mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
	return nil, nil
}

type nilRootsHandler struct{}

func (nilRootsHandler) ListRoots(context.Context, mcp.ListRootsRequest) (*mcp.ListRootsResult, error) {
	return nil, nil
}

func TestFulfillInputRequestRejectsNilHandlerResult(t *testing.T) {
	// A handler that returns (nil, nil) must surface an error rather than
	// panicking on a nil result.
	tests := []struct {
		name    string
		client  *Client
		request mcp.InputRequest
		wantErr string
	}{
		{
			name:    "elicitation",
			client:  &Client{elicitationHandler: nilElicitationHandler{}},
			request: mcp.NewElicitationInputRequest(mcp.ElicitationParams{Message: "hi", RequestedSchema: map[string]any{"type": "object"}}),
			wantErr: "elicitation handler returned no result",
		},
		{
			name:    "sampling",
			client:  &Client{samplingHandler: nilSamplingHandler{}},
			request: mcp.NewSamplingInputRequest(mcp.CreateMessageParams{MaxTokens: 1}),
			wantErr: "sampling handler returned no result",
		},
		{
			name:    "roots",
			client:  &Client{rootsHandler: nilRootsHandler{}},
			request: mcp.NewRootsInputRequest(),
			wantErr: "roots handler returned no result",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NotPanics(t, func() {
				_, err := tt.client.fulfillInputRequest(t.Context(), tt.request)
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			})
		})
	}
}

func TestSetLevelIsSafeForConcurrentUse(t *testing.T) {
	// SetLevel writes the level that applyRequestMeta stamps into every
	// outbound request. The two must not race.
	c := clientAtVersion(mcp.ProtocolVersion20260728)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				_ = c.SetLevel(t.Context(), mcp.SetLevelRequest{
					Params: mcp.SetLevelParams{Level: mcp.LoggingLevelDebug},
				})
			}
		})
		wg.Go(func() {
			for range 50 {
				c.applyRequestMeta(c.currentEra(), map[string]any{"name": "tool"})
			}
		})
	}
	wg.Wait()

	assert.Equal(t, mcp.LoggingLevelDebug, c.currentLogLevel())
}

func TestSubscribeAccumulatesFilterAcrossListens(t *testing.T) {
	// Subscribe records URIs for the next Listen call. They must not be
	// discarded when a stream ends.
	c := clientAtVersion(mcp.ProtocolVersion20260728)

	require.NoError(t, c.Subscribe(t.Context(), mcp.SubscribeRequest{
		Params: mcp.SubscribeParams{URI: "file:///a"},
	}))
	require.NoError(t, c.Subscribe(t.Context(), mcp.SubscribeRequest{
		Params: mcp.SubscribeParams{URI: "file:///b"},
	}))

	assert.Equal(t, []string{"file:///a", "file:///b"},
		c.PendingSubscriptionFilter().ResourceSubscriptions)

	require.NoError(t, c.Unsubscribe(t.Context(), mcp.UnsubscribeRequest{
		Params: mcp.UnsubscribeParams{URI: "file:///a"},
	}))
	assert.Equal(t, []string{"file:///b"},
		c.PendingSubscriptionFilter().ResourceSubscriptions)
}
