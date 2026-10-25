package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// A listen stream carries an SSE comment line as a keep-alive at the
// interval WithKeepAlive sets (Transports.md row 40): a proxy with an idle
// timeout does not cut a quiet stream.
func TestStreamableHTTP_AListenStreamCarriesKeepAlives(t *testing.T) {
	mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(true))
	httpServer := httptest.NewServer(NewStreamableHTTPServer(mcpServer, WithKeepAlive(20*time.Millisecond)))
	defer httpServer.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  mcp.MethodSubscriptionsListen,
		"params": map[string]any{
			"notifications": map[string]any{"toolsListChanged": true},
			"_meta":         modernMeta(),
		},
	})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(mcp.HeaderProtocolVersion, mcp.ProtocolVersion20260728)
	req.Header.Set(mcp.HeaderMethod, string(mcp.MethodSubscriptionsListen))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	comments := make(chan string, 8)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if line := scanner.Text(); strings.HasPrefix(line, ":") {
				select {
				case comments <- line:
				default:
				}
			}
		}
	}()
	for range 2 {
		select {
		case <-comments:
		case <-time.After(3 * time.Second):
			t.Fatal("no keep-alive arrived on the quiet listen stream")
		}
	}
}
