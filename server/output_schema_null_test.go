package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

type nilFieldsInner struct {
	A int `json:"a"`
}

// nilFieldsResult holds every kind of field encoding/json writes as null
// when it is nil.
type nilFieldsResult struct {
	P     *nilFieldsInner `json:"p"`
	S     []string        `json:"s"`
	M     map[string]int  `json:"m"`
	Q     *string         `json:"q"`
	Omit  *nilFieldsInner `json:"omit,omitempty"`
	Value int             `json:"value"`
}

// TestOutputSchemaValidation_NilFieldsAreNull pins that a tool's own zero
// result passes output validation against the schema WithOutputSchema
// generates for it: its nil pointer, slice and map fields are written as
// null, which their schemas admit.
func TestOutputSchemaValidation_NilFieldsAreNull(t *testing.T) {
	srv := NewMCPServer("p", "1.0", WithOutputSchemaValidation())
	var result nilFieldsResult
	srv.AddTool(mcp.NewTool("out", mcp.WithOutputSchema[nilFieldsResult]()),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultStructuredOnly(result), nil
		})

	called := callOutputTool(t, srv)
	require.False(t, called.IsError, "the zero result is refused: %v", called.Content)

	result = nilFieldsResult{P: &nilFieldsInner{A: 1}, S: []string{}, M: map[string]int{"k": 1}, Value: 1}
	assert.False(t, callOutputTool(t, srv).IsError, "a populated result is refused")
}

// nilEnumResult holds nullable fields with an enum, unquoted and quoted.
type nilEnumResult struct {
	Mode   *string        `json:"mode" jsonschema:"enum=a,enum=b"`
	Quoted *int           `json:"quoted,string" jsonschema:"enum=1,enum=2"`
	M      map[string]int `json:"m"`
}

// TestOutputSchemaValidation_NilFieldsWithAnEnumAreNull pins that a nil
// pointer field with an enum passes output validation: its enum admits null
// as its type does.
func TestOutputSchemaValidation_NilFieldsWithAnEnumAreNull(t *testing.T) {
	srv := NewMCPServer("p", "1.0", WithOutputSchemaValidation())
	var result nilEnumResult
	srv.AddTool(mcp.NewTool("out", mcp.WithOutputSchema[nilEnumResult]()),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultStructuredOnly(result), nil
		})

	called := callOutputTool(t, srv)
	require.False(t, called.IsError, "the zero result is refused: %v", called.Content)

	mode, quoted := "a", 2
	result = nilEnumResult{Mode: &mode, Quoted: &quoted}
	assert.False(t, callOutputTool(t, srv).IsError, "a populated result is refused")
	mode = "c"
	assert.True(t, callOutputTool(t, srv).IsError, "a value outside the enum passes")
}
