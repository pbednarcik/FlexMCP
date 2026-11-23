package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// A message whose member names the core refuses is no subscriptions/listen:
// the transport opens no stream for it, so even behind a writer that cannot
// stream it gets the core's -32600, not the transport's 500.
func TestStreamableHTTP_ListenWithRefusedMemberNamesIsNoStream(t *testing.T) {
	meta, err := json.Marshal(modernMeta())
	require.NoError(t, err)
	params := `{"notifications":{"toolsListChanged":true},"_meta":` + string(meta) + `}`
	cases := map[string]string{
		"folded method":    `{"jsonrpc":"2.0","id":2,"Method":"subscriptions/listen","params":` + params + `}`,
		"duplicate method": `{"jsonrpc":"2.0","id":2,"method":"tools/list","method":"subscriptions/listen","params":` + params + `}`,
		"folded id":        `{"jsonrpc":"2.0","Id":2,"method":"subscriptions/listen","params":` + params + `}`,
		"folded params":    `{"jsonrpc":"2.0","id":2,"method":"subscriptions/listen","Params":` + params + `}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			mcpServer := NewMCPServer("modern-test", "1.0.0", WithToolCapabilities(true))
			handler := NewStreamableHTTPServer(mcpServer)
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handler.ServeHTTP(&hiddenFlusherWriter{ResponseWriter: w}, r)
			}))
			defer httpServer.Close()

			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL, bytes.NewReader([]byte(body)))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set(mcp.HeaderProtocolVersion, mcp.ProtocolVersion20260728)
			req.Header.Set(mcp.HeaderMethod, string(mcp.MethodSubscriptionsListen))
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
			var message mcp.JSONRPCError
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&message))
			assert.Equal(t, mcp.INVALID_REQUEST, message.Error.Code)
		})
	}
}
