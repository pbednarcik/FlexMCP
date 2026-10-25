package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// Validation feedback is bounded: a call whose arguments fail in thousands
// of places gets the first few failures and a count of the rest, not one
// message per failure, so a response cannot grow out of proportion to the
// request that drew it.
func TestInputSchemaValidation_FeedbackIsBounded(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation())
	values := make([]string, 50)
	for i := range values {
		values[i] = fmt.Sprintf("%q", fmt.Sprintf("option_value_%02d", i))
	}
	tool := mcp.Tool{
		Name: "picky",
		RawInputSchema: json.RawMessage(`{"type":"object","properties":{"xs":{"type":"array","items":{"enum":[` +
			strings.Join(values, ",") + `]}}}}`),
	}
	srv.AddTool(tool, okHandler)

	xs := make([]any, 10000)
	for i := range xs {
		xs[i] = 0
	}
	resp := callTool(t, srv, tool.Name, map[string]any{"xs": xs})
	jr, ok := resp.(mcp.JSONRPCResponse)
	require.True(t, ok)
	result, ok := jr.Result.(*mcp.CallToolResult)
	require.True(t, ok)
	require.True(t, result.IsError)
	text := result.Content[0].(mcp.TextContent).Text

	assert.Less(t, len(text), 8*1024, "validation feedback grew with the number of failures")
	assert.Contains(t, text, "/xs/0: ")
	assert.Contains(t, text, fmt.Sprintf("/xs/%d: ", maxValidationMessages-1))
	assert.NotContains(t, text, fmt.Sprintf("/xs/%d: ", maxValidationMessages))
	assert.Contains(t, text, fmt.Sprintf("and %d more", len(xs)-maxValidationMessages))
}

// One message is bounded too: a leaf that repeats a long enum is cut.
func TestInputSchemaValidation_OneMessageIsBounded(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation())
	values := make([]string, 200)
	for i := range values {
		values[i] = fmt.Sprintf("%q", fmt.Sprintf("option_value_%03d", i))
	}
	tool := mcp.Tool{
		Name:           "picky",
		RawInputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"enum":[` + strings.Join(values, ",") + `]}}}`),
	}
	srv.AddTool(tool, okHandler)

	resp := callTool(t, srv, tool.Name, map[string]any{"x": "nope"})
	result := resp.(mcp.JSONRPCResponse).Result.(*mcp.CallToolResult)
	require.True(t, result.IsError)
	text := result.Content[0].(mcp.TextContent).Text
	assert.Less(t, len(text), maxValidationMessageBytes+64, "one message repeated the whole enum")
	assert.Contains(t, text, "/x: ")
}
