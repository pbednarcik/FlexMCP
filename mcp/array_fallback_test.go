package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSchemaTagFallbackPreservesArrayLength pins that the tag fallback, taken
// when an element type's tags need it, keeps a fixed array's length as
// minItems and maxItems, and gives a slice neither.
func TestSchemaTagFallbackPreservesArrayLength(t *testing.T) {
	type item struct {
		Mode string `json:"mode" jsonschema:"enum=fast,enum=safe"`
	}
	type request struct {
		Pair  [2]item `json:"pair"`
		Items []item  `json:"items"`
	}
	raw, err := SchemaForRaw[request]()
	require.NoError(t, err)
	var schema map[string]any
	require.NoError(t, json.Unmarshal(raw, &schema))
	properties := schema["properties"].(map[string]any)
	pair := properties["pair"].(map[string]any)
	assert.Equal(t, float64(2), pair["minItems"], string(raw))
	assert.Equal(t, float64(2), pair["maxItems"], string(raw))
	items := properties["items"].(map[string]any)
	assert.NotContains(t, items, "minItems")
	assert.NotContains(t, items, "maxItems")
}
