package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// A cancel takes a request's id out of flight, and the client may reuse the
// id at once. The cancelled request, still winding down, must not take its
// successor's filing with it when it returns: a third request on the id is
// refused while the second runs, and a cancel for the second still reaches
// it.
func TestACancelledRequestsReturnLeavesItsSuccessorFiled(t *testing.T) {
	type call struct {
		entered, release chan struct{}
		ignoresCancel    bool
	}
	calls := map[string]*call{
		"a": {entered: make(chan struct{}), release: make(chan struct{}), ignoresCancel: true},
		"b": {entered: make(chan struct{}), release: make(chan struct{})},
	}
	s := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	s.AddTool(mcp.NewTool("wait"), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		c := calls[request.GetString("who", "")]
		close(c.entered)
		if c.ignoresCancel {
			<-c.release
		} else {
			select {
			case <-c.release:
			case <-ctx.Done():
			}
		}
		return mcp.NewToolResultText("done"), nil
	})
	session := &fakeSession{sessionID: "s1", notificationChannel: make(chan mcp.JSONRPCNotification, 10), initialized: true}
	ctx := s.WithContext(t.Context(), session)
	callTool := func(who string) <-chan mcp.JSONRPCMessage {
		returned := make(chan mcp.JSONRPCMessage, 1)
		go func() {
			returned <- s.HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wait","arguments":{"who":"`+who+`"}}}`))
		}()
		return returned
	}
	cancelID1 := func() {
		s.HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`))
	}

	first := callTool("a")
	<-calls["a"].entered
	cancelID1() // a's id is out of flight; a itself goes on
	second := callTool("b")
	<-calls["b"].entered
	close(calls["a"].release)
	select {
	case <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("the first request never returned")
	}

	third := s.HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	errResp, refused := third.(mcp.JSONRPCError)
	require.True(t, refused, "the cancelled request's return unfiled its successor: a third request on the id was served: %v", third)
	require.Equal(t, mcp.INVALID_REQUEST, errResp.Error.Code)

	cancelID1()
	select {
	case <-second:
	case <-time.After(3 * time.Second):
		t.Fatal("the cancel for the second request was lost")
	}
}

// In-flight ids are scoped by session, not by the text of session and id
// joined: a session ID with a delimiter in it cannot make another session's
// request look like its own.
func TestInFlightIDsDoNotCollideAcrossSessionsWithDelimiters(t *testing.T) {
	entered := make(chan struct{})
	s := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	s.AddTool(mcp.NewTool("wait"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	colon := s.WithContext(t.Context(), &fakeSession{sessionID: "a:string", notificationChannel: make(chan mcp.JSONRPCNotification, 10), initialized: true})
	plain := s.WithContext(t.Context(), &fakeSession{sessionID: "a", notificationChannel: make(chan mcp.JSONRPCNotification, 10), initialized: true})

	first := make(chan mcp.JSONRPCMessage, 1)
	go func() {
		first <- s.HandleMessage(colon, []byte(`{"jsonrpc":"2.0","id":"x","method":"tools/call","params":{"name":"wait"}}`))
	}()
	<-entered

	// Session "a", id "string:x": another session, another id.
	second := s.HandleMessage(plain, []byte(`{"jsonrpc":"2.0","id":"string:x","method":"ping"}`))
	_, served := second.(mcp.JSONRPCResponse)
	require.True(t, served, "a request was refused for an id in flight in another session: %v", second)

	s.HandleMessage(colon, []byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"x"}}`))
	select {
	case <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("the cancel for the first request was lost")
	}
}

// A request refused for reusing an in-flight id runs no request hook: the
// initialization hook does not see it, and the error hook does, with
// ErrRequestIDInFlight.
func TestARefusedDuplicateIDReachesOnlyTheErrorHook(t *testing.T) {
	var initializations atomic.Int32
	errs := make(chan error, 4)
	hooks := &Hooks{}
	hooks.AddOnRequestInitialization(func(context.Context, any, any) error { initializations.Add(1); return nil })
	hooks.AddOnError(func(_ context.Context, _ any, _ mcp.MCPMethod, _ any, err error) { errs <- err })
	entered := make(chan struct{})
	s := NewMCPServer("test", "1.0.0", WithHooks(hooks), WithToolCapabilities(false))
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
	errResp, refused := second.(mcp.JSONRPCError)
	require.True(t, refused, "the duplicate was served: %v", second)
	require.Equal(t, mcp.INVALID_REQUEST, errResp.Error.Code)
	require.Equal(t, int32(1), initializations.Load(), "the refused request ran the initialization hook")
	select {
	case err := <-errs:
		require.ErrorIs(t, err, ErrRequestIDInFlight)
	default:
		t.Fatal("the refusal did not reach the error hook")
	}

	s.HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`))
	select {
	case <-first:
	case <-time.After(3 * time.Second):
		t.Fatal("the cancel for the first request was lost")
	}
}
