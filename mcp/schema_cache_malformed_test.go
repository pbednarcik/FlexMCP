package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A cached schema that is not one JSON value makes Save fail and leaves the
// file it would replace as it was: spliced in as it is, it would make the
// file unloadable, or add an entry of its own.
func TestSaveRefusesAMalformedCachedSchema(t *testing.T) {
	for name, raw := range map[string]string{
		"truncated object":  `{"type":`,
		"multiple values":   `{"type":"object"} {"type":"string"}`,
		"whitespace":        " \t\n",
		"extra cache entry": `true,"Injected":{"type":"string"}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "schemas.json")
			c := NewSchemaCache()
			c.WarmRaw("Existing", json.RawMessage(`{"type":"object"}`))
			require.NoError(t, c.Save(path))
			before, err := os.ReadFile(path)
			require.NoError(t, err)

			c.WarmRaw("Broken", json.RawMessage(raw))
			err = c.Save(path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), `"Broken"`)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
		})
	}
}

// Valid cached schemas are written byte for byte, a large integer included.
func TestMarshalKeepsCachedSchemasAsWritten(t *testing.T) {
	var c SchemaCache
	c.WarmRaw("A", json.RawMessage(`false`))
	c.WarmRaw("N", json.RawMessage(`{"const":9007199254740993}`))
	data, err := c.MarshalJSON()
	require.NoError(t, err)
	assert.Equal(t, `{"A":false,"N":{"const":9007199254740993}}`, string(data))
}
