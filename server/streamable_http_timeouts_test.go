package server

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The http.Server Start builds has timeouts: a client that never finishes
// its headers, or keeps an idle connection, does not hold a goroutine or a
// connection for good.
func TestStreamableHTTP_StartSetsServerTimeouts(t *testing.T) {
	httpServer := NewStreamableHTTPServer(NewMCPServer("test", "1.0.0"))
	go func() { _ = httpServer.Start("127.0.0.1:0") }()
	require.Eventually(t, func() bool {
		httpServer.mu.RLock()
		defer httpServer.mu.RUnlock()
		return httpServer.httpServer != nil
	}, time.Second, 10*time.Millisecond)
	httpServer.mu.RLock()
	srv := httpServer.httpServer
	httpServer.mu.RUnlock()

	assert.Equal(t, DefaultReadHeaderTimeout, srv.ReadHeaderTimeout)
	assert.Equal(t, DefaultIdleTimeout, srv.IdleTimeout)

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	require.NoError(t, httpServer.Shutdown(ctx))
}

// dripBody sends a POST whose body stops arriving after a few bytes and
// returns the response, if one comes within a few seconds.
func dripBody(t *testing.T, addr string) (*http.Response, error) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	_, err = fmt.Fprint(conn, "POST /mcp HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/json\r\n"+
		"Accept: application/json, text/event-stream\r\nContent-Length: 100\r\n\r\n{\"jsonrpc\"")
	require.NoError(t, err)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err == nil {
		t.Cleanup(func() { resp.Body.Close() })
	}
	return resp, err
}

// A body that arrives slower than WithRequestBodyTimeout allows is refused:
// the bound on a body's size does not bound how long reading it holds the
// handler and its connection.
func TestStreamableHTTP_SlowBodyIsRefused(t *testing.T) {
	httpServer := NewStreamableHTTPServer(NewMCPServer("test", "1.0.0"),
		WithRequestBodyTimeout(200*time.Millisecond))
	ts := httptest.NewServer(httpServer)
	defer ts.Close()

	resp, err := dripBody(t, ts.Listener.Addr().String())
	require.NoError(t, err, "the server never answered a dripped body")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// A server with its own ReadTimeout keeps it: the body deadline neither
// replaces the deployer's bound with a looser one nor lifts it.
func TestStreamableHTTP_AServersOwnReadTimeoutIsKept(t *testing.T) {
	httpServer := NewStreamableHTTPServer(NewMCPServer("test", "1.0.0"))
	ts := httptest.NewUnstartedServer(httpServer)
	ts.Config.ReadTimeout = 200 * time.Millisecond
	ts.Start()
	defer ts.Close()

	resp, err := dripBody(t, ts.Listener.Addr().String())
	require.NoError(t, err, "the body deadline replaced the server's own ReadTimeout")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}
