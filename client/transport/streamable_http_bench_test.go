package transport

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
)

// answerRoundTripper answers every request with body, as JSON, without a
// network. length is the answer's ContentLength: 0 as a response built by
// hand leaves it, -1, the unknown length net/http gives a chunked answer, or
// len(body), as net/http states a small answer's length.
type answerRoundTripper struct {
	body   []byte
	length int64
}

func (rt answerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(rt.body)),
		ContentLength: rt.length,
		Request:       r,
	}, nil
}

// eventStreamRoundTripper answers every request with body as an event
// stream, of unknown length as an event stream is, without a network.
type eventStreamRoundTripper struct{ body []byte }

func (rt eventStreamRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": {"text/event-stream"}},
		Body:          io.NopCloser(bytes.NewReader(rt.body)),
		ContentLength: -1,
		Request:       r,
	}, nil
}

// textAnswer is the answer to request 7: a tool result of size bytes of text.
func textAnswer(size int) []byte {
	return fmt.Appendf(nil, `{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":%q}]}}`, strings.Repeat("x", size))
}

// eventStreamAnswer is textAnswer as the one event of an event stream.
func eventStreamAnswer(size int) []byte {
	return fmt.Appendf(nil, "event: message\nid: 1\ndata: %s\n\n", textAnswer(size))
}

// sendRequestEach sends request 7 over a client of its own answered by rt,
// once before the loop and then once per iteration.
func sendRequestEach(b *testing.B, rt http.RoundTripper) {
	c, err := NewStreamableHTTP("http://127.0.0.1:1/mcp", WithHTTPBasicClient(&http.Client{Transport: rt}))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = c.Close() })
	request := JSONRPCRequest{JSONRPC: "2.0", Method: "tools/call", ID: mcp.NewRequestId(int64(7))}
	if _, err := c.SendRequest(b.Context(), request); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.SendRequest(b.Context(), request); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStreamableHTTPSendRequest is the client's own cost of a request
// over Streamable HTTP answered with JSON: building it, net/http's request
// and reading the answer, with no network. parallel sends from every P at
// once, as a client shared by concurrent callers does; their answer leaves
// its length at 0. chunked is serial's answer of unknown length, as net/http
// gives a chunked answer; stated is the same answer with its length stated,
// as net/http states a small one's; and 16384 is an answer of that many bytes
// of text, of unknown length: net/http's server chunks an answer that size
// unless its handler states the length.
func BenchmarkStreamableHTTPSendRequest(b *testing.B) {
	answer := []byte(`{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"ok"}]}}`)
	c, err := NewStreamableHTTP("http://127.0.0.1:1/mcp", WithHTTPBasicClient(&http.Client{
		Transport: answerRoundTripper{body: answer},
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
	b.Run("chunked", func(b *testing.B) {
		sendRequestEach(b, answerRoundTripper{body: answer, length: -1})
	})
	b.Run("stated", func(b *testing.B) {
		sendRequestEach(b, answerRoundTripper{body: answer, length: int64(len(answer))})
	})
	b.Run("16384", func(b *testing.B) {
		sendRequestEach(b, answerRoundTripper{body: textAnswer(16 << 10), length: -1})
	})
}

// BenchmarkStreamableHTTPSSEAnswer is a request over Streamable HTTP
// answered with an event stream of one event, by the result's size: the
// client's cost of sending it, and of reading the stream and decoding the
// answer, with no network.
func BenchmarkStreamableHTTPSSEAnswer(b *testing.B) {
	for _, size := range []int{512, 16 << 10, 256 << 10} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			sendRequestEach(b, eventStreamRoundTripper{body: eventStreamAnswer(size)})
		})
	}
}

// A request costs the client a bounded number of allocations: 39 for an
// answer of no stated length, 40 when the server URL's string was made for
// every request. An answer whose length is stated, as net/http states a
// small one, costs one more, the timer that bounds the read of what is left
// of it on release.
func TestStreamableHTTPSendRequestAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	answer := []byte(`{"jsonrpc":"2.0","id":7,"result":{}}`)
	for _, tc := range []struct {
		name   string
		length int64
		max    float64
	}{
		{"no length stated", 0, 39},
		{"its length stated", int64(len(answer)), 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewStreamableHTTP("http://127.0.0.1:1/mcp", WithHTTPBasicClient(&http.Client{
				Transport: answerRoundTripper{body: answer, length: tc.length},
			}))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			request := JSONRPCRequest{JSONRPC: "2.0", Method: "tools/call", ID: mcp.NewRequestId(int64(7))}
			allocs := fewestAllocs(20, 500, func() {
				if _, err := c.SendRequest(t.Context(), request); err != nil {
					t.Fatal(err)
				}
			})
			if allocs > tc.max {
				t.Errorf("%v allocations per request, want at most %v", allocs, tc.max)
			}
		})
	}
}

// fewestAllocs is the fewest allocations per run of f over rounds rounds of
// testing.AllocsPerRun(runs, f). An unchanged path now and then counts one
// more, for several rounds in a row, so the fewest is its count.
func fewestAllocs(rounds, runs int, f func()) float64 {
	fewest := testing.AllocsPerRun(runs, f)
	for range rounds - 1 {
		fewest = min(fewest, testing.AllocsPerRun(runs, f))
	}
	return fewest
}

// An answer on an event stream costs a bounded number of allocations: 55
// for an empty text and 61 for 16 KiB of it.
func TestStreamableHTTPSSEAnswerAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	for _, tc := range []struct {
		size int
		max  float64
	}{{0, 55}, {16 << 10, 61}} {
		t.Run(fmt.Sprint(tc.size), func(t *testing.T) {
			c, err := NewStreamableHTTP("http://127.0.0.1:1/mcp", WithHTTPBasicClient(&http.Client{
				Transport: eventStreamRoundTripper{body: eventStreamAnswer(tc.size)},
			}))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			request := JSONRPCRequest{JSONRPC: "2.0", Method: "tools/call", ID: mcp.NewRequestId(int64(7))}
			allocs := fewestAllocs(20, 500, func() {
				if _, err := c.SendRequest(t.Context(), request); err != nil {
					t.Fatal(err)
				}
			})
			if allocs > tc.max {
				t.Errorf("%v allocations per request, want at most %v", allocs, tc.max)
			}
		})
	}
}
