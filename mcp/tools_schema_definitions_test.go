package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A schema's definitions move to $defs, with their refs, only when the
// schema's dialect reads $defs and the schema has none of its own. Otherwise
// they stay as written, so the schema validates the same after a round trip.
func TestToolSchemaDefinitionsFollowTheDialect(t *testing.T) {
	const (
		pointer   = `"definitions":{"sku":{"type":"string","minLength":2}},"properties":{"sku":{"$ref":"#/definitions/sku"}}}`
		plainName = `"definitions":{"sku":{"$id":"#sku","type":"string","minLength":2}},"properties":{"sku":{"$ref":"#sku"}}}`
		draft04ID = `"definitions":{"sku":{"id":"#sku","type":"string","minLength":2}},"properties":{"sku":{"$ref":"#sku"}}}`
		both      = `"definitions":{"value":{"type":"integer"}},"$defs":{"value":{"type":"string"}},"properties":{"old":{"$ref":"#/definitions/value"},"new":{"$ref":"#/$defs/value"}}}`
		draft07   = `{"$schema":"http://json-schema.org/draft-07/schema#","type":"object",`
	)
	tests := []struct {
		name string
		// reused is the Defs of the receiver the schema is decoded into.
		reused map[string]any
		schema string
		// defs and definitions say which containers are on the wire.
		defs, definitions bool
		// refs maps a property to its $ref on the wire.
		refs map[string]string
		// valid and invalid are instances the round-tripped schema accepts
		// and refuses; with neither, it is not compiled.
		valid, invalid string
	}{
		{name: "undeclared", schema: `{"type":"object",` + pointer, defs: true, refs: map[string]string{"sku": "#/$defs/sku"}, valid: `{"sku":"ab"}`, invalid: `{"sku":"a"}`},
		{name: "2019-09", schema: `{"$schema":"https://json-schema.org/draft/2019-09/schema","type":"object",` + pointer, defs: true, refs: map[string]string{"sku": "#/$defs/sku"}, valid: `{"sku":"ab"}`, invalid: `{"sku":"a"}`},
		{name: "2020-12", schema: `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object",` + pointer, defs: true, refs: map[string]string{"sku": "#/$defs/sku"}, valid: `{"sku":"ab"}`, invalid: `{"sku":"a"}`},
		{name: "unknown $schema", schema: `{"$schema":"https://example.com/dialect","type":"object",` + pointer, defs: true, refs: map[string]string{"sku": "#/$defs/sku"}},
		{name: "draft-07 pointer ref", schema: draft07 + pointer, definitions: true, refs: map[string]string{"sku": "#/definitions/sku"}, valid: `{"sku":"ab"}`, invalid: `{"sku":"a"}`},
		{name: "draft-07 plain-name $id", schema: draft07 + plainName, definitions: true, refs: map[string]string{"sku": "#sku"}, valid: `{"sku":"ab"}`, invalid: `{"sku":"a"}`},
		{name: "draft-06 pointer ref", schema: `{"$schema":"http://json-schema.org/draft-06/schema#","type":"object",` + pointer, definitions: true, refs: map[string]string{"sku": "#/definitions/sku"}, valid: `{"sku":"ab"}`, invalid: `{"sku":"a"}`},
		{name: "draft-06 plain-name $id", schema: `{"$schema":"http://json-schema.org/draft-06/schema#","type":"object",` + plainName, definitions: true, refs: map[string]string{"sku": "#sku"}, valid: `{"sku":"ab"}`, invalid: `{"sku":"a"}`},
		{name: "draft-04 pointer ref", schema: `{"$schema":"http://json-schema.org/draft-04/schema#","type":"object",` + pointer, definitions: true, refs: map[string]string{"sku": "#/definitions/sku"}, valid: `{"sku":"ab"}`, invalid: `{"sku":"a"}`},
		{name: "draft-04 plain-name id", schema: `{"$schema":"http://json-schema.org/draft-04/schema#","type":"object",` + draft04ID, definitions: true, refs: map[string]string{"sku": "#sku"}, valid: `{"sku":"ab"}`, invalid: `{"sku":"a"}`},
		{name: "both containers, undeclared", schema: `{"type":"object",` + both, defs: true, definitions: true, refs: map[string]string{"old": "#/definitions/value", "new": "#/$defs/value"}, valid: `{"old":1,"new":"x"}`, invalid: `{"old":"x","new":"x"}`},
		{name: "both containers, draft-07", schema: draft07 + both, defs: true, definitions: true, refs: map[string]string{"old": "#/definitions/value", "new": "#/$defs/value"}, valid: `{"old":1,"new":"x"}`, invalid: `{"old":"x","new":"x"}`},
		{name: "null definitions", schema: `{"type":"object","definitions":null}`},
		{name: "null definitions, draft-07", schema: draft07 + `"definitions":null}`},
		{
			name:   "a reused receiver's Defs",
			reused: map[string]any{"old": map[string]any{"type": "integer"}},
			schema: `{"type":"object","definitions":{"value":{"type":"string"}},"properties":{"new":{"$ref":"#/definitions/value"}}}`,
			defs:   true, definitions: true, refs: map[string]string{"new": "#/definitions/value"}, valid: `{"new":"x"}`, invalid: `{"new":1}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema := ToolArgumentsSchema{Defs: tt.reused}
			require.NoError(t, json.Unmarshal([]byte(tt.schema), &schema))
			if tt.definitions {
				assert.IsType(t, map[string]any{}, schema.AdditionalFields["definitions"], "definitions is kept as written")
			} else {
				assert.NotContains(t, schema.AdditionalFields, "definitions")
			}
			if len(schema.AdditionalFields) == 0 {
				assert.Nil(t, schema.AdditionalFields, "no keyword is left beside the modeled ones")
			}
			encoded, err := json.Marshal(schema)
			require.NoError(t, err)
			t.Logf("round-tripped: %s", encoded)

			var wire map[string]any
			require.NoError(t, json.Unmarshal(encoded, &wire))
			_, hasDefs := wire["$defs"]
			_, hasDefinitions := wire["definitions"]
			assert.Equal(t, tt.defs, hasDefs, "$defs on the wire")
			assert.Equal(t, tt.definitions, hasDefinitions, "definitions on the wire")
			properties, _ := wire["properties"].(map[string]any)
			for name, ref := range tt.refs {
				property, _ := properties[name].(map[string]any)
				assert.Equal(t, ref, property["$ref"], name)
			}

			var again ToolArgumentsSchema
			require.NoError(t, json.Unmarshal(encoded, &again))
			second, err := json.Marshal(again)
			require.NoError(t, err)
			assert.JSONEq(t, string(encoded), string(second), "a second round trip changes nothing")

			if tt.valid == "" {
				return
			}
			compiled := compileDefinitionsSchema(t, encoded)
			assert.True(t, definitionsSchemaAccepts(t, compiled, tt.valid), tt.valid)
			assert.False(t, definitionsSchemaAccepts(t, compiled, tt.invalid), tt.invalid)
		})
	}
}

// A definitions kept as written is still walked for x-mcp-header: one on a
// property inside it is not reached through properties alone, and is refused.
func TestToolSchemaKeptDefinitionsAreStillChecked(t *testing.T) {
	tool := NewTool("t")
	require.NoError(t, json.Unmarshal([]byte(`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object",
		"definitions":{"d":{"properties":{"x":{"type":"string","x-mcp-header":"X"}}}}}`), &tool.InputSchema))
	require.Contains(t, tool.InputSchema.AdditionalFields, "definitions")
	assert.ErrorContains(t, ValidateParamHeaderAnnotations(&tool), "#/definitions/d/properties/x")
}

// compileDefinitionsSchema compiles schema with the validator the server
// uses.
func compileDefinitionsSchema(t *testing.T, schema []byte) *jsonschema.Schema {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(schema)))
	require.NoError(t, err)
	compiler := jsonschema.NewCompiler()
	require.NoError(t, compiler.AddResource("mem:///schema.json", doc))
	compiled, err := compiler.Compile("mem:///schema.json")
	require.NoError(t, err, "the round-tripped schema compiles")
	return compiled
}

// definitionsSchemaAccepts reports whether schema accepts instance.
func definitionsSchemaAccepts(t *testing.T, schema *jsonschema.Schema, instance string) bool {
	t.Helper()
	value, err := jsonschema.UnmarshalJSON(strings.NewReader(instance))
	require.NoError(t, err)
	return schema.Validate(value) == nil
}
