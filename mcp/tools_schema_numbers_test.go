package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schemaMember returns the value at path in a decoded schema value, or nil
// when a step is not an object.
func schemaMember(value any, path ...string) any {
	for _, key := range path {
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		value = object[key]
	}
	return value
}

// TestToolSchemaKeepsExactNumbers checks that the numbers of properties,
// $defs, definitions and additionalProperties are decoded as written.
func TestToolSchemaKeepsExactNumbers(t *testing.T) {
	const exact = "9007199254740993"
	tests := []struct {
		name, data, wire string
		decoded          func(ToolArgumentsSchema) any
	}{
		{
			"properties",
			`{"type":"object","properties":{"v":{"const":` + exact + `}}}`,
			`"properties":{"v":{"const":` + exact + `}}`,
			func(s ToolArgumentsSchema) any { return schemaMember(s.Properties, "v", "const") },
		},
		{
			"$defs",
			`{"type":"object","$defs":{"v":{"const":` + exact + `}},"properties":{"v":{"$ref":"#/$defs/v"}}}`,
			`"$defs":{"v":{"const":` + exact + `}}`,
			func(s ToolArgumentsSchema) any { return schemaMember(s.Defs, "v", "const") },
		},
		{
			"additionalProperties",
			`{"type":"object","additionalProperties":{"const":` + exact + `}}`,
			`"additionalProperties":{"const":` + exact + `}`,
			func(s ToolArgumentsSchema) any { return schemaMember(s.AdditionalProperties, "const") },
		},
		{
			"definitions adopted into $defs",
			`{"type":"object","definitions":{"v":{"const":` + exact + `}},"properties":{"v":{"$ref":"#/definitions/v"}}}`,
			`"$defs":{"v":{"const":` + exact + `}}`,
			func(s ToolArgumentsSchema) any { return schemaMember(s.Defs, "v", "const") },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var schema ToolArgumentsSchema
			require.NoError(t, json.Unmarshal([]byte(tt.data), &schema))
			assert.Equal(t, json.Number(exact), tt.decoded(schema))
			encoded, err := json.Marshal(schema)
			require.NoError(t, err)
			assert.Contains(t, string(encoded), tt.wire)
		})
	}

	t.Run("a number no float holds", func(t *testing.T) {
		var schema ToolArgumentsSchema
		require.NoError(t, json.Unmarshal([]byte(`{"definitions":{"h":{"const":1e400}}}`), &schema))
		assert.Equal(t, json.Number("1e400"), schemaMember(schema.Defs, "h", "const"))
		encoded, err := json.Marshal(schema)
		require.NoError(t, err)
		assert.Contains(t, string(encoded), `"$defs":{"h":{"const":1e400}}`)
	})

	// The rows below hold no number: they pin how null and a reused receiver
	// decode, which the exact decode keeps as it was.
	t.Run("null clears a field", func(t *testing.T) {
		schema := ToolArgumentsSchema{
			Defs:                 map[string]any{"a": true},
			Properties:           map[string]any{"a": true},
			PropertyOrder:        []string{"a"},
			AdditionalProperties: map[string]any{"a": true},
		}
		require.NoError(t, json.Unmarshal([]byte(`{"type":"object","$defs":null,"properties":null,"additionalProperties":null,"definitions":null}`), &schema))
		assert.Equal(t, ToolArgumentsSchema{Type: "object"}, schema)
	})
	t.Run("a null definitions is not adopted", func(t *testing.T) {
		var schema ToolArgumentsSchema
		require.NoError(t, json.Unmarshal([]byte(`{"type":"object","definitions":null}`), &schema))
		assert.Equal(t, ToolArgumentsSchema{Type: "object"}, schema)
	})
	t.Run("a reused receiver merges objects and replaces additionalProperties", func(t *testing.T) {
		schema := ToolArgumentsSchema{
			Defs:                 map[string]any{"a": true},
			Properties:           map[string]any{"a": true},
			AdditionalProperties: map[string]any{"a": true},
		}
		require.NoError(t, json.Unmarshal([]byte(`{"type":"object","$defs":{"b":false},"properties":{"b":false},"additionalProperties":{"b":false}}`), &schema))
		assert.Equal(t, ToolArgumentsSchema{
			Type:                 "object",
			Defs:                 map[string]any{"a": true, "b": false},
			Properties:           map[string]any{"a": true, "b": false},
			PropertyOrder:        []string{"b"},
			AdditionalProperties: map[string]any{"b": false},
		}, schema)
	})
	t.Run("a reused receiver keeps the fields a schema leaves out", func(t *testing.T) {
		schema := ToolArgumentsSchema{
			Defs:                 map[string]any{"a": true},
			Properties:           map[string]any{"a": true},
			AdditionalProperties: false,
		}
		require.NoError(t, json.Unmarshal([]byte(`{"type":"object","definitions":{"b":false}}`), &schema))
		assert.Equal(t, ToolArgumentsSchema{
			Type:                 "object",
			Defs:                 map[string]any{"a": true},
			Properties:           map[string]any{"a": true},
			AdditionalProperties: false,
		}, schema)
	})
}
