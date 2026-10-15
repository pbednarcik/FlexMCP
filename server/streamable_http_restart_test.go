package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A server is not restarted: Start after Shutdown returns
// http.ErrServerClosed, whether or not Start had run before, since the
// listen streams Shutdown ended stay ended.
func TestStreamableHTTP_StartAfterShutdownIsRefused(t *testing.T) {
	httpServer := NewStreamableHTTPServer(NewMCPServer("test", "1.0.0"))
	require.NoError(t, httpServer.Shutdown(t.Context()))
	t.Cleanup(func() { _ = httpServer.Shutdown(t.Context()) })

	started := make(chan error, 1)
	go func() { started <- httpServer.Start("127.0.0.1:0") }()
	select {
	case err := <-started:
		require.ErrorIs(t, err, http.ErrServerClosed)
	case <-time.After(2 * time.Second):
		t.Fatal("Start served after Shutdown")
	}
}
