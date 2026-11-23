package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pbednarcik/flexmcp/mcp"
)

// A schema that does not compile is named in the error by its own kind: the
// output schema is not called input-schema.json.
func TestSchemaCompileErrorNamesItsKind(t *testing.T) {
	bad := map[string]any{"x": map[string]any{"type": 5}}
	register := func(tool mcp.Tool, opts ...ServerOption) (message string) {
		defer func() { message = fmt.Sprint(recover()) }()
		NewMCPServer("p", "1.0", opts...).AddTool(tool, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("x"), nil
		})
		return ""
	}

	output := mcp.NewTool("t")
	output.OutputSchema = mcp.ToolOutputSchema{Type: "object", Properties: bad}
	message := register(output, WithOutputSchemaValidation())
	assert.Contains(t, message, "mem:///flexmcp/tools/t/output-schema.json#")
	assert.NotContains(t, message, "input-schema.json")

	input := mcp.NewTool("t")
	input.InputSchema.Properties = bad
	message = register(input, WithInputSchemaValidation())
	assert.Contains(t, message, "mem:///flexmcp/tools/t/input-schema.json#")
}
