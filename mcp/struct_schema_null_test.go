package mcp

import (
	"encoding/json"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type nullableLeaf struct {
	A int `json:"a"`
}

type nullableMaps struct {
	M map[string]bool `json:"m"`
}

// nullableNative is inferred by jsonschema-go and reconciled.
type nullableNative struct {
	P         *nullableLeaf             `json:"p"`
	S         []string                  `json:"s"`
	M         map[string]int            `json:"m"`
	Q         *string                   `json:"q"`
	PM        *map[string]int           `json:"pm"`
	Items     []map[string]int          `json:"items"`
	Values    map[string][]int          `json:"values"`
	MapValues map[string]map[string]int `json:"mapValues"`
	Inner     nullableMaps              `json:"inner"`
	Quoted    *int                      `json:"quoted,string"`
	Number    int                       `json:"number,string"`
}

// nullableTagged takes the tag fallback, as do the containers holding it.
type nullableTagged struct {
	Mode string         `json:"mode" jsonschema:"enum=a,enum=b"`
	M    map[string]int `json:"m"`
}

// nullableFallback is built by the tag fallback, field by field.
type nullableFallback struct {
	Mode   string                    `json:"mode" jsonschema:"enum=a,enum=b"`
	P      *nullableTagged           `json:"p"`
	S      []nullableTagged          `json:"s"`
	M      map[string]nullableTagged `json:"m"`
	Plain  map[string]int            `json:"plain"`
	Inner  nullableTagged            `json:"inner"`
	Quoted *int                      `json:"quoted,string"`
	Choice *string                   `json:"choice" jsonschema:"enum=x,enum=y"`
}

// TestSchemaFor_NilFieldsAreNull pins that a field encoding/json writes as
// null when it is nil, a pointer, slice or map, admits null in its schema,
// native or built by the tag fallback, at any depth, so the zero value of a
// struct is valid against its own schema; json:",string" fields keep the
// quoted form, and a root map is still an object.
func TestSchemaFor_NilFieldsAreNull(t *testing.T) {
	nullable := []any{"null", "object"}
	t.Run("native", func(t *testing.T) {
		schema, err := schemaFor[nullableNative]()
		require.NoError(t, err)
		assertValidJSON(t, schema, nullableNative{})
		assertValidJSON(t, schema, nullableNative{
			Items: []map[string]int{nil}, Values: map[string][]int{"v": nil},
			MapValues: map[string]map[string]int{"v": nil},
		})
		properties := schemaProperties(t, schema)
		assert.Equal(t, nullable, properties["m"]["type"])
		assert.Equal(t, nullable, properties["pm"]["type"])
		assert.Equal(t, []any{"null", "array"}, properties["s"]["type"])
		assert.Equal(t, nullable, properties["items"]["items"].(map[string]any)["type"])
		assert.Equal(t, nullable, properties["mapValues"]["additionalProperties"].(map[string]any)["type"])
		assert.Equal(t, nullable, properties["inner"]["properties"].(map[string]any)["m"].(map[string]any)["type"])
		assert.Equal(t, "object", properties["inner"]["type"], "a struct value is never null")
		assert.Equal(t, []any{"null", "string"}, properties["quoted"]["type"])
		assert.Equal(t, "string", properties["number"]["type"])
	})
	t.Run("fallback", func(t *testing.T) {
		schema, err := schemaFor[nullableFallback]()
		require.NoError(t, err)
		assertValidJSON(t, schema, nullableFallback{Mode: "a", Inner: nullableTagged{Mode: "b"}})
		assertValidJSON(t, schema, nullableFallback{
			Mode: "a", Inner: nullableTagged{Mode: "b"},
			P: &nullableTagged{Mode: "a"}, S: []nullableTagged{{Mode: "a"}}, M: map[string]nullableTagged{"k": {Mode: "b"}},
		})
		properties := schemaProperties(t, schema)
		assert.Equal(t, nullable, properties["p"]["type"])
		assert.Equal(t, []any{"null", "array"}, properties["s"]["type"])
		assert.Equal(t, nullable, properties["m"]["type"])
		assert.Equal(t, nullable, properties["plain"]["type"])
		assert.Equal(t, "object", properties["inner"]["type"], "a struct value is never null")
		assert.Equal(t, nullable, properties["inner"]["properties"].(map[string]any)["m"].(map[string]any)["type"])
		assert.Equal(t, []any{"null", "string"}, properties["quoted"]["type"])
		assert.Equal(t, []any{"null", "string"}, properties["choice"]["type"])
		assert.Equal(t, []any{"x", "y", nil}, properties["choice"]["enum"], "a nullable field's enum admits null")
		assert.Equal(t, []any{"a", "b"}, properties["mode"]["enum"], "a field never null has no null in its enum")
	})
	t.Run("root map", func(t *testing.T) {
		schema, err := schemaFor[map[string]map[string]int]()
		require.NoError(t, err)
		assert.Equal(t, "object", schema.Type)
		assert.Empty(t, schema.Types)
		assert.Equal(t, []string{"null", "object"}, schema.AdditionalProperties.Types, "a map inside the root admits null")
	})
}

// assertValidJSON checks value, as encoding/json writes it, against schema.
func assertValidJSON(t *testing.T, schema *jsonschema.Schema, value any) {
	t.Helper()
	resolved, err := schema.Resolve(nil)
	require.NoError(t, err)
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	var instance any
	require.NoError(t, json.Unmarshal(encoded, &instance))
	assert.NoError(t, resolved.Validate(instance), "%s", encoded)
}

// schemaProperties returns the encoded schema's properties, by name.
func schemaProperties(t *testing.T, schema *jsonschema.Schema) map[string]map[string]any {
	t.Helper()
	encoded, err := json.Marshal(schema)
	require.NoError(t, err)
	var decoded struct {
		Properties map[string]map[string]any `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	return decoded.Properties
}
