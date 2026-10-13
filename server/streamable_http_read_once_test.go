package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
)

// readOnceHTTP serves one tool, echo, over the streamable transport, and the
// headers of a modern or a legacy tools/call of it.
func readOnceHTTP(t testing.TB) (http.Handler, func(modern bool) http.Header) {
	t.Helper()
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	srv.AddTool(mcp.NewTool("echo"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	name, _ := mcp.EncodeHeaderValue("echo")
	header := func(modern bool) http.Header {
		h := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"}}
		if modern {
			h.Set("Mcp-Protocol-Version", "2026-07-28")
			h.Set("Mcp-Method", "tools/call")
			h.Set("Mcp-Name", name)
		} else {
			h.Set("Mcp-Protocol-Version", "2025-11-25")
		}
		return h
	}
	return NewStreamableHTTPServer(srv), header
}

// A POST is read once on its way to dispatch, and an awkward body still gets
// the answer it got when the transport and the core each read it: these were
// recorded from that code before the single read replaced it.
func TestStreamableHTTPReadsOnceWithTheSameAnswers(t *testing.T) {
	const meta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"c","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}`
	for _, tc := range []struct {
		name, body  string
		modern      bool
		status      int
		contentType string
		want        string
	}{
		{"valid", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo",` + meta + `}}`, true, 200, "application/json",
			`{"jsonrpc":"2.0","id":1,"result":{"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"test","version":"1.0.0"}},"content":[{"type":"text","text":"ok"}],"resultType":"complete"}}`},
		{"params an array", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":[1]}`, true, 400, "application/json",
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"missing or invalid _meta field io.modelcontextprotocol/protocolVersion"}}`},
		{"protocolVersion a number", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","_meta":{"io.modelcontextprotocol/protocolVersion":5}}}`, true, 400, "application/json",
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"missing or invalid _meta field io.modelcontextprotocol/protocolVersion"}}`},
		{"id an object", `{"jsonrpc":"2.0","id":{},"method":"tools/call","params":{"name":"echo",` + meta + `}}`, true, 400, "application/json",
			`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"invalid request id"}}`},
		{"method a number", `{"jsonrpc":"2.0","id":1,"method":5}`, true, 400, "application/json",
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"request body is not a JSON-RPC message"}}`},
		{"not json", `{"jsonrpc":`, true, 400, "application/json",
			`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"request body is not valid json"}}`},
		{"an array", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, true, 400, "application/json",
			`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"request body is not a JSON-RPC message"}}`},
		{"a response", `{"jsonrpc":"2.0","id":1,"result":{}}`, true, 400, "application/json",
			`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"a client sends no responses on this protocol revision"}}`},
		{"legacy valid", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`, false, 200, "application/json",
			`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`},
		{"legacy params an array", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":[1]}`, false, 200, "application/json",
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"unparsable tools/call request: json: cannot unmarshal array into Go value of type mcp.params"}}`},
		{"legacy response", `{"jsonrpc":"2.0","id":1,"result":{}}`, false, 202, "", ``},
		{"legacy notification", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, false, 202, "", ``},
		{"modern _meta under a legacy header", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo",` + meta + `}}`, false, 400, "application/json",
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32020,"message":"Mcp-Protocol-Version header 2025-11-25 does not match request io.modelcontextprotocol/protocolVersion 2026-07-28"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, header := readOnceHTTP(t)
			r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(tc.body)))
			r.Header = header(tc.modern)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			assert.Equal(t, tc.status, w.Code)
			assert.Equal(t, tc.contentType, w.Header().Get("Content-Type"))
			if tc.want == "" {
				assert.Empty(t, bytes.TrimSpace(w.Body.Bytes()))
				return
			}
			assert.JSONEq(t, tc.want, w.Body.String())
		})
	}
}

// A modern tools/call over HTTP is read once: the transport's read serves the
// core, which no longer reads it again. 97 allocations in the server when
// the transport and the core each read it, 93 read once, 82 with no span
// started for a server without a tracer.
func TestStreamableHTTPModernToolCallAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	h, header := readOnceHTTP(t)
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"c","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}}}`)
	hdr := header(true)
	request := func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
		r.Header = hdr
		return r
	}
	harness := testing.AllocsPerRun(1000, func() { _, _ = request(), httptest.NewRecorder() })
	total := testing.AllocsPerRun(1000, func() { h.ServeHTTP(httptest.NewRecorder(), request()) })
	assert.LessOrEqual(t, total-harness, 82.0, "allocations in the server per modern tools/call over HTTP")
}
