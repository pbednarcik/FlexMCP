package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAnUntypedOutputSchemaIsAnObjectSchema pins that an output schema with
// any keyword set and no type is listed with "type":"object", the type the
// specification requires of an outputSchema, while the zero output schema is
// still left out, and that a typed one is listed as it is.
func TestAnUntypedOutputSchemaIsAnObjectSchema(t *testing.T) {
	for _, c := range []struct {
		name   string
		schema ToolOutputSchema
		want   string
	}{
		{"properties", ToolOutputSchema{Properties: map[string]any{"n": map[string]any{"type": "integer"}}},
			`{"type":"object","properties":{"n":{"type":"integer"}},"required":[]}`},
		{"empty properties", ToolOutputSchema{Properties: map[string]any{}},
			`{"type":"object","properties":{},"required":[]}`},
		{"required", ToolOutputSchema{Required: []string{"n"}},
			`{"type":"object","properties":{},"required":["n"]}`},
		{"$defs", ToolOutputSchema{Defs: map[string]any{"d": map[string]any{"type": "string"}}},
			`{"type":"object","$defs":{"d":{"type":"string"}},"properties":{},"required":[]}`},
		{"additionalProperties", ToolOutputSchema{AdditionalProperties: false},
			`{"type":"object","properties":{},"required":[],"additionalProperties":false}`},
		{"a kept keyword", ToolOutputSchema{AdditionalFields: map[string]any{"title": "Result"}},
			`{"type":"object","title":"Result","properties":{},"required":[]}`},
		{"a type", ToolOutputSchema{Type: "object", Required: []string{"n"}},
			`{"type":"object","properties":{},"required":["n"]}`},
		{"a type array", ToolOutputSchema{Types: []string{"object", "null"}},
			`{"type":["object","null"],"properties":{},"required":[]}`},
		// "type":[] names no type, and would list an invalid schema.
		{"an empty type array", ToolOutputSchema{Types: []string{}},
			`{"type":"object","properties":{},"required":[]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			assert.False(t, c.schema.IsZero())
			tool := NewTool("t")
			tool.OutputSchema = c.schema
			encoded, err := json.Marshal(tool)
			require.NoError(t, err)
			var listed struct {
				OutputSchema json.RawMessage `json:"outputSchema"`
			}
			require.NoError(t, json.Unmarshal(encoded, &listed))
			assert.JSONEq(t, c.want, string(listed.OutputSchema))

			// Set beside a raw output schema, it is a conflict.
			tool.RawOutputSchema = json.RawMessage(`{"type":"object"}`)
			_, err = json.Marshal(tool)
			assert.ErrorIs(t, err, errToolSchemaConflict)
		})
	}

	t.Run("zero", func(t *testing.T) {
		assert.True(t, ToolOutputSchema{}.IsZero())
		assert.True(t, ToolOutputSchema{PropertyOrder: []string{"n"}}.IsZero(), "an order alone sets no keyword")
		tool := NewTool("t")
		encoded, err := json.Marshal(tool)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), "outputSchema")

		tool.RawOutputSchema = json.RawMessage(`{"type":"object"}`)
		encoded, err = json.Marshal(tool)
		require.NoError(t, err)
		assert.Contains(t, string(encoded), `"outputSchema":{"type":"object"}`)
	})
}
