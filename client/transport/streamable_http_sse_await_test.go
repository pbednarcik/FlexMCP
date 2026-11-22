package transport

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// A response the stream queued before the request's context ended, by
// Close or the caller, is the answer: the response arrived. A select
// choosing at random between the two would fail the request about half the
// time.
func TestAResponseQueuedBeforeTheCancelIsTheAnswer(t *testing.T) {
	for range 200 {
		responses := make(chan *JSONRPCResponse, 1)
		queued := &JSONRPCResponse{ID: mcp.NewRequestId(1)}
		responses <- queued
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		response, err := awaitSSEResponse(ctx, responses, &sseStreamEnd{})
		require.NoError(t, err)
		require.Same(t, queued, response)
	}
}

// With nothing queued, the end of the context is the answer; a stream that
// ended without the response answers why it ended.
func TestAwaitSSEResponseWithNothingQueued(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := awaitSSEResponse(ctx, make(chan *JSONRPCResponse, 1), &sseStreamEnd{})
	require.ErrorIs(t, err, context.Canceled)

	closed := make(chan *JSONRPCResponse, 1)
	close(closed)
	_, err = awaitSSEResponse(t.Context(), closed, &sseStreamEnd{failure: assert.AnError})
	require.ErrorIs(t, err, assert.AnError)
}
