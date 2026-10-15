package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// A request whose id is still in flight in its session is refused, and the
// in-flight request is left alone (BaseProtocol.md row 4): a cancel for the
// id still reaches the first request.
func TestARequestReusingAnInFlightIDIsRefused(t *testing.T) {
	entered := make(chan struct{})
	s := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	s.AddTool(mcp.NewTool("wait"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	session := &fakeSession{sessionID: "s1", notificationChannel: make(chan mcp.JSONRPCNotification, 10), initialized: true}
	ctx := s.WithContext(t.Context(), session)

	first := make(chan mcp.JSONRPCMessage, 1)
	go func() {
		first <- s.HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wait"}}`))
	}()
	<-entered

	second := s.HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	errResp, ok := second.(mcp.JSONRPCError)
	require.True(t, ok, "a request reusing an in-flight id was answered: %v", second)
	require.Equal(t, mcp.INVALID_REQUEST, errResp.Error.Code)

	s.HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`))
	select {
	case <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("the cancel for the first request was lost")
	}
}

// Without a session there is no scope to refuse in: two callers of
// HandleMessage with no session, unrelated to each other, may use one id at
// once, and both are served, as before.
func TestSessionlessRequestsMayShareAnID(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	s := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	s.AddTool(mcp.NewTool("wait"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		entered <- struct{}{}
		<-release
		return mcp.NewToolResultText("ran"), nil
	})
	call := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wait"}}`)
	results := make(chan mcp.JSONRPCMessage, 2)
	for range 2 {
		go func() { results <- s.HandleMessage(t.Context(), call) }()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("a sessionless request was refused for an id another caller uses")
		}
	}
	close(release)
	for range 2 {
		result := <-results
		_, ok := result.(mcp.JSONRPCResponse)
		require.True(t, ok, "a sessionless request was refused: %v", result)
	}
}
