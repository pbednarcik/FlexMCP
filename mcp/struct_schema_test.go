package mcp

import (
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSchemaFor_JSONSchemaEnumTagSpacing(t *testing.T) {
	type withLeadingSpace struct {
		Name string `json:"name" jsonschema_description:"User name to query" jsonschema:" enum=Alice,enum=Bob"`
	}
	type withoutLeadingSpace struct {
		Name string `json:"name" jsonschema_description:"User name to query" jsonschema:"enum=Alice,enum=Bob"`
	}

	tests := []struct {
		name   string
		schema func() json.RawMessage
	}{
		{
			name: "leading space",
			schema: func() json.RawMessage {
				return NewTool("get_user_info", WithInputSchema[withLeadingSpace]()).RawInputSchema
			},
		},
		{
			name: "no leading space",
			schema: func() json.RawMessage {
				return NewTool("get_user_info", WithInputSchema[withoutLeadingSpace]()).RawInputSchema
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := test.schema()
			require.NotNil(t, raw)

			var schema map[string]any
			require.NoError(t, json.Unmarshal(raw, &schema))

			properties := schema["properties"].(map[string]any)
			nameProp := properties["name"].(map[string]any)
			assert.Equal(t, "User name to query", nameProp["description"])
			assert.ElementsMatch(t, []any{"Alice", "Bob"}, nameProp["enum"])
		})
	}
}

func TestSchemaFor_UnsupportedJSONSchemaOptionReturnsError(t *testing.T) {
	type request struct {
		Name string `json:"name" jsonschema:"description=User name"`
	}

	_, err := SchemaForRaw[request]()
	require.ErrorContains(t, err, "tag must not begin with 'WORD='")

	type private struct {
		Count int `json:"count,string" jsonschema:"description=Count"`
	}
	type promoted struct {
		private
		Mode string `json:"mode" jsonschema:"enum=fast,enum=safe"`
	}
	_, err = SchemaForRaw[promoted]()
	require.ErrorContains(t, err, "tag must not begin with 'WORD='")
}

func TestSchemaFor_NamedAnonymousStructTags(t *testing.T) {
	type Embedded struct {
		Mode string `json:"mode" jsonschema_description:"Run mode" jsonschema:"enum=fast,enum=safe"`
	}
	type request struct {
		Embedded `json:"embedded"`
	}

	raw, err := SchemaForRaw[request]()
	require.NoError(t, err)

	var schema map[string]any
	require.NoError(t, json.Unmarshal(raw, &schema))

	properties := schema["properties"].(map[string]any)
	assert.NotContains(t, properties, "mode")
	embedded := properties["embedded"].(map[string]any)
	embeddedProperties := embedded["properties"].(map[string]any)
	mode := embeddedProperties["mode"].(map[string]any)
	assert.Equal(t, "Run mode", mode["description"])
	assert.ElementsMatch(t, []any{"fast", "safe"}, mode["enum"])
}

func TestSchemaFor_RecursiveTaggedStructReturnsError(t *testing.T) {
	type node struct {
		Kind string `json:"kind" jsonschema:"enum=branch,enum=leaf"`
		Next *node  `json:"next,omitempty"`
	}

	_, err := SchemaForRaw[node]()
	require.ErrorIs(t, err, errRecursiveSchemaFallback)

	type EmbeddedNode struct {
		Kind string `json:"kind" jsonschema:"enum=branch,enum=leaf"`
		*EmbeddedNode
	}

	_, err = SchemaForRaw[EmbeddedNode]()
	require.ErrorIs(t, err, errRecursiveSchemaFallback)
}

func TestSchemaFor_NestedStructTags(t *testing.T) {
	type mode struct {
		Name string `json:"name" jsonschema_description:"Run mode" jsonschema:"enum=fast,enum=safe"`
	}
	type request struct {
		Primary   mode            `json:"primary"`
		Secondary mode            `json:"secondary"`
		Optional  *mode           `json:"optional"`
		Modes     []mode          `json:"modes"`
		ByName    map[string]mode `json:"byName"`
	}

	raw, err := SchemaForRaw[request]()
	require.NoError(t, err)

	var schema map[string]any
	require.NoError(t, json.Unmarshal(raw, &schema))

	properties, ok := schema["properties"].(map[string]any)
	require.True(t, ok)

	assertModeSchema := func(t *testing.T, nested any) {
		t.Helper()
		nestedSchema, ok := nested.(map[string]any)
		require.True(t, ok)
		nestedProperties, ok := nestedSchema["properties"].(map[string]any)
		require.True(t, ok)
		modeName, ok := nestedProperties["name"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "Run mode", modeName["description"])
		assert.ElementsMatch(t, []any{"fast", "safe"}, modeName["enum"])
	}

	for _, name := range []string{"primary", "secondary", "optional"} {
		assertModeSchema(t, properties[name])
	}

	modes, ok := properties["modes"].(map[string]any)
	require.True(t, ok)
	assertModeSchema(t, modes["items"])

	byName, ok := properties["byName"].(map[string]any)
	require.True(t, ok)
	assertModeSchema(t, byName["additionalProperties"])
}

func TestSchemaFor_StructuredInputOutputExampleTags(t *testing.T) {
	type WeatherRequest struct {
		Location string `json:"location,required" jsonschema_description:"City or location"` //nolint:staticcheck // required is interpreted by schemaFor, not encoding/json
		Units    string `json:"units,omitempty" jsonschema_description:"celsius or fahrenheit" jsonschema:"enum=celsius,enum=fahrenheit"`
	}

	tool := NewTool("get_weather",
		WithInputSchema[WeatherRequest](),
	)
	require.NotNil(t, tool.RawInputSchema)

	var schema map[string]any
	require.NoError(t, json.Unmarshal(tool.RawInputSchema, &schema))

	properties := schema["properties"].(map[string]any)

	location := properties["location"].(map[string]any)
	assert.Equal(t, "City or location", location["description"])

	units := properties["units"].(map[string]any)
	assert.Equal(t, "celsius or fahrenheit", units["description"])
	assert.ElementsMatch(t, []any{"celsius", "fahrenheit"}, units["enum"])

	required := schema["required"].([]any)
	assert.Contains(t, required, "location")
	assert.NotContains(t, required, "units")
}

func TestSchemaFor_RecursiveEmbeddingTags(t *testing.T) {
	type Node struct {
		*Node
		Count int `json:"count" jsonschema_description:"Number of results"`
	}
	_, err := jsonschema.For[Node](nil)
	require.NoError(t, err, "native inference accepts the recursive embedding")

	raw, err := SchemaForRaw[Node]()
	require.NoError(t, err)
	var schema map[string]any
	require.NoError(t, json.Unmarshal(raw, &schema))
	count := schema["properties"].(map[string]any)["count"].(map[string]any)
	assert.Equal(t, "integer", count["type"])
	assert.Equal(t, "Number of results", count["description"])

	tool := NewTool("recursive", WithInputSchema[Node]())
	assert.JSONEq(t, string(raw), string(tool.RawInputSchema))
}

// TestSchemaFor_FallbackFieldSelection pins the tag fallback to the fields
// encoding/json reads and writes: the shallowest field wins, a tagged field
// beats an untagged one at the same depth, two of a kind at the same depth
// drop each other, and an unexported embedding still promotes its fields.
func TestSchemaFor_FallbackFieldSelection(t *testing.T) {
	type Inner struct {
		Count int `json:"count"`
	}
	type shadowed struct {
		Count string `json:"count"`
		Inner
		Mode string `json:"mode" jsonschema:"enum=fast,enum=safe"`
	}
	type private struct {
		Count  int `json:"count"`
		hidden int
	}
	type promoted struct {
		private
		Mode string `json:"mode" jsonschema:"enum=fast,enum=safe"`
	}
	type Plain struct{ Count int }
	type Tagged struct {
		Other string `json:"Count"`
	}
	type OtherPlain struct{ Count bool }
	type taggedWins struct {
		Plain
		Tagged
		Mode string `json:"mode" jsonschema:"enum=fast,enum=safe"`
	}
	type taggedFirst struct {
		Tagged
		Plain
		Mode string `json:"mode" jsonschema:"enum=fast,enum=safe"`
	}
	// Two untagged fields at one depth: go vet refuses two equal json tags.
	type ambiguous struct {
		Plain
		OtherPlain
		Mode string `json:"mode" jsonschema:"enum=fast,enum=safe"`
	}
	type namedPrivate struct {
		private `json:"nested"`
		Mode    string `json:"mode" jsonschema:"enum=fast,enum=safe"`
	}
	for _, test := range []struct {
		name     string
		raw      func() (json.RawMessage, error)
		value    any
		wantJSON string
		nested   string
		property string
		kind     string
		required []any
	}{
		{name: "shallow field shadows embedded", raw: SchemaForRaw[shadowed], value: shadowed{Count: "x", Inner: Inner{Count: 7}, Mode: "fast"}, wantJSON: `{"count":"x","mode":"fast"}`, property: "count", kind: "string", required: []any{"count", "mode"}},
		{name: "unexported embedding promotes", raw: SchemaForRaw[promoted], value: promoted{private: private{Count: 7, hidden: 9}, Mode: "fast"}, wantJSON: `{"count":7,"mode":"fast"}`, property: "count", kind: "integer", required: []any{"count", "mode"}},
		{name: "tagged beats untagged", raw: SchemaForRaw[taggedWins], value: taggedWins{Plain: Plain{Count: 7}, Tagged: Tagged{Other: "x"}, Mode: "fast"}, wantJSON: `{"Count":"x","mode":"fast"}`, property: "Count", kind: "string", required: []any{"Count", "mode"}},
		{name: "tagged first beats untagged", raw: SchemaForRaw[taggedFirst], value: taggedFirst{Tagged: Tagged{Other: "x"}, Plain: Plain{Count: 7}, Mode: "fast"}, wantJSON: `{"Count":"x","mode":"fast"}`, property: "Count", kind: "string", required: []any{"Count", "mode"}},
		{name: "ambiguous dropped", raw: SchemaForRaw[ambiguous], value: ambiguous{Mode: "fast"}, wantJSON: `{"mode":"fast"}`, property: "Count", required: []any{"mode"}},
		{name: "named unexported embedding", raw: SchemaForRaw[namedPrivate], value: namedPrivate{private: private{Count: 7, hidden: 9}, Mode: "fast"}, wantJSON: `{"nested":{"count":7},"mode":"fast"}`, nested: "nested", property: "count", kind: "integer", required: []any{"count"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.value)
			require.NoError(t, err)
			require.JSONEq(t, test.wantJSON, string(encoded))

			raw, err := test.raw()
			require.NoError(t, err)
			var schema map[string]any
			require.NoError(t, json.Unmarshal(raw, &schema))
			if test.nested != "" {
				nested, ok := schema["properties"].(map[string]any)[test.nested].(map[string]any)
				require.True(t, ok, "property %q missing from %s", test.nested, raw)
				schema = nested
			}
			properties := schema["properties"].(map[string]any)
			assert.NotContains(t, properties, "hidden")
			if test.kind == "" {
				assert.NotContains(t, properties, test.property)
			} else {
				require.Contains(t, properties, test.property)
				assert.Equal(t, test.kind, properties[test.property].(map[string]any)["type"])
			}
			assert.Equal(t, test.required, schema["required"])
		})
	}
}
