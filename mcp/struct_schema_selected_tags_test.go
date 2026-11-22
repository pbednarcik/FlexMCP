package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type ShadowedTagEmbed struct {
	A string `json:"dup" jsonschema:"enum=x" jsonschema_description:"shadowed"`
}

type shadowedTag struct {
	ShadowedTagEmbed
	Dup string `json:"dup" jsonschema_description:"selected"`
}

type promotedTagInner struct {
	Mode string `json:"mode" jsonschema:"enum=a,enum=b"`
}

type promotedTag struct {
	promotedTagInner
	Y int `json:"y"`
}

// A property's description and enum are those of the field encoding/json
// picks for it: a shadowed field lends none, and a field promoted from an
// unexported embedding keeps its own.
func TestSchemaForAnnotationsComeFromTheSelectedField(t *testing.T) {
	schema, err := schemaFor[shadowedTag]()
	require.NoError(t, err)
	dup := schema.Properties["dup"]
	require.NotNil(t, dup)
	assert.Empty(t, dup.Enum, "the shadowed field's enum")
	assert.Equal(t, "selected", dup.Description)

	schema, err = schemaFor[promotedTag]()
	require.NoError(t, err)
	mode := schema.Properties["mode"]
	require.NotNil(t, mode)
	assert.Equal(t, []any{"a", "b"}, mode.Enum)
}
