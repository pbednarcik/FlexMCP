package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// TestRequestProtocolInfoReadsMetaByItsKeys pins how a request's _meta is
// read: the four well-known keys decide the era and the identity, each on
// its own. Malformed client info is ignored; the capabilities are the one
// value a modern request cannot do without; a log level that is not one of
// the eight is refused (Utilities.md row 11), and so is an extension whose
// settings are not an object (Lifecycle.md row 9).
func TestRequestProtocolInfoReadsMetaByItsKeys(t *testing.T) {
	const modern = `"io.modelcontextprotocol/protocolVersion":"2026-07-28"`
	const caps = `"io.modelcontextprotocol/clientCapabilities":{}`

	tests := []struct {
		name    string
		params  string
		want    *RequestProtocolInfo
		wantErr string
	}{
		{"no _meta is legacy", `{}`, &RequestProtocolInfo{}, ""},
		{"_meta null is legacy", `{"_meta":null}`, &RequestProtocolInfo{}, ""},
		{"_meta that is not an object is legacy", `{"_meta":"x"}`, &RequestProtocolInfo{}, ""},
		{"a handshake-era version is legacy and kept", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25"}}`,
			&RequestProtocolInfo{ProtocolVersion: "2025-11-25"}, ""},
		{"a version that is not a string is legacy", `{"_meta":{"io.modelcontextprotocol/protocolVersion":2026}}`, &RequestProtocolInfo{}, ""},
		{"modern with empty capabilities", `{"_meta":{` + modern + `,` + caps + `}}`,
			&RequestProtocolInfo{Modern: true, ProtocolVersion: "2026-07-28", ClientCapabilities: &mcp.ClientCapabilities{}}, ""},
		{"modern with client info and log level", `{"_meta":{` + modern + `,` + caps + `,"io.modelcontextprotocol/clientInfo":{"name":"c","version":"1"},"io.modelcontextprotocol/logLevel":"debug"}}`,
			&RequestProtocolInfo{Modern: true, ProtocolVersion: "2026-07-28", ClientCapabilities: &mcp.ClientCapabilities{},
				ClientInfo: &mcp.Implementation{Name: "c", Version: "1"}, LogLevel: mcp.LoggingLevelDebug}, ""},
		{"malformed client info is ignored", `{"_meta":{` + modern + `,` + caps + `,"io.modelcontextprotocol/clientInfo":"c"}}`,
			&RequestProtocolInfo{Modern: true, ProtocolVersion: "2026-07-28", ClientCapabilities: &mcp.ClientCapabilities{}}, ""},
		{"a log level that is not a string is an error", `{"_meta":{` + modern + `,` + caps + `,"io.modelcontextprotocol/logLevel":5}}`,
			nil, "invalid _meta field io.modelcontextprotocol/logLevel"},
		{"an unknown log level is an error", `{"_meta":{` + modern + `,` + caps + `,"io.modelcontextprotocol/logLevel":"loud"}}`,
			nil, "invalid _meta field io.modelcontextprotocol/logLevel"},
		{"key case is not significant, as encoding/json matches field tags", `{"_meta":{"IO.modelcontextprotocol/protocolversion":"2026-07-28","io.modelcontextprotocol/CLIENTCAPABILITIES":{}}}`,
			&RequestProtocolInfo{Modern: true, ProtocolVersion: "2026-07-28", ClientCapabilities: &mcp.ClientCapabilities{}}, ""},
		{"modern without capabilities is an error", `{"_meta":{` + modern + `}}`, nil, "missing or invalid _meta field io.modelcontextprotocol/clientCapabilities"},
		{"modern with null capabilities is an error", `{"_meta":{` + modern + `,"io.modelcontextprotocol/clientCapabilities":null}}`, nil, "missing or invalid _meta field io.modelcontextprotocol/clientCapabilities"},
		{"modern with malformed capabilities is an error", `{"_meta":{` + modern + `,"io.modelcontextprotocol/clientCapabilities":5}}`, nil, "missing or invalid _meta field io.modelcontextprotocol/clientCapabilities"},
		{"an extension with empty settings", `{"_meta":{` + modern + `,"io.modelcontextprotocol/clientCapabilities":{"extensions":{"example.com/ext":{}}}}}`,
			&RequestProtocolInfo{Modern: true, ProtocolVersion: "2026-07-28", ClientCapabilities: &mcp.ClientCapabilities{Extensions: map[string]any{"example.com/ext": map[string]any{}}}}, ""},
		{"an extension with settings", `{"_meta":{` + modern + `,"io.modelcontextprotocol/clientCapabilities":{"extensions":{"example.com/ext":{"mode":"x"}}}}}`,
			&RequestProtocolInfo{Modern: true, ProtocolVersion: "2026-07-28", ClientCapabilities: &mcp.ClientCapabilities{Extensions: map[string]any{"example.com/ext": map[string]any{"mode": "x"}}}}, ""},
		{"extension settings null is an error", `{"_meta":{` + modern + `,"io.modelcontextprotocol/clientCapabilities":{"extensions":{"example.com/ext":null}}}}`,
			nil, `invalid _meta field io.modelcontextprotocol/clientCapabilities: extension "example.com/ext" settings are not an object`},
		{"extension settings false is an error", `{"_meta":{` + modern + `,"io.modelcontextprotocol/clientCapabilities":{"extensions":{"example.com/ext":false}}}}`,
			nil, `invalid _meta field io.modelcontextprotocol/clientCapabilities: extension "example.com/ext" settings are not an object`},
		{"extension settings a string is an error", `{"_meta":{` + modern + `,"io.modelcontextprotocol/clientCapabilities":{"extensions":{"example.com/ext":"on"}}}}`,
			nil, `invalid _meta field io.modelcontextprotocol/clientCapabilities: extension "example.com/ext" settings are not an object`},
		{"extension settings a number is an error", `{"_meta":{` + modern + `,"io.modelcontextprotocol/clientCapabilities":{"extensions":{"example.com/ext":1}}}}`,
			nil, `invalid _meta field io.modelcontextprotocol/clientCapabilities: extension "example.com/ext" settings are not an object`},
		{"extension settings an array is an error", `{"_meta":{` + modern + `,"io.modelcontextprotocol/clientCapabilities":{"extensions":{"example.com/ext":[]}}}}`,
			nil, `invalid _meta field io.modelcontextprotocol/clientCapabilities: extension "example.com/ext" settings are not an object`},
		{"of several malformed extensions the first by id is named", `{"_meta":{` + modern + `,"io.modelcontextprotocol/clientCapabilities":{"extensions":{"example.com/z":null,"example.com/ok":{},"example.com/b":1,"example.com/c":[]}}}}`,
			nil, `invalid _meta field io.modelcontextprotocol/clientCapabilities: extension "example.com/b" settings are not an object`},
		{"an empty extension id is named too", `{"_meta":{` + modern + `,"io.modelcontextprotocol/clientCapabilities":{"extensions":{"":null,"example.com/b":1}}}}`,
			nil, `invalid _meta field io.modelcontextprotocol/clientCapabilities: extension "" settings are not an object`},
		{"a legacy request's extensions are not read", `{"_meta":{"io.modelcontextprotocol/clientCapabilities":{"extensions":{"example.com/ext":null}}}}`,
			&RequestProtocolInfo{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			message := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":` + tt.params + `}`)

			info, err := extractRequestProtocolInfo(message)

			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				assert.Nil(t, info)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, info)
		})
	}
}

// TestClientDeclaresExtension pins the per-request extension check: a modern
// request declares an extension under clientCapabilities.extensions, on its
// own and for itself (Lifecycle.md rows 8 and 9); a legacy request never
// does, as extensions arrived with protocol version 2026-07-28.
func TestClientDeclaresExtension(t *testing.T) {
	const id = "example.com/ext"
	declaring := &mcp.ClientCapabilities{Extensions: map[string]any{id: map[string]any{}}}

	tests := []struct {
		name string
		info *RequestProtocolInfo
		want bool
	}{
		{"no protocol info", nil, false},
		{"a modern request that declares it", &RequestProtocolInfo{Modern: true, ClientCapabilities: declaring}, true},
		{"a modern request that declares another", &RequestProtocolInfo{Modern: true, ClientCapabilities: &mcp.ClientCapabilities{Extensions: map[string]any{"example.com/other": map[string]any{}}}}, false},
		{"a modern request with no extensions", &RequestProtocolInfo{Modern: true, ClientCapabilities: &mcp.ClientCapabilities{}}, false},
		{"a modern request with no capabilities", &RequestProtocolInfo{Modern: true}, false},
		{"a legacy request carrying the key", &RequestProtocolInfo{ClientCapabilities: declaring}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			if tt.info != nil {
				ctx = WithRequestProtocolInfo(ctx, tt.info)
			}
			assert.Equal(t, tt.want, ClientDeclaresExtension(ctx, id))
		})
	}

	t.Run("a tool handler sees the request's own declaration", func(t *testing.T) {
		mcpServer := NewMCPServer("extension-test", "1.0.0", WithToolCapabilities(true))
		mcpServer.AddTool(mcp.NewTool("declared"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(strconv.FormatBool(ClientDeclaresExtension(ctx, id))), nil
		})
		httpServer := httptest.NewServer(NewStreamableHTTPServer(mcpServer))
		t.Cleanup(httpServer.Close)

		call := func(t *testing.T, capabilities map[string]any) string {
			t.Helper()
			meta := modernMeta()
			meta[mcp.MetaKeyClientCapabilities] = capabilities
			message := decodeJSONRPC(t, postModernWithMeta(t, httpServer.URL, mcp.MethodToolsCall, map[string]any{"name": "declared"}, meta))
			result, ok := message["result"].(map[string]any)
			require.True(t, ok, "expected a result, got %v", message)
			content, ok := result["content"].([]any)
			require.True(t, ok, "expected content, got %v", result)
			require.Len(t, content, 1)
			text, ok := content[0].(map[string]any)
			require.True(t, ok)
			return text["text"].(string)
		}
		assert.Equal(t, "true", call(t, map[string]any{"extensions": map[string]any{id: map[string]any{}}}))
		assert.Equal(t, "false", call(t, map[string]any{}))
	})
}

// TestMalformedExtensionSettingsAreInvalidParams pins, over HTTP and so
// through the single read of the body, that a modern request declaring an
// extension with settings that are not an object is refused as invalid params
// before any handler runs (Lifecycle.md row 9; a malformed request is -32602,
// status 400 on HTTP).
func TestMalformedExtensionSettingsAreInvalidParams(t *testing.T) {
	const id = "example.com/ext"
	mcpServer := NewMCPServer("extension-test", "1.0.0", WithToolCapabilities(true))
	mcpServer.AddTool(mcp.NewTool("declared"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText(strconv.FormatBool(ClientDeclaresExtension(ctx, id))), nil
	})
	httpServer := httptest.NewServer(NewStreamableHTTPServer(mcpServer))
	t.Cleanup(httpServer.Close)

	tests := []struct {
		name     string
		settings any
		want     string
	}{
		{"empty settings", map[string]any{}, "true"},
		{"settings", map[string]any{"mode": "x"}, "true"},
		{"null", nil, ""},
		{"false", false, ""},
		{"a string", "on", ""},
		{"a number", 1, ""},
		{"an array", []any{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := modernMeta()
			meta[mcp.MetaKeyClientCapabilities] = map[string]any{"extensions": map[string]any{id: tt.settings}}
			resp := postModernWithMeta(t, httpServer.URL, mcp.MethodToolsCall, map[string]any{"name": "declared"}, meta)
			message := decodeJSONRPC(t, resp)

			if tt.want == "" {
				assert.Equal(t, 400, resp.StatusCode)
				rpcErr, ok := message["error"].(map[string]any)
				require.True(t, ok, "expected an error, got %v", message)
				assert.InDelta(t, float64(mcp.INVALID_PARAMS), rpcErr["code"], 0)
				assert.Equal(t, `invalid _meta field io.modelcontextprotocol/clientCapabilities: extension "example.com/ext" settings are not an object`, rpcErr["message"])
				return
			}
			assert.Equal(t, 200, resp.StatusCode)
			result, ok := message["result"].(map[string]any)
			require.True(t, ok, "expected a result, got %v", message)
			content, ok := result["content"].([]any)
			require.True(t, ok, "expected content, got %v", result)
			require.Len(t, content, 1)
			text, ok := content[0].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, tt.want, text["text"])
		})
	}
}
