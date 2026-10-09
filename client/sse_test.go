package client

import (
	"context"
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

// legacySSEServer is the smallest HTTP+SSE (2024-11-05) server: GET /sse opens
// the stream and names /message as the endpoint, and every POSTed request is
// answered on the stream. It answers initialize and nothing else, and records
// the headers of every POST.
type legacySSEServer struct {
	*httptest.Server
	stream  chan string
	mu      sync.Mutex
	headers []http.Header
}

func newLegacySSEServer(t *testing.T) *legacySSEServer {
	t.Helper()
	s := &legacySSEServer{stream: make(chan string, 16)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, "event: endpoint\ndata: /message\n\n")
		flusher.Flush()
		for {
			select {
			case data := <-s.stream:
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
				flusher.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
	mux.HandleFunc("POST /message", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.headers = append(s.headers, r.Header.Clone())
		s.mu.Unlock()
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		if len(request.ID) == 0 {
			return
		}
		if request.Method == string(mcp.MethodInitialize) {
			s.stream <- `{"jsonrpc":"2.0","id":` + string(request.ID) + `,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"sse-test","version":"1.0.0"}}}`
			return
		}
		s.stream <- `{"jsonrpc":"2.0","id":` + string(request.ID) + `,"error":{"code":-32601,"message":"Method not found"}}`
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func (s *legacySSEServer) postHeaders() []http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]http.Header(nil), s.headers...)
}

func TestSSEMCPClient(t *testing.T) {
	srv := newLegacySSEServer(t)
	c, err := NewSSEMCPClient(srv.URL+"/sse",
		WithHeaders(map[string]string{"X-Test-Header": "static"}),
		WithHeaderFunc(func(context.Context) map[string]string {
			return map[string]string{"X-Test-Header-Func": "dynamic"}
		}),
	)
	require.NoError(t, err)
	defer c.Close()

	require.NoError(t, c.Start(t.Context()))
	assert.Equal(t, "/message", GetEndpoint(c).Path)

	result, err := c.Initialize(t.Context(), mcp.InitializeRequest{})
	require.NoError(t, err)
	assert.Equal(t, "sse-test", result.ServerInfo.Name)

	headers := srv.postHeaders()
	require.NotEmpty(t, headers)
	for _, h := range headers {
		assert.Equal(t, "static", h.Get("X-Test-Header"))
		assert.Equal(t, "dynamic", h.Get("X-Test-Header-Func"))
	}
}

func TestNewSSEMCPClientRejectsAnInvalidURL(t *testing.T) {
	_, err := NewSSEMCPClient("://not a url")
	require.Error(t, err)
}
