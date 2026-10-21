package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readOnceServer serves one tool, echo, and the context a modern HTTP
// tools/call of it carries.
func readOnceServer(t testing.TB) (*MCPServer, context.Context) {
	t.Helper()
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	srv.AddTool(mcp.NewTool("echo"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	name, _ := mcp.EncodeHeaderValue("echo")
	hdr := http.Header{"Mcp-Protocol-Version": {"2026-07-28"}, "Mcp-Method": {"tools/call"}, "Mcp-Name": {name}}
	return srv, context.WithValue(t.Context(), requestHeader, hdr)
}

const readOnceMeta = `"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"c","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}}`

// A modern request is read once, and a malformed one still gets the answer it
// got when each check read the body on its own: these were recorded from that
// code before the single read replaced it, the undecodable-params ones with
// the fixed text that later replaced the decoder's.
func TestHandleMessageReadsOnceWithTheSameAnswers(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"valid", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{},` + readOnceMeta + `}}`,
			`{"jsonrpc":"2.0","id":1,"result":{"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"test","version":"1.0.0"}},"content":[{"type":"text","text":"ok"}],"resultType":"complete"}}`},
		{"params array", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":[1,2]}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"invalid params for tools/call"}}`},
		{"params string", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"x"}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"invalid params for tools/call"}}`},
		{"name a number", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":5,` + readOnceMeta + `}}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32020,"message":"header mismatch: Mcp-Name: failed to extract name from parameters for method \"tools/call\"","data":{"header":"Mcp-Name"}}}`},
		{"name mismatch", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"other",` + readOnceMeta + `}}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32020,"message":"header mismatch: Mcp-Name: header value \"echo\" does not match body value \"other\"","data":{"header":"Mcp-Name"}}}`},
		{"_meta an array", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","_meta":[1]}}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"invalid params for tools/call"}}`},
		{"clientInfo the wrong shape", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":"x","io.modelcontextprotocol/clientCapabilities":{}}}}`,
			`{"jsonrpc":"2.0","id":1,"result":{"_meta":{"io.modelcontextprotocol/serverInfo":{"name":"test","version":"1.0.0"}},"content":[{"type":"text","text":"ok"}],"resultType":"complete"}}`},
		{"logLevel unknown", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/logLevel":"loud","io.modelcontextprotocol/clientCapabilities":{}}}}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"invalid _meta field io.modelcontextprotocol/logLevel"}}`},
		{"clientCapabilities missing", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`,
			`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"missing or invalid _meta field io.modelcontextprotocol/clientCapabilities"}}`},
		{"protocolVersion a number", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","_meta":{"io.modelcontextprotocol/protocolVersion":20260728,"io.modelcontextprotocol/clientCapabilities":{}}}}`,
			`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, ctx := readOnceServer(t)
			got, err := json.Marshal(srv.HandleMessage(ctx, json.RawMessage(tc.body)))
			require.NoError(t, err)
			assert.JSONEq(t, tc.want, string(got))
		})
	}
}

// A modern tools/call reads its body once before dispatch: the protocol
// info, the standard headers and the header bindings no longer decode it again
// each: 66 allocations when each read it on its own, 57 read once, 56 with
// the server identity boxed once, 52 with no span started for a server
// without a tracer, 49 with the id read on the stack and the message boxed
// only for a hook, 47 with the context layers made in one allocation, 48
// once modern decoration stamps a private copy of the handler's result
// rather than the result itself (decorateResult), 35 with _meta read in one
// pass, 33 with the strings a client repeats kept by the decoder, 30 with the
// arguments read in one pass.
func TestHandleMessageModernToolCallAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	srv, ctx := readOnceServer(t)
	msg := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{},` + readOnceMeta + `}}`)
	allocs := testing.AllocsPerRun(1000, func() { srv.HandleMessage(ctx, msg) })
	assert.LessOrEqual(t, allocs, 30.0, "allocations per modern tools/call through HandleMessage")
}

// A tools/call's arguments are read in one pass, the strings a client repeats
// kept by the decoder: on bench/compare's payloads, 38 allocations through
// json.Unmarshal into a map on the small one, 31 in one pass; 602 on the
// complex one, 184 in one pass.
func TestHandleMessageToolCallArgumentsAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	srv, ctx := readOnceServer(t)
	for _, tc := range []struct {
		payload string
		ceiling float64
	}{
		{"modern-small.json", 31},
		{"modern-complex.json", 184},
	} {
		t.Run(tc.payload, func(t *testing.T) {
			msg, err := os.ReadFile(filepath.Join("..", "bench", "compare", "harness", "testdata", tc.payload))
			require.NoError(t, err)
			out, err := json.Marshal(srv.HandleMessage(ctx, json.RawMessage(msg)))
			require.NoError(t, err)
			require.Contains(t, string(out), `"text":"ok"`)
			allocs := testing.AllocsPerRun(1000, func() { srv.HandleMessage(ctx, json.RawMessage(msg)) })
			assert.LessOrEqual(t, allocs, tc.ceiling, "allocations per tools/call of %s through HandleMessage", tc.payload)
		})
	}
}

// Read once, a request's _meta gives the protocol info that reading each key
// on its own gives, nulls included: the single read must not change an era, a
// client identity or an error.
func TestProtocolInfoReadOnceMatchesEachKeyOnItsOwn(t *testing.T) {
	for _, meta := range []string{
		`{}`,
		`{"io.modelcontextprotocol/protocolVersion":"2025-11-25"}`,
		`{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`,
		`{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{"sampling":{}}}`,
		`{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":null}`,
		`{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}`,
		`{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"c","version":"1","title":"T"},"io.modelcontextprotocol/clientCapabilities":{}}`,
		`{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":null,"io.modelcontextprotocol/clientCapabilities":{}}`,
		`{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/logLevel":"debug","io.modelcontextprotocol/clientCapabilities":{}}`,
		`{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/logLevel":null,"io.modelcontextprotocol/clientCapabilities":{}}`,
		`{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/logLevel":"loud","io.modelcontextprotocol/clientCapabilities":{}}`,
		`{"IO.MODELCONTEXTPROTOCOL/PROTOCOLVERSION":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`,
	} {
		t.Run(meta, func(t *testing.T) {
			message := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","_meta":` + meta + `}}`)
			var envelope requestEnvelope
			require.NoError(t, readEnvelope(message, &envelope), "this request is read once")
			once, onceErr := protocolInfoFromMeta(envelope.Params.Meta)
			each, eachErr := extractRequestProtocolInfo(message)
			assert.Equal(t, fmt.Sprint(eachErr), fmt.Sprint(onceErr))
			assert.Equal(t, each, once)
		})
	}
}
