package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A *TextContent is a Content (its methods have value receivers), and the
// helpers read it as they read the value; a nil one is no content.
func TestContentHelpersAcceptAPointer(t *testing.T) {
	block := NewTextContent("hello")
	text, ok := AsTextContent(&block)
	require.True(t, ok)
	assert.Equal(t, "hello", text.Text)
	assert.Equal(t, "hello", GetTextFromContent(&block))

	image := NewImageContent("aGk=", "image/png")
	got, ok := AsImageContent(&image)
	require.True(t, ok)
	assert.Equal(t, "image/png", got.MIMEType)

	_, ok = AsTextContent((*TextContent)(nil))
	assert.False(t, ok)
	_, ok = AsTextContent(&image)
	assert.False(t, ok)
}
