package client

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"
)

// connectInProcess starts an in-process client with options and initializes
// it, negotiating the newest protocol version when version is empty.
func connectInProcess(t *testing.T, s *server.MCPServer, version string, options ...ClientOption) *Client {
	t.Helper()
	c, err := NewInProcessClientWithOptions(s, options...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Start(t.Context()))
	_, err = c.Initialize(t.Context(), mcp.InitializeRequest{Params: mcp.InitializeParams{
		ProtocolVersion: version,
		ClientInfo:      mcp.Implementation{Name: "test-client", Version: "1.0.0"},
	}})
	require.NoError(t, err)
	return c
}

// sessionCapture records the session the in-process transport registers, so
// that a test can observe what the server learned about the client.
type sessionCapture struct {
	mu      sync.Mutex
	session server.ClientSession
}

func captureSession(hooks *server.Hooks) *sessionCapture {
	capture := &sessionCapture{}
	hooks.AddOnRegisterSession(func(_ context.Context, s server.ClientSession) {
		capture.mu.Lock()
		defer capture.mu.Unlock()
		capture.session = s
	})
	return capture
}

func (c *sessionCapture) get(t *testing.T) server.ClientSession {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	require.NotNil(t, c.session, "the in-process transport registered no session")
	return c.session
}

// receive waits for one value, failing the test if none arrives in time.
func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a value")
		var zero T
		return zero
	}
}
