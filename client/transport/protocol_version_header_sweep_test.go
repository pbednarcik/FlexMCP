package transport

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// A request that carries its own Mcp-Protocol-Version keeps it: the
// transport's negotiated copy fills in the header only on a request without
// one, so a request of one era is never sent with another's version (a
// probe sets the copy while requests of the era in effect are in flight).
func TestAnExplicitProtocolVersionHeaderWinsOverTheTransportsCopy(t *testing.T) {
	t.Run("streamable HTTP", func(t *testing.T) {
		var mu sync.Mutex
		var seen []string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, r.Header.Get(HeaderKeyProtocolVersion))
			mu.Unlock()
			var request JSONRPCRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(JSONRPCResponse{JSONRPC: mcp.JSONRPC_VERSION, ID: request.ID, Result: json.RawMessage(`{}`)})
		}))
		t.Cleanup(server.Close)
		tr, err := NewStreamableHTTP(server.URL)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tr.Close() })
		require.NoError(t, tr.Start(t.Context()))
		tr.SetProtocolVersion(mcp.LATEST_PROTOCOL_VERSION)

		_, err = tr.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Method: "tools/list",
			Header: http.Header{HeaderKeyProtocolVersion: []string{mcp.LATEST_LEGACY_PROTOCOL_VERSION}}})
		require.NoError(t, err)
		_, err = tr.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(2)), Method: "tools/list"})
		require.NoError(t, err)

		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, []string{mcp.LATEST_LEGACY_PROTOCOL_VERSION, mcp.LATEST_PROTOCOL_VERSION}, seen, "the request's own version, then the transport's copy")
	})
	t.Run("HTTP+SSE", func(t *testing.T) {
		var mu sync.Mutex
		var seen []string
		events := make(chan []byte, 4)
		mux := http.NewServeMux()
		mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			fmt.Fprint(w, "event: endpoint\ndata: /message\n\n")
			flusher.Flush()
			for {
				select {
				case <-r.Context().Done():
					return
				case data := <-events:
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
					flusher.Flush()
				}
			}
		})
		mux.HandleFunc("/message", func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, r.Header.Get(HeaderKeyProtocolVersion))
			mu.Unlock()
			var request JSONRPCRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			data, _ := json.Marshal(JSONRPCResponse{JSONRPC: mcp.JSONRPC_VERSION, ID: request.ID, Result: json.RawMessage(`{}`)})
			events <- data
			w.WriteHeader(http.StatusAccepted)
		})
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		tr, err := NewSSE(server.URL + "/sse")
		require.NoError(t, err)
		t.Cleanup(func() { _ = tr.Close() })
		require.NoError(t, tr.Start(t.Context()))
		tr.SetProtocolVersion(mcp.LATEST_PROTOCOL_VERSION)

		_, err = tr.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Method: "tools/list",
			Header: http.Header{HeaderKeyProtocolVersion: []string{mcp.LATEST_LEGACY_PROTOCOL_VERSION}}})
		require.NoError(t, err)
		_, err = tr.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(2)), Method: "tools/list"})
		require.NoError(t, err)

		mu.Lock()
		defer mu.Unlock()
		assert.Equal(t, []string{mcp.LATEST_LEGACY_PROTOCOL_VERSION, mcp.LATEST_PROTOCOL_VERSION}, seen, "the request's own version, then the transport's copy")
	})
}
