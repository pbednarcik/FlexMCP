package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// legacySSEServer is the smallest HTTP+SSE (2024-11-05) server: GET /sse opens
// the stream and names /message as the endpoint, and every POSTed request is
// answered on the stream. It answers initialize and nothing else, and records
// the headers of every POST and the Host of every request. A request for a
// silent method is never answered. Every POSTed message is recorded.
type legacySSEServer struct {
	*httptest.Server
	stream   chan string
	silent   []string
	mu       sync.Mutex
	headers  []http.Header
	hosts    []string
	messages []postedMessage
}

// postedMessage is a message a client POSTed.
type postedMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func newLegacySSEServer(t *testing.T, silent ...string) *legacySSEServer {
	t.Helper()
	s := &legacySSEServer{stream: make(chan string, 16), silent: silent}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sse", func(w http.ResponseWriter, r *http.Request) {
		s.recordHost(r)
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
		s.recordHost(r)
		s.mu.Lock()
		s.headers = append(s.headers, r.Header.Clone())
		s.mu.Unlock()
		var request postedMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		s.messages = append(s.messages, request)
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		if len(request.ID) == 0 || slices.Contains(s.silent, request.Method) {
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

func (s *legacySSEServer) recordHost(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hosts = append(s.hosts, r.Host)
}

func (s *legacySSEServer) postHeaders() []http.Header {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]http.Header(nil), s.headers...)
}

func (s *legacySSEServer) postedMessages() []postedMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]postedMessage(nil), s.messages...)
}

func (s *legacySSEServer) requestHosts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.hosts...)
}

// countingRoundTripper counts the requests an http.Client sends through it.
type countingRoundTripper struct {
	calls atomic.Int32
}

func (c *countingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}

func TestSSEClientOptionsShapeEveryRequest(t *testing.T) {
	srv := newLegacySSEServer(t)
	roundTripper := &countingRoundTripper{}
	c, err := NewSSEMCPClient(srv.URL+"/sse",
		WithHTTPClient(&http.Client{Transport: roundTripper}),
		WithHTTPHost("mcp.example.test"),
	)
	require.NoError(t, err)
	defer c.Close()

	require.NoError(t, c.Start(t.Context()))
	_, err = c.Initialize(t.Context(), mcp.InitializeRequest{})
	require.NoError(t, err)

	hosts := srv.requestHosts()
	require.NotEmpty(t, hosts)
	for _, host := range hosts {
		assert.Equal(t, "mcp.example.test", host)
	}
	assert.Equal(t, int32(len(hosts)), roundTripper.calls.Load(), "every request goes through the given http.Client")
}

func TestOnConnectionLostFiresWhenTheStreamDrops(t *testing.T) {
	srv := newLegacySSEServer(t)
	c, err := NewSSEMCPClient(srv.URL + "/sse")
	require.NoError(t, err)
	defer c.Close()
	lost := make(chan error, 1)
	c.OnConnectionLost(func(err error) { lost <- err })
	require.NoError(t, c.Start(t.Context()))

	srv.CloseClientConnections()

	require.Error(t, receive(t, lost))
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
