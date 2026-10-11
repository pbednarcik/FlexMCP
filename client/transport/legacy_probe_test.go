package transport

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A 4xx to initialize means a legacy HTTP+SSE server only when its body is
// not a JSON-RPC error. A modern-only server answers initialize with an
// error naming the versions it supports, and that answer reaches the caller.
func TestStreamableHTTP_InitializeRefusedWithAJSONRPCBodyIsNotALegacyServer(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   bool
	}{
		{name: "404 with a JSON-RPC error", status: http.StatusNotFound, body: true},
		{name: "405 with a JSON-RPC error", status: http.StatusMethodNotAllowed, body: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"error":{"code":%d,"message":"initialize is not served","data":{"supported":[%q]}}}`, mcp.METHOD_NOT_FOUND, mcp.ProtocolVersion20260728)
			}))
			defer server.Close()
			trans, err := NewStreamableHTTP(server.URL)
			require.NoError(t, err)

			response, err := trans.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Method: string(mcp.MethodInitialize)})
			require.NoError(t, err)
			require.NotNil(t, response.Error)
			assert.Equal(t, mcp.METHOD_NOT_FOUND, response.Error.Code)
		})
	}

	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		t.Run(fmt.Sprintf("%d without a body is a legacy server", status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			}))
			defer server.Close()
			trans, err := NewStreamableHTTP(server.URL)
			require.NoError(t, err)

			_, err = trans.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Method: string(mcp.MethodInitialize)})
			assert.ErrorIs(t, err, ErrLegacySSEServer)
		})
	}
}

// A failed initialize leaves the transport as it was: not initialized, and
// without the session ID the error response may carry.
func TestStreamableHTTP_AFailedInitializeInitializesNothing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(HeaderKeySessionID, "never-started")
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"error":{"code":%d,"message":"no"}}`, mcp.INVALID_REQUEST)
	}))
	defer server.Close()
	trans, err := NewStreamableHTTP(server.URL)
	require.NoError(t, err)

	response, err := trans.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Method: string(mcp.MethodInitialize)})
	require.NoError(t, err)
	require.NotNil(t, response.Error)

	assert.Empty(t, trans.GetSessionId())
	select {
	case <-trans.initialized:
		t.Fatal("a failed initialize marked the transport initialized")
	default:
	}
}
