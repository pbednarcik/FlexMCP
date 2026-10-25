package transport

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
)

func TestInProcessRegistrationHookCanCloseTransport(t *testing.T) {
	hooks := &server.Hooks{}
	s := server.NewMCPServer("test", "1", server.WithHooks(hooks))
	tr := NewInProcessTransport(s)
	var unregistered atomic.Int32
	hooks.AddOnRegisterSession(func(context.Context, server.ClientSession) { _ = tr.Close() })
	hooks.AddOnUnregisterSession(func(context.Context, server.ClientSession) { unregistered.Add(1) })
	done := make(chan error, 1)
	go func() { done <- tr.Start(t.Context()) }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrTransportClosed)
	case <-time.After(time.Second):
		t.Fatal("registration hook deadlocked closing the transport")
	}
	require.Equal(t, int32(1), unregistered.Load(), "registration must be cleaned up after the hook closes the transport")
	require.ErrorIs(t, tr.Start(t.Context()), ErrTransportClosed)
}

func TestConcurrentInProcessStartsWaitForRegistration(t *testing.T) {
	hooks := &server.Hooks{}
	s := server.NewMCPServer("test", "1", server.WithHooks(hooks))
	tr := NewInProcessTransport(s)
	entered, release := make(chan struct{}), make(chan struct{})
	var registrations atomic.Int32
	hooks.AddOnRegisterSession(func(context.Context, server.ClientSession) {
		registrations.Add(1)
		close(entered)
		<-release
	})
	results := make(chan error, 2)
	go func() { results <- tr.Start(t.Context()) }()
	<-entered
	go func() { results <- tr.Start(t.Context()) }()
	select {
	case err := <-results:
		close(release)
		t.Fatalf("Start returned before registration finished: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	require.Equal(t, int32(1), registrations.Load())
	require.NoError(t, tr.Close())
}

// A registration hook that closes the transport while holding a lock the
// unregister hooks also take does not deadlock: the unregister hooks run
// after the registration hooks return.
func TestInProcessRegistrationHookClosesUnderALockTheUnregisterHookTakes(t *testing.T) {
	hooks := &server.Hooks{}
	s := server.NewMCPServer("test", "1", server.WithHooks(hooks))
	tr := NewInProcessTransport(s)
	var mu sync.Mutex
	hooks.AddOnRegisterSession(func(context.Context, server.ClientSession) {
		mu.Lock()
		defer mu.Unlock()
		_ = tr.Close()
	})
	hooks.AddOnUnregisterSession(func(context.Context, server.ClientSession) {
		mu.Lock()
		defer mu.Unlock()
	})
	done := make(chan error, 1)
	go func() { done <- tr.Start(t.Context()) }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrTransportClosed)
	case <-time.After(time.Second):
		t.Fatal("registration hook deadlocked against the unregister hook")
	}
}

// A Close while a registration hook runs leaves the session to the Start
// registering it, which unregisters it once the hook returns, exactly once,
// and the client receives nothing the server sent it meanwhile.
func TestInProcessCloseDuringRegistrationDeliversNothing(t *testing.T) {
	hooks := &server.Hooks{}
	s := server.NewMCPServer("test", "1", server.WithHooks(hooks))
	tr := NewInProcessTransport(s)
	var received, unregistered atomic.Int32
	tr.SetNotificationHandler(func(mcp.JSONRPCNotification) { received.Add(1) })
	entered, release := make(chan string), make(chan struct{})
	hooks.AddOnRegisterSession(func(_ context.Context, session server.ClientSession) {
		session.Initialize()
		entered <- session.SessionID()
		<-release
	})
	hooks.AddOnUnregisterSession(func(context.Context, server.ClientSession) { unregistered.Add(1) })
	done := make(chan error, 1)
	go func() { done <- tr.Start(t.Context()) }()
	sessionID := <-entered

	require.NoError(t, tr.Close())
	_ = s.SendNotificationToSpecificClient(sessionID, "notifications/message", nil)
	close(release)
	require.ErrorIs(t, <-done, ErrTransportClosed)

	require.ErrorIs(t, s.SendNotificationToSpecificClient(sessionID, "notifications/message", nil), server.ErrSessionNotFound)
	require.Equal(t, int32(1), unregistered.Load())
	require.Zero(t, received.Load(), "a closed transport delivered a notification")
}
