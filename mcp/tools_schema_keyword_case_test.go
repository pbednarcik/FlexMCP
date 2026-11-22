package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A tool schema decoded from the wire reads its modelled keywords by their
// exact names: a root key in another case, such as "Properties", "Type" or
// "Required", is an unknown keyword, kept and encoded again as written, and
// lends its value to no modelled one.
func TestToolSchemaModelledKeywordsAreCaseSensitive(t *testing.T) {
	var schema ToolInputSchema
	require.NoError(t, json.Unmarshal([]byte(`{"Type":"string","Properties":{"a":{"type":"string"}},"Required":["a"],"AdditionalProperties":false,"$Defs":{"d":{}}}`), &schema))
	assert.Empty(t, schema.Type)
	assert.Empty(t, schema.Properties)
	assert.Empty(t, schema.Required)
	assert.Nil(t, schema.AdditionalProperties)
	assert.Empty(t, schema.Defs)
	assert.Equal(t, map[string]any{
		"Type":                 "string",
		"Properties":           map[string]any{"a": map[string]any{"type": "string"}},
		"Required":             []any{"a"},
		"AdditionalProperties": false,
		"$Defs":                map[string]any{"d": map[string]any{}},
	}, schema.AdditionalFields)

	encoded, err := json.Marshal(schema)
	require.NoError(t, err)
	var again map[string]any
	require.NoError(t, json.Unmarshal(encoded, &again))
	assert.Equal(t, map[string]any{"a": map[string]any{"type": "string"}}, again["Properties"], "kept as written")
	assert.Empty(t, again["properties"], "no properties it never had")

	t.Run("beside the exact keyword", func(t *testing.T) {
		var both ToolInputSchema
		require.NoError(t, json.Unmarshal([]byte(`{"type":"object","properties":{"a":{"type":"string"}},"Properties":{"b":{}},"required":["a"],"Required":["b"]}`), &both))
		assert.Equal(t, "object", both.Type)
		assert.Equal(t, map[string]any{"a": map[string]any{"type": "string"}}, both.Properties)
		assert.Equal(t, []string{"a"}, both.Required)
		assert.Equal(t, map[string]any{"Properties": map[string]any{"b": map[string]any{}}, "Required": []any{"b"}}, both.AdditionalFields)
	})
}

// A server reads a tool's raw schema and a client the same schema decoded
// from tools/list: they agree on its annotations whatever the case of its
// root keywords, so a client mirrors the headers the server expects.
func TestParamHeaderAnnotationsAgreeOnTheRawAndTheListedSchema(t *testing.T) {
	params := json.RawMessage(`{"arguments":{"a":"v"}}`)
	for name, raw := range map[string]string{
		"properties":                 `{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"A"}}}`,
		"Properties":                 `{"type":"object","Properties":{"a":{"type":"string","x-mcp-header":"A"}}}`,
		"Properties, bad annotation": `{"type":"object","Properties":{"a":{"type":"array","x-mcp-header":"bad header"}}}`,
		"properties then Properties": `{"type":"object","properties":{"a":{"type":"string","x-mcp-header":"A"}},"Properties":{}}`,
		"Properties then properties": `{"type":"object","Properties":{},"properties":{"a":{"type":"string","x-mcp-header":"A"}}}`,
		"properties, bad annotation": `{"type":"object","properties":{"a":{"type":"array","x-mcp-header":"A"}}}`,
		"nested Properties":          `{"type":"object","properties":{"b":{"type":"object","Properties":{"c":{"type":"string","x-mcp-header":"C"}}}}}`,
		"Type beside the annotation": `{"type":"object","properties":{"a":{"Type":"string","x-mcp-header":"A"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := &Tool{Name: "t", RawInputSchema: json.RawMessage(raw)}
			var client Tool
			require.NoError(t, json.Unmarshal([]byte(`{"name":"t","inputSchema":`+raw+`}`), &client))
			serverErr, clientErr := ValidateParamHeaderAnnotations(server), ValidateParamHeaderAnnotations(&client)
			assert.Equal(t, serverErr == nil, clientErr == nil, "server %v, client %v", serverErr, clientErr)
			assert.Equal(t, ExtractParamHeaderBindings(server), ExtractParamHeaderBindings(&client))
			assert.Equal(t, GenerateParamHeaders(server, params), GenerateParamHeaders(&client, params))
		})
	}
}
