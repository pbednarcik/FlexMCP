package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Meta encodes and decodes itself on the stream it is read from or written
// to (MarshalJSONTo, UnmarshalJSONFrom); the result is the one MarshalJSON
// and UnmarshalJSON give, byte for byte and value for value, under the
// encoding/json rules: sorted keys, HTML escaping, the last of duplicate
// names, invalid UTF-8 replaced.
func TestMetaStreamsAsItMarshals(t *testing.T) {
	for _, meta := range []*Meta{
		{},
		{AdditionalFields: map[string]any{}},
		{ProgressToken: "p-1"},
		{ProgressToken: float64(7), AdditionalFields: map[string]any{"z": 1, "a": "<b>&"}},
		{AdditionalFields: map[string]any{"progressToken": "kept", "nested": map[string]any{"k": []any{1, "two", nil}}}},
		{ProgressToken: "outer", AdditionalFields: map[string]any{"progressToken": "inner"}},
	} {
		want, err := meta.MarshalJSON()
		require.NoError(t, err)
		got, err := json.Marshal(meta)
		require.NoError(t, err)
		assert.Equal(t, string(want), string(got))
	}

	for _, input := range []string{
		`{}`,
		`{"progressToken":"p-1","io.modelcontextprotocol/protocolVersion":"2026-07-28","nested":{"a":[1,{"b":null}]}}`,
		`{"a":1,"a":2}`,
		"{\"bad\":\"\xff\"}",
		`{"progressToken":7}`,
	} {
		var want Meta
		require.NoError(t, want.UnmarshalJSON([]byte(input)), input)
		var got struct {
			Meta *Meta `json:"_meta"`
		}
		require.NoError(t, json.Unmarshal([]byte(`{"_meta":`+input+`}`), &got), input)
		require.NotNil(t, got.Meta, input)
		assert.Equal(t, want, *got.Meta, input)
	}
}
