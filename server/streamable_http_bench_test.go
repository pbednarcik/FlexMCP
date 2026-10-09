package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// toolCallRequest is one tools/call the way Claude Code sends it to a
// 2026-07-28 server: modern _meta with a progress token, the protocol
// version, method and name headers. legacy drops the modern parts, which is
// what a client on the handshake era sends after initialize.
func toolCallRequest(b *testing.B, legacy bool) (body []byte, header http.Header) {
	b.Helper()
	params := map[string]any{
		"name":      "echo",
		"arguments": map[string]any{"region": "us-east-1"},
	}
	header = http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"}}
	if !legacy {
		meta := modernMeta()
		meta["progressToken"] = "p-1"
		params["_meta"] = meta
		header.Set(mcp.HeaderProtocolVersion, mcp.ProtocolVersion20260728)
		header.Set(mcp.HeaderMethod, string(mcp.MethodToolsCall))
		name, _ := mcp.EncodeHeaderValue("echo")
		header.Set(mcp.HeaderName, name)
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": mcp.MethodToolsCall, "params": params})
	if err != nil {
		b.Fatal(err)
	}
	return body, header
}

// BenchmarkStreamableHTTPToolCall drives ServeHTTP with one tools/call per
// iteration, no network: the whole per-request cost of the transport and the
// core, which is what Reflex's door pays. Legs: the modern request against a
// small schema; the same with input validation on and an 8 KB schema, the
// door's shape; a legacy request in stateless mode.
func BenchmarkStreamableHTTPToolCall(b *testing.B) {
	echo := func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText(request.GetString("region", "")), nil
	}
	for _, leg := range []struct {
		name    string
		legacy  bool
		schema  json.RawMessage
		options []ServerOption
	}{
		{name: "modern/small", schema: json.RawMessage(`{"type":"object","properties":{"region":{"type":"string"}}}`)},
		{name: "modern/validated-8k", schema: bigRawSchema(8192, false), options: []ServerOption{WithInputSchemaValidation()}},
		{name: "legacy-stateless/small", legacy: true, schema: json.RawMessage(`{"type":"object","properties":{"region":{"type":"string"}}}`)},
	} {
		b.Run(leg.name, func(b *testing.B) {
			srv := NewMCPServer("bench", "1.0.0", append([]ServerOption{WithToolCapabilities(false)}, leg.options...)...)
			srv.AddTool(mcp.NewToolWithRawSchema("echo", "echoes region", leg.schema), echo)
			httpSrv := NewStreamableHTTPServer(srv, WithStateLess(true))
			body, header := toolCallRequest(b, leg.legacy)

			// One call outside the loop proves the leg answers 200 with a result.
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
			r.Header = header
			httpSrv.ServeHTTP(w, r)
			if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"us-east-1"`)) {
				b.Fatalf("leg %s: status %d body %s", leg.name, w.Code, w.Body.String())
			}

			b.ReportAllocs()
			for b.Loop() {
				w := httptest.NewRecorder()
				r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
				r.Header = header
				httpSrv.ServeHTTP(w, r)
			}
		})
	}
}
