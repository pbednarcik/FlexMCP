package transport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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

// sizedLoopback is sseLoopback answering every request with a result of a
// fixed size, built without decoding the request.
type sizedLoopback struct {
	mu     sync.Mutex
	stream *io.PipeWriter
	suffix []byte
	in     bytes.Buffer
	out    []byte
}

func (l *sizedLoopback) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodGet {
		body, stream := io.Pipe()
		l.mu.Lock()
		l.stream = stream
		l.mu.Unlock()
		go func() { _, _ = io.WriteString(stream, "event: endpoint\ndata: /message\n\n") }()
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body, Request: r}, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.in.Reset()
	_, _ = l.in.ReadFrom(r.Body)
	_ = r.Body.Close()
	if _, id, ok := bytes.Cut(l.in.Bytes(), []byte(`"id":`)); ok {
		id = id[:bytes.IndexAny(id, ",}")]
		l.out = append(append(append(l.out[:0], "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":"...), id...), l.suffix...)
		_, _ = l.stream.Write(l.out)
	}
	return &http.Response{StatusCode: http.StatusAccepted, Header: http.Header{}, Body: http.NoBody, Request: r}, nil
}

// BenchmarkSSERoundTrip is a request and its answer, by the result's size:
// the client's cost of posting it, and of reading, decoding and matching the
// answer from the stream.
func BenchmarkSSERoundTrip(b *testing.B) {
	for _, size := range []int{512, 16 << 10, 256 << 10} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			loopback := &sizedLoopback{suffix: fmt.Appendf(nil, `,"result":{"content":[{"type":"text","text":%q}]}}`+"\n\n", strings.Repeat("x", size))}
			c, err := NewSSE("http://sse.test/sse", WithHTTPClient(&http.Client{Transport: loopback}))
			if err != nil {
				b.Fatal(err)
			}
			if err := c.Start(b.Context()); err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = c.Close() })
			request := JSONRPCRequest{JSONRPC: "2.0", Method: "ping"}
			var id int64
			b.ReportAllocs()
			for b.Loop() {
				id++
				request.ID = mcp.NewRequestId(id)
				if _, err := c.SendRequest(b.Context(), request); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSSEStreamNotification is a notification arriving on the stream,
// by its size: the client's cost of reading and decoding it.
func BenchmarkSSEStreamNotification(b *testing.B) {
	for _, size := range []int{512, 16 << 10} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			body, stream := io.Pipe()
			c, err := NewSSE("http://sse.test/sse")
			if err != nil {
				b.Fatal(err)
			}
			got := make(chan struct{}, 1)
			c.SetNotificationHandler(func(mcp.JSONRPCNotification) { got <- struct{}{} })
			done := make(chan struct{})
			go func() { c.readSSE(body, make(chan struct{}), &sync.Once{}, 0); close(done) }()
			event := fmt.Appendf(nil, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/message\",\"params\":{\"level\":\"info\",\"data\":%q}}\n\n", strings.Repeat("x", size))
			b.ReportAllocs()
			for b.Loop() {
				_, _ = stream.Write(event)
				<-got
			}
			c.closed.Store(true)
			_ = stream.Close()
			<-done
		})
	}
}
