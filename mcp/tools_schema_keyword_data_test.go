package mcp

import (
	"bytes"
	"encoding/json"
	"maps"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A kept keyword keeps its numbers exactly: a constant past 2^53 is not
// rounded, and a number no float64 holds is not refused.
func TestSchemaAdditionalFieldsKeepNumbersExact(t *testing.T) {
	var schema ToolArgumentsSchema
	require.NoError(t, json.Unmarshal([]byte(`{"type":"object","const":{"n":9007199254740993},"x-big":1e400}`), &schema))
	encoded, err := json.Marshal(schema)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"n":9007199254740993`)
	assert.Contains(t, string(encoded), `"x-big":1e400`)
}

// Rewriting draft-07 references touches schemas only: a $ref inside a
// constant, an enum, a default, an example or a vendor keyword is data and
// stays as written, while a property named like one of those keywords is
// still a schema.
func TestSchemaDraft07RefsLeaveDataAlone(t *testing.T) {
	const literal = `{"$ref":"#/definitions/value"}`
	var schema ToolArgumentsSchema
	require.NoError(t, json.Unmarshal([]byte(`{"type":"object","definitions":{"value":true},`+
		`"const":`+literal+`,"enum":[`+literal+`],"default":`+literal+`,"examples":[`+literal+`],"x-vendor":`+literal+`,`+
		`"properties":{"const":{"$ref":"#/definitions/value"},"plain":{"type":"object","const":`+literal+`}},`+
		`"oneOf":[{"$ref":"#/definitions/value"}]}`), &schema))
	encoded, err := json.Marshal(schema)
	require.NoError(t, err)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(encoded, &wire))
	var data map[string]any
	require.NoError(t, json.Unmarshal([]byte(literal), &data))
	assert.Equal(t, data, wire["const"])
	assert.Equal(t, []any{data}, wire["enum"])
	assert.Equal(t, data, wire["default"])
	assert.Equal(t, []any{data}, wire["examples"])
	assert.Equal(t, data, wire["x-vendor"])
	properties := wire["properties"].(map[string]any)
	assert.Equal(t, "#/$defs/value", properties["const"].(map[string]any)["$ref"])
	assert.Equal(t, data, properties["plain"].(map[string]any)["const"])
	assert.Equal(t, "#/$defs/value", wire["oneOf"].([]any)[0].(map[string]any)["$ref"])
}

// An x-mcp-header is allowed only on a property reached from the root
// through properties keys alone; anywhere else the tool is refused, whether
// the schema is typed or raw.
func TestParamHeaderAnnotationsOnlyThroughProperties(t *testing.T) {
	annotated := map[string]any{"properties": map[string]any{"x": map[string]any{"type": "string", "x-mcp-header": "X"}}}
	for _, tt := range []struct {
		name   string
		fields map[string]any
	}{
		{"allOf", map[string]any{"allOf": []any{annotated}}},
		{"anyOf", map[string]any{"anyOf": []any{annotated}}},
		{"not", map[string]any{"not": annotated}},
		{"if", map[string]any{"if": annotated}},
		{"definitions", map[string]any{"$defs": map[string]any{"d": annotated}}},
	} {
		t.Run(tt.name+" typed", func(t *testing.T) {
			tool := NewTool("t", WithString("y"))
			tool.InputSchema.AdditionalFields = tt.fields
			// $defs is modeled: a typed schema carries it in Defs.
			if defs, ok := tt.fields["$defs"].(map[string]any); ok {
				tool.InputSchema.Defs = defs
			}
			require.Error(t, ValidateParamHeaderAnnotations(&tool))
		})
		t.Run(tt.name+" raw", func(t *testing.T) {
			raw := map[string]any{"type": "object"}
			maps.Copy(raw, tt.fields)
			encoded, err := json.Marshal(raw)
			require.NoError(t, err)
			tool := NewToolWithRawSchema("t", "", encoded)
			require.Error(t, ValidateParamHeaderAnnotations(&tool))
		})
	}
	t.Run("items", func(t *testing.T) {
		tool := NewToolWithRawSchema("t", "", json.RawMessage(`{"type":"object","properties":{"list":{"type":"array","items":{"type":"string","x-mcp-header":"X"}}}}`))
		require.Error(t, ValidateParamHeaderAnnotations(&tool))
	})
	t.Run("nested properties are fine", func(t *testing.T) {
		tool := NewToolWithRawSchema("t", "", json.RawMessage(`{"type":"object","properties":{"o":{"type":"object","properties":{"x":{"type":"string","x-mcp-header":"X"}}}},"const":{"x-mcp-header":"data"}}`))
		require.NoError(t, ValidateParamHeaderAnnotations(&tool))
	})
}

// An output schema of kept keywords only is advertised, and set beside a
// raw output schema it is the same conflict as any other.
func TestOutputSchemaOfKeptKeywordsOnly(t *testing.T) {
	tool := NewTool("t")
	tool.OutputSchema = ToolOutputSchema{AdditionalFields: map[string]any{"title": "Result"}}
	encoded, err := json.Marshal(tool)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), `"outputSchema":{`)
	assert.Contains(t, string(encoded), `"title":"Result"`)

	tool.RawOutputSchema = json.RawMessage(`{"type":"object"}`)
	_, err = json.Marshal(tool)
	require.Error(t, err)
}

// draft-07 dependencies maps property names to schemas: a property named
// like a keyword is still a schema there, so its refs are rewritten and its
// annotations checked. (In 2020-12, the default, dependencies is not a
// keyword.)
func TestSchemaDependenciesAreSchemas(t *testing.T) {
	var schema ToolArgumentsSchema
	require.NoError(t, json.Unmarshal([]byte(`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","definitions":{"value":true},"dependencies":{"const":{"$ref":"#/definitions/value"},"list":["a"]}}`), &schema))
	dependencies := schema.AdditionalFields["dependencies"].(map[string]any)
	assert.Equal(t, "#/$defs/value", dependencies["const"].(map[string]any)["$ref"])
	assert.Equal(t, []any{"a"}, dependencies["list"])

	tool := NewToolWithRawSchema("t", "", json.RawMessage(`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","dependencies":{"const":{"properties":{"x":{"type":"string","x-mcp-header":"X"}}}}}`))
	require.Error(t, ValidateParamHeaderAnnotations(&tool))
}

// A keyword that holds no schema holds data: an x-mcp-header key or a $ref
// inside it is neither an annotation nor a reference.
func TestSchemaUnknownKeywordsAreData(t *testing.T) {
	for _, raw := range []string{
		`{"type":"object","dependentRequired":{"x-mcp-header":["y"]}}`,
		`{"type":"object","extension-data":{"x-mcp-header":"data"}}`,
	} {
		tool := NewToolWithRawSchema("t", "", json.RawMessage(raw))
		require.NoError(t, ValidateParamHeaderAnnotations(&tool), raw)
	}
	var schema ToolArgumentsSchema
	require.NoError(t, json.Unmarshal([]byte(`{"type":"object","definitions":{"value":true},"extension-data":{"$ref":"#/definitions/value"}}`), &schema))
	assert.Equal(t, "#/definitions/value", schema.AdditionalFields["extension-data"].(map[string]any)["$ref"])
}

// A number no float64 holds does not hide a misplaced annotation.
func TestParamHeaderAnnotationsBesideAHugeNumber(t *testing.T) {
	const raw = `{"type":"object","x-big":1e400,"allOf":[{"properties":{"x":{"type":"string","x-mcp-header":"X"}}}]}`
	tool := NewToolWithRawSchema("t", "", json.RawMessage(raw))
	require.Error(t, ValidateParamHeaderAnnotations(&tool))
	var typed Tool
	require.NoError(t, json.Unmarshal([]byte(`{"name":"t","inputSchema":`+raw+`}`), &typed))
	require.Error(t, ValidateParamHeaderAnnotations(&typed))
}

// A generated input schema replaces the whole decoded one, kept keywords
// included.
func TestGeneratedInputSchemaReplacesKeptKeywords(t *testing.T) {
	type args struct {
		Name string `json:"name"`
	}
	for name, option := range map[string]ToolOption{
		"WithInputSchema":       WithInputSchema[args](),
		"WithCachedInputSchema": WithCachedInputSchema[args](NewSchemaCache()),
	} {
		t.Run(name, func(t *testing.T) {
			var tool Tool
			require.NoError(t, json.Unmarshal([]byte(`{"name":"t","inputSchema":{"type":"object","title":"old"}}`), &tool))
			option(&tool)
			encoded, err := json.Marshal(tool)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), `"old"`)
		})
	}
}

// Which keywords hold schemas depends on the dialect the schema declares:
// prefixItems is data in draft-07 and additionalItems is data in 2020-12,
// the default.
func TestSchemaKeywordsFollowTheDeclaredDialect(t *testing.T) {
	const draft07 = `"$schema":"http://json-schema.org/draft-07/schema#",`
	for _, tt := range []struct {
		schema  string
		refused bool
	}{
		{`{` + draft07 + `"type":"object","prefixItems":[{"x-mcp-header":"data"}]}`, false},
		{`{` + draft07 + `"type":"object","additionalItems":{"x-mcp-header":"X"}}`, true},
		{`{"type":"object","additionalItems":{"x-mcp-header":"data"}}`, false},
		{`{"type":"object","prefixItems":[{"x-mcp-header":"X"}]}`, true},
		{`{"$schema":"https://json-schema.org/draft/2019-09/schema","type":"object","additionalItems":{"x-mcp-header":"X"}}`, true},
	} {
		tool := NewToolWithRawSchema("t", "", json.RawMessage(tt.schema))
		err := ValidateParamHeaderAnnotations(&tool)
		if tt.refused {
			assert.Error(t, err, tt.schema)
		} else {
			assert.NoError(t, err, tt.schema)
		}
	}
	var schema ToolArgumentsSchema
	require.NoError(t, json.Unmarshal([]byte(`{`+draft07+`"type":"object","definitions":{"v":true},"prefixItems":[{"$ref":"#/definitions/v"}]}`), &schema))
	assert.Equal(t, "#/definitions/v", schema.AdditionalFields["prefixItems"].([]any)[0].(map[string]any)["$ref"])
}

// A subschema with its own $id is a resource of its own: its local refs
// point into it, so adopting the root's definitions does not rewrite them.
func TestSchemaDraft07RefsStopAtANestedResource(t *testing.T) {
	var schema ToolArgumentsSchema
	require.NoError(t, json.Unmarshal([]byte(`{"type":"object","definitions":{"v":false},"allOf":[{"$id":"urn:nested","definitions":{"v":true},"$ref":"#/definitions/v"}]}`), &schema))
	nested := schema.AdditionalFields["allOf"].([]any)[0].(map[string]any)
	assert.Equal(t, "#/definitions/v", nested["$ref"])
	assert.Contains(t, nested, "definitions")
}

// compileError compiles schema the way input validation does, returning
// the validator's verdict.
func compileError(schema []byte) error {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("mem:///schema.json", doc); err != nil {
		return err
	}
	_, err = c.Compile("mem:///schema.json")
	return err
}

// A schema the validator compiles still compiles after a decode and an
// encode: where a subschema starts a resource of its own, and what its
// dialect requires, is read as the validator reads it.
func TestSchemaRoundTripStillCompiles(t *testing.T) {
	const (
		draft04 = `"$schema":"http://json-schema.org/draft-04/schema#",`
		draft07 = `"$schema":"http://json-schema.org/draft-07/schema#",`
	)
	for name, source := range map[string]string{
		"a fragment-only $id is an anchor": `{` + draft07 + `"type":"object","definitions":{"v":{"type":"string"}},"properties":{"p":{"$id":"#p","allOf":[{"$ref":"#/definitions/v"}]}}}`,
		"an empty $id is no resource":      `{` + draft07 + `"type":"object","definitions":{"v":{"type":"string"}},"properties":{"p":{"$id":"","allOf":[{"$ref":"#/definitions/v"}]}}}`,
		"a $id of # is no resource":        `{` + draft07 + `"type":"object","definitions":{"v":{"type":"string"}},"properties":{"p":{"$id":"#","allOf":[{"$ref":"#/definitions/v"}]}}}`,
		"a $ref hides a draft-07 $id":      `{` + draft07 + `"type":"object","definitions":{"v":{"type":"string"}},"allOf":[{"$id":"urn:hidden","$ref":"#/definitions/v"}]}`,
		"draft-04 names resources by id":   `{` + draft04 + `"type":"object","required":["n"],"definitions":{"v":{"type":"string"}},"allOf":[{"id":"urn:nested","definitions":{"v":{"type":"number"}},"properties":{"n":{"$ref":"#/definitions/v"}}}]}`,
		"draft-04 required is never empty": `{` + draft04 + `"type":"object"}`,
	} {
		t.Run(name, func(t *testing.T) {
			require.NoError(t, compileError([]byte(source)), "the source schema")
			var schema ToolArgumentsSchema
			require.NoError(t, json.Unmarshal([]byte(source), &schema))
			encoded, err := json.Marshal(schema)
			require.NoError(t, err)
			assert.NoError(t, compileError(encoded), string(encoded))
		})
	}
}

// Each subschema is walked in the dialect of its resource, named by the
// whole $schema URI as the validator reads it: a keyword holds schemas only
// where that dialect says so, an embedded resource may name its own
// dialect, and a URI naming no known dialect means 2020-12.
func TestParamHeaderAnnotationsFollowTheResourceDialect(t *testing.T) {
	const (
		draft04 = `"$schema":"http://json-schema.org/draft-04/schema#",`
		draft06 = `"$schema":"http://json-schema.org/draft-06/schema#",`
		draft07 = `"$schema":"http://json-schema.org/draft-07/schema#",`
	)
	for name, tt := range map[string]struct {
		schema  string
		refused bool
	}{
		"draft-04 contains is data":         {`{` + draft04 + `"type":"object","contains":{"x-mcp-header":"data"}}`, false},
		"draft-04 propertyNames is data":    {`{` + draft04 + `"type":"object","propertyNames":{"x-mcp-header":"data"}}`, false},
		"draft-06 if is data":               {`{` + draft06 + `"type":"object","if":{"x-mcp-header":"data"}}`, false},
		"draft-06 contains is a schema":     {`{` + draft06 + `"type":"object","contains":{"x-mcp-header":"X"}}`, true},
		"draft-07 if is a schema":           {`{` + draft07 + `"type":"object","if":{"x-mcp-header":"X"}}`, true},
		"a 2020-12 resource in draft-07":    {`{` + draft07 + `"type":"object","allOf":[{"$id":"urn:child","$schema":"https://json-schema.org/draft/2020-12/schema","prefixItems":[{"x-mcp-header":"X"}]}]}`, true},
		"a draft-07 resource in 2020-12":    {`{"type":"object","allOf":[{"$id":"urn:child",` + draft07 + `"prefixItems":[{"x-mcp-header":"data"}]}]}`, false},
		"$schema outside a resource root":   {`{"type":"object","allOf":[{` + draft07 + `"prefixItems":[{"x-mcp-header":"X"}]}]}`, true},
		"a custom URI naming draft-07":      {`{"$schema":"urn:my:draft-07","type":"object","prefixItems":[{"x-mcp-header":"X"}]}`, true},
		"a custom URI naming 2019-09":       {`{"$schema":"urn:my:2019-09","type":"object","prefixItems":[{"x-mcp-header":"X"}]}`, true},
		"a fragment names no dialect":       {`{"$schema":"http://json-schema.org/draft-07/schema#/x","type":"object","prefixItems":[{"x-mcp-header":"X"}]}`, true},
		"https and no fragment":             {`{"$schema":"https://json-schema.org/draft-07/schema","type":"object","prefixItems":[{"x-mcp-header":"data"}]}`, false},
		"the latest-dialect URI is 2020-12": {`{"$schema":"https://json-schema.org/schema","type":"object","additionalItems":{"x-mcp-header":"data"}}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			tool := NewToolWithRawSchema("t", "", json.RawMessage(tt.schema))
			err := ValidateParamHeaderAnnotations(&tool)
			if tt.refused {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
