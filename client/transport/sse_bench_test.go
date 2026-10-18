package transport

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// sseLoopback is a legacy HTTP+SSE server in memory: the GET's body is a
// pipe that carries the endpoint event and then the answer to every POSTed
// request, so the figures are the transport's and net/http's request
// building, not the network's.
type sseLoopback struct {
	mu     sync.Mutex
	stream *io.PipeWriter
}

func (l *sseLoopback) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodGet {
		body, stream := io.Pipe()
		l.mu.Lock()
		l.stream = stream
		l.mu.Unlock()
		go func() { _, _ = io.WriteString(stream, "event: endpoint\ndata: /message\n\n") }()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body, Request: r}, nil
	}
	var message struct {
		ID json.RawMessage `json:"id"`
	}
	encoded, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	_ = json.Unmarshal(encoded, &message)
	if len(message.ID) > 0 {
		l.mu.Lock()
		stream := l.stream
		l.mu.Unlock()
		_, _ = fmt.Fprintf(stream, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{}}\n\n", message.ID)
	}
	return &http.Response{StatusCode: http.StatusAccepted, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
}

func startedLoopbackSSE(b *testing.B) *SSE {
	b.Helper()
	c, err := NewSSE("http://sse.test/sse", WithHTTPClient(&http.Client{Transport: &sseLoopback{}}))
	if err != nil {
		b.Fatal(err)
	}
	if err := c.Start(b.Context()); err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = c.Close() })
	return c
}

// BenchmarkSSESendRequest is one request and its answer on the stream.
func BenchmarkSSESendRequest(b *testing.B) {
	c := startedLoopbackSSE(b)
	request := JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(7)), Method: "tools/list"}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.SendRequest(b.Context(), request); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSSESendNotification is one notification POSTed.
func BenchmarkSSESendNotification(b *testing.B) {
	c := startedLoopbackSSE(b)
	notification := mcp.JSONRPCNotification{JSONRPC: mcp.JSONRPC_VERSION, Method: "notifications/initialized"}
	b.ReportAllocs()
	for b.Loop() {
		if err := c.SendNotification(b.Context(), notification); err != nil {
			b.Fatal(err)
		}
	}
}
