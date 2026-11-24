package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// TestAnUntypedOutputSchemaIsValidatedAsAnObjectSchema pins that an output
// schema with keywords and no type registers with output validation on and is
// validated as "type":"object", as tools/list writes it, whichever keyword it
// sets, and that the zero output schema validates nothing.
func TestAnUntypedOutputSchemaIsValidatedAsAnObjectSchema(t *testing.T) {
	for _, c := range []struct {
		name   string
		schema mcp.ToolOutputSchema
		good   any
		bad    any
	}{
		{"properties", mcp.ToolOutputSchema{Properties: map[string]any{"n": map[string]any{"type": "integer"}}},
			map[string]any{"n": 1}, map[string]any{"n": "one"}},
		{"required", mcp.ToolOutputSchema{Required: []string{"n"}},
			map[string]any{"n": 1}, map[string]any{}},
		{"$defs", mcp.ToolOutputSchema{Defs: map[string]any{"d": map[string]any{"type": "string"}}},
			map[string]any{}, []any{"not an object"}},
		{"additionalProperties", mcp.ToolOutputSchema{AdditionalProperties: false},
			map[string]any{}, map[string]any{"extra": 1}},
		{"a kept keyword", mcp.ToolOutputSchema{AdditionalFields: map[string]any{"title": "Result"}},
			map[string]any{}, "not an object"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := NewMCPServer("p", "1.0.0", WithOutputSchemaValidation())
			var structured any
			tool := mcp.NewTool("out")
			tool.OutputSchema = c.schema
			require.NotPanics(t, func() {
				srv.AddTool(tool, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return mcp.NewToolResultStructuredOnly(structured), nil
				})
			})

			listed := toolsListOutputSchema(t, srv)
			assert.Equal(t, "object", listed["type"], "listed as %v", listed)

			structured = c.good
			assert.False(t, callOutputTool(t, srv).IsError, "a conforming result is refused")
			structured = c.bad
			assert.True(t, callOutputTool(t, srv).IsError, "a result breaking the schema passes")
		})
	}

	t.Run("zero", func(t *testing.T) {
		srv := NewMCPServer("p", "1.0.0", WithOutputSchemaValidation())
		srv.AddTool(mcp.NewTool("out"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultStructuredOnly("anything"), nil
		})
		assert.Nil(t, toolsListOutputSchema(t, srv))
		assert.False(t, callOutputTool(t, srv).IsError)
	})
}

// toolsListOutputSchema returns the output schema tools/list writes for the
// server's one tool, nil when it writes none.
func toolsListOutputSchema(t *testing.T, srv *MCPServer) map[string]any {
	t.Helper()
	out, err := json.Marshal(srv.HandleMessage(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	require.NoError(t, err)
	var response struct {
		Result struct {
			Tools []struct {
				OutputSchema map[string]any `json:"outputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(out, &response))
	require.Len(t, response.Result.Tools, 1, "tools/list answered %s", out)
	return response.Result.Tools[0].OutputSchema
}

// callOutputTool calls the server's tool "out" and returns its result.
func callOutputTool(t *testing.T, srv *MCPServer) mcp.CallToolResult {
	t.Helper()
	out, err := json.Marshal(srv.HandleMessage(t.Context(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"out","arguments":{}}}`)))
	require.NoError(t, err)
	var response struct {
		Result mcp.CallToolResult `json:"result"`
	}
	require.NoError(t, json.Unmarshal(out, &response), "tools/call answered %s", out)
	return response.Result
}
