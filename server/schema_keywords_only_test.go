package server

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pbednarcik/flexmcp/mcp"
)

// A typed schema that carries only keywords the struct does not model is a
// schema all the same: with validation on it is compiled at registration,
// and one that cannot be compiled is refused rather than served
// unvalidated.
func TestSchemaOfKeptKeywordsOnlyIsCompiledAtRegistration(t *testing.T) {
	unknownDialect := map[string]any{"$schema": "http://json-schema.org/draft-03/schema#"}
	for _, path := range registrationPaths {
		t.Run(path.name, func(t *testing.T) {
			srv := NewMCPServer("schemas", "1.0.0", WithToolCapabilities(true), WithInputSchemaValidation())
			tool := mcp.Tool{Name: "tool", InputSchema: mcp.ToolInputSchema{AdditionalFields: unknownDialect}}
			assert.Contains(t, panicMessage(func() { path.register(srv, tool) }), `tool "tool" input schema cannot be compiled`)
			assert.Empty(t, srv.ListTools())

			srv = NewMCPServer("schemas", "1.0.0", WithToolCapabilities(true), WithOutputSchemaValidation())
			tool = mcp.NewTool("tool")
			tool.OutputSchema = mcp.ToolOutputSchema{AdditionalFields: unknownDialect}
			assert.Contains(t, panicMessage(func() { path.register(srv, tool) }), `tool "tool" output schema cannot be compiled`)
			assert.Empty(t, srv.ListTools())
		})
	}
}
