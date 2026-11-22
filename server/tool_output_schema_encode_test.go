package server

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// A structured output schema tools/list leaves out, one with properties but
// no type, is still the tool author's: one that cannot be encoded is refused
// at registration, as one tools/list writes is, whether output validation is
// on or off, rather than validated never without a word.
func TestAnOutputSchemaThatCannotBeEncodedIsRefused(t *testing.T) {
	byProperties := mcp.NewTool("out")
	byProperties.OutputSchema = mcp.ToolOutputSchema{Properties: map[string]any{"n": map[string]any{"minimum": math.Inf(-1)}}}
	byRequired := mcp.NewTool("out")
	byRequired.OutputSchema = mcp.ToolOutputSchema{Required: []string{"n"}, Defs: map[string]any{"d": map[string]any{"minimum": math.Inf(-1)}}}
	encodable := mcp.NewTool("out")
	encodable.OutputSchema = mcp.ToolOutputSchema{Properties: map[string]any{"n": map[string]any{"minimum": 0}}}

	for name, validation := range map[string]bool{"validation on": true, "validation off": false} {
		t.Run(name, func(t *testing.T) {
			var options []ServerOption
			if validation {
				options = append(options, WithOutputSchemaValidation())
			}
			srv := NewMCPServer("p", "1.0.0", options...)
			for _, unencodable := range []mcp.Tool{byProperties, byRequired} {
				message := panicMessage(func() { srv.AddTool(unencodable, okHandler) })
				assert.Contains(t, message, `tool "out" output schema cannot be encoded`)
				assert.Contains(t, message, "-Inf")
				assert.Nil(t, srv.GetTool("out"))
			}

			// An encodable one is as before: with validation on it does not
			// compile (it is written with "type":""), with it off it
			// registers, unlisted.
			message := panicMessage(func() { srv.AddTool(encodable, okHandler) })
			if validation {
				assert.Contains(t, message, "cannot be compiled")
			} else {
				require.Empty(t, message)
			}
		})
	}
}
