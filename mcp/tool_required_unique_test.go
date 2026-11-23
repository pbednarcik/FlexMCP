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

// A property's last declaration decides whether it is required, as it
// decides its schema: one declared again without Required() is not.
func TestAPropertyDeclaredAgainWithoutRequiredIsNotRequired(t *testing.T) {
	tool := NewTool("t",
		WithString("name", Required()), WithString("name"),
		WithNumber("count", Required()), WithNumber("count"),
		WithInteger("n", Required()), WithInteger("n"),
		WithBoolean("b", Required()), WithBoolean("b"),
		WithObject("o", Required()), WithObject("o"),
		WithArray("a", Required()), WithArray("a"),
		WithAny("x", Required()), WithAny("x"),
		WithString("kept", Required()),
		WithString("late"), WithString("late", Required()),
	)
	assert.Equal(t, []string{"kept", "late"}, tool.InputSchema.Required)
}

// Adding a name to required, or dropping one, leaves alone a Required slice
// another tool shares, as a copied Tool does, spare capacity or not.
func TestRequiredLeavesASharedSliceAlone(t *testing.T) {
	first := NewTool("t", WithString("a", Required()), WithString("b", Required()))
	second := first
	WithString("a")(&second)
	assert.Equal(t, []string{"a", "b"}, first.InputSchema.Required)
	assert.Equal(t, []string{"b"}, second.InputSchema.Required)

	base := NewTool("t", WithString("a", Required()), WithString("b", Required()), WithString("c", Required()))
	p, q := base, base
	WithString("p", Required())(&p)
	WithString("q", Required())(&q)
	assert.Equal(t, []string{"a", "b", "c"}, base.InputSchema.Required)
	assert.Equal(t, []string{"a", "b", "c", "p"}, p.InputSchema.Required)
	assert.Equal(t, []string{"a", "b", "c", "q"}, q.InputSchema.Required)
}
