package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Required() has no meaning in an array's item schema, and a boolean
// "required" is not JSON Schema: the item builders leave it out.
func TestPrimitiveItemSchemasHaveNoRequired(t *testing.T) {
	for name, items := range map[string]PropertyOption{
		"string":  WithStringItems(Required()),
		"number":  WithNumberItems(Required()),
		"integer": WithIntegerItems(Required()),
		"boolean": WithBooleanItems(Required()),
	} {
		tool := NewTool("t", WithArray("list", items))
		item := tool.InputSchema.Properties["list"].(map[string]any)["items"].(map[string]any)
		assert.NotContains(t, item, "required", name)
		assert.Equal(t, name, item["type"], name)
	}
}
