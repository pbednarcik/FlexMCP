package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// startedServer runs Start on a loopback port and returns the http.Server it
// built, once it has; Shutdown runs at the end of the test.
func startedServer(t *testing.T, httpServer *StreamableHTTPServer) *http.Server {
	t.Helper()
	t.Cleanup(func() { _ = httpServer.Shutdown(t.Context()) })
	go func() { _ = httpServer.Start("127.0.0.1:0") }()
	var srv *http.Server
	require.Eventually(t, func() bool {
		httpServer.mu.RLock()
		defer httpServer.mu.RUnlock()
		srv = httpServer.httpServer
		return srv != nil
	}, 2*time.Second, 5*time.Millisecond, "Start did not build the server")
	return srv
}

// WithReadHeaderTimeout and WithIdleTimeout land on the http.Server Start
// builds; zero or less leaves net/http's own, which is none. The defaults
// are pinned by TestStreamableHTTP_StartSetsServerTimeouts.
func TestStreamableHTTP_StartAppliesTheConfiguredTimeouts(t *testing.T) {
	t.Run("configured", func(t *testing.T) {
		srv := startedServer(t, NewStreamableHTTPServer(NewMCPServer("test", "1.0.0"),
			WithReadHeaderTimeout(7*time.Second), WithIdleTimeout(9*time.Second)))
		require.Equal(t, 7*time.Second, srv.ReadHeaderTimeout)
		require.Equal(t, 9*time.Second, srv.IdleTimeout)
	})
	t.Run("zero leaves none", func(t *testing.T) {
		srv := startedServer(t, NewStreamableHTTPServer(NewMCPServer("test", "1.0.0"),
			WithReadHeaderTimeout(0), WithIdleTimeout(-time.Second)))
		require.Zero(t, srv.ReadHeaderTimeout)
		require.Zero(t, srv.IdleTimeout)
	})
}
