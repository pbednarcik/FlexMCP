package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A member named twice keeps its last value, as the per-check reads kept it:
// read into types, a duplicate would merge into or keep the earlier one.
func TestReadOnceDuplicateMembersKeepTheLastValue(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	srv.AddTool(mcp.NewTool("echo"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		roots := "no roots"
		if info := RequestProtocolInfoFromContext(ctx); info != nil && info.ClientCapabilities != nil && info.ClientCapabilities.Roots != nil {
			roots = "roots"
		}
		return mcp.NewToolResultText(roots), nil
	})
	name, _ := mcp.EncodeHeaderValue("echo")
	ctx := context.WithValue(t.Context(), requestHeader, http.Header{
		"Mcp-Protocol-Version": {"2026-07-28"}, "Mcp-Method": {"tools/call"}, "Mcp-Name": {name},
	})
	call := func(body string) string {
		out, err := json.Marshal(srv.HandleMessage(ctx, json.RawMessage(body)))
		require.NoError(t, err)
		return string(out)
	}
	const (
		version = `"io.modelcontextprotocol/protocolVersion":"2026-07-28"`
		caps    = `"io.modelcontextprotocol/clientCapabilities":{}`
	)

	t.Run("protocolVersion, then null", func(t *testing.T) {
		out := call(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","_meta":{` +
			version + `,` + caps + `,"io.modelcontextprotocol/protocolVersion":null}}}`)
		assert.NotContains(t, out, `"resultType"`, "the last version is null, a legacy request: %s", out)
	})
	t.Run("clientCapabilities with roots, then without", func(t *testing.T) {
		out := call(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","_meta":{` + version +
			`,"io.modelcontextprotocol/clientCapabilities":{"roots":{}},"io.modelcontextprotocol/clientCapabilities":{}}}}`)
		assert.Contains(t, out, `"text":"no roots"`, out)
	})
	t.Run("params, then empty params", func(t *testing.T) {
		out := call(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","_meta":{` + version + `,` + caps + `}},"params":{}}`)
		assert.Contains(t, out, `"code":-32020`, "the header names echo, the last params no tool: %s", out)
	})
}
