package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// blockingToolServer serves one tool, "block", whose handler closes started
// and then waits for its context to end.
func blockingToolServer(t *testing.T) (*MCPServer, <-chan struct{}) {
	t.Helper()
	started := make(chan struct{})
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	srv.AddTool(mcp.NewTool("block"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	return srv, started
}

func callBlockInBackground(srv *MCPServer, ctx context.Context) <-chan mcp.JSONRPCMessage {
	responses := make(chan mcp.JSONRPCMessage, 1)
	go func() {
		responses <- srv.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"block"}}`))
	}()
	return responses
}

func waitStarted(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not start")
	}
}

func cancelTestSession(id string) *sessionTestClient {
	return &sessionTestClient{sessionID: id, notificationChannel: make(chan mcp.JSONRPCNotification, 1), initialized: true}
}

const cancelSeven = `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`

// TestCancelledNotificationCancelsInflightRequest pins the cancellation
// contract: notifications/cancelled for an in-flight request ID ends that
// request's context, the handler returns, and the caller gets an error
// response instead of waiting for a result that will never come.
func TestCancelledNotificationCancelsInflightRequest(t *testing.T) {
	srv, started := blockingToolServer(t)
	ctx := srv.WithContext(t.Context(), cancelTestSession("cancel-session"))

	responses := callBlockInBackground(srv, ctx)
	waitStarted(t, started)

	require.Nil(t, srv.HandleMessage(ctx, json.RawMessage(cancelSeven)), "a notification has no response")

	select {
	case resp := <-responses:
		errResp, ok := resp.(mcp.JSONRPCError)
		require.True(t, ok, "expected an error response, got %T", resp)
		assert.Equal(t, mcp.INTERNAL_ERROR, errResp.Error.Code)
		assert.Contains(t, errResp.Error.Message, context.Canceled.Error())
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not end after notifications/cancelled")
	}
}

// TestCancelledNotificationIsScopedToTheSession pins that the in-flight key
// is session-scoped: a cancel for request 7 from another session leaves this
// session's request 7 running.
func TestCancelledNotificationIsScopedToTheSession(t *testing.T) {
	srv, started := blockingToolServer(t)
	ctx := srv.WithContext(t.Context(), cancelTestSession("mine"))

	responses := callBlockInBackground(srv, ctx)
	waitStarted(t, started)

	require.Nil(t, srv.HandleMessage(srv.WithContext(t.Context(), cancelTestSession("other")), json.RawMessage(cancelSeven)))
	select {
	case resp := <-responses:
		t.Fatalf("another session's cancel ended the request: %v", resp)
	case <-time.After(200 * time.Millisecond):
	}

	require.Nil(t, srv.HandleMessage(ctx, json.RawMessage(cancelSeven)))
	select {
	case <-responses:
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not end after its own session's cancel")
	}
}
