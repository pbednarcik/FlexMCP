package client

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// encodingTransport answers every request with result after encoding it as
// a real transport does, so a benchmark pays for the client's request
// building and the wire encoding, and nothing else.
type encodingTransport struct {
	result json.RawMessage
}

func (t *encodingTransport) Start(context.Context) error { return nil }

func (t *encodingTransport) SendRequest(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	if _, err := json.Marshal(request); err != nil {
		return nil, err
	}
	return &transport.JSONRPCResponse{JSONRPC: "2.0", ID: request.ID, Result: t.result}, nil
}

func (t *encodingTransport) SendNotification(context.Context, mcp.JSONRPCNotification) error {
	return nil
}
func (t *encodingTransport) SetNotificationHandler(func(mcp.JSONRPCNotification)) {}
func (t *encodingTransport) Close() error                                         { return nil }
func (t *encodingTransport) GetSessionId() string                                 { return "" }

// benchmarkClient is a client on version, as Initialize leaves one.
func benchmarkClient(b *testing.B, version string) *Client {
	b.Helper()
	c := NewClient(&encodingTransport{result: json.RawMessage(`{"content":[{"type":"text","text":"ok"}],"resultType":"complete"}`)})
	c.updateEra(func(e *era) { e.version = version })
	c.initialized.Store(true)
	return c
}

func benchmarkCall() mcp.CallToolRequest {
	var request mcp.CallToolRequest
	request.Params.Name = "echo"
	request.Params.Arguments = map[string]any{"message": "hello", "n": 3}
	return request
}

func benchmarkCallTool(b *testing.B, version string, request mcp.CallToolRequest) {
	c := benchmarkClient(b, version)
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.CallTool(ctx, request); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCallToolModern is a tools/call on protocol version 2026-07-28,
// whose every request carries the client's _meta.
func BenchmarkCallToolModern(b *testing.B) {
	benchmarkCallTool(b, mcp.LATEST_PROTOCOL_VERSION, benchmarkCall())
}

// BenchmarkCallToolModernWithMeta is the same call carrying _meta of its
// own, which the client's entries are merged into.
func BenchmarkCallToolModernWithMeta(b *testing.B) {
	request := benchmarkCall()
	request.Params.Meta = &mcp.Meta{ProgressToken: mcp.ProgressToken(int64(7))}
	benchmarkCallTool(b, mcp.LATEST_PROTOCOL_VERSION, request)
}

// BenchmarkCallToolLegacy is a tools/call on the handshake era, which adds
// no _meta.
func BenchmarkCallToolLegacy(b *testing.B) {
	benchmarkCallTool(b, mcp.LATEST_LEGACY_PROTOCOL_VERSION, benchmarkCall())
}
