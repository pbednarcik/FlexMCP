package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A tool_result whose content is not an array is an error, not a result
// with no content.
func TestToolResultContentMustBeAnArray(t *testing.T) {
	for name, content := range map[string]any{"string": "answer", "object": map[string]any{"type": "text", "text": "x"}} {
		_, err := ParseContent(map[string]any{"type": "tool_result", "toolUseId": "t1", "content": content})
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "tool_result content: expected array", name)
	}
	parsed, err := ParseContent(map[string]any{"type": "tool_result", "toolUseId": "t1"})
	require.NoError(t, err, "no content is still allowed")
	assert.NotNil(t, parsed)
}
