package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A property declared again with Required() is listed in required once.
func TestARequiredPropertyDeclaredAgainIsRequiredOnce(t *testing.T) {
	tool := NewTool("t",
		WithString("name", Required()),
		WithNumber("count", Required()),
		WithString("name", Required()),
		WithInteger("n", Required()), WithInteger("n", Required()),
		WithBoolean("b", Required()), WithBoolean("b", Required()),
		WithObject("o", Required()), WithObject("o", Required()),
		WithArray("a", Required()), WithArray("a", Required()),
		WithAny("x", Required()), WithAny("x", Required()),
	)
	assert.Equal(t, []string{"name", "count", "n", "b", "o", "a", "x"}, tool.InputSchema.Required)
}
