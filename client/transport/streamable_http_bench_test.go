package transport

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// answerRoundTripper answers every request with body, as JSON, without a
// network.
type answerRoundTripper struct{ body []byte }

func (rt answerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(rt.body)),
		Request:    r,
	}, nil
}

// BenchmarkStreamableHTTPSendRequest is the client's own cost of a request
// over Streamable HTTP answered with JSON: building it, net/http's request
// and reading the answer, with no network. parallel sends from every P at
// once, as a client shared by concurrent callers does.
func BenchmarkStreamableHTTPSendRequest(b *testing.B) {
	c, err := NewStreamableHTTP("http://127.0.0.1:1/mcp", WithHTTPBasicClient(&http.Client{
		Transport: answerRoundTripper{body: []byte(`{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"ok"}]}}`)},
	}))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = c.Close() })
	request := JSONRPCRequest{JSONRPC: "2.0", Method: "tools/call", ID: mcp.NewRequestId(int64(7))}
	if _, err := c.SendRequest(b.Context(), request); err != nil {
		b.Fatal(err)
	}
	b.Run("serial", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := c.SendRequest(b.Context(), request); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("parallel", func(b *testing.B) {
		ctx := b.Context()
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if _, err := c.SendRequest(ctx, request); err != nil {
					b.Error(err)
					return
				}
			}
		})
	})
}
