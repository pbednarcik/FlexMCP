package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A Start refused for a conflicting address leaves the server usable: a
// Shutdown after it returns.
func TestStreamableHTTP_RefusedStartLeavesTheServerUsable(t *testing.T) {
	httpServer := NewStreamableHTTPServer(NewMCPServer("test", "1.0.0"),
		WithStreamableHTTPServer(&http.Server{Addr: "127.0.0.1:1"}))
	require.Error(t, httpServer.Start("127.0.0.1:2"))

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- httpServer.Shutdown(ctx) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown blocked after a refused Start")
	}
}
