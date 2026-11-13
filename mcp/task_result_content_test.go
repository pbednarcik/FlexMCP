package mcp

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTaskResultResultWritesAnEmptyContentArray checks that a task result
// with no content writes "content":[] through encoding/json and through
// encoding/json/v2, as a tools/call result does, and that a parsed result
// with an empty array re-encodes it.
func TestTaskResultResultWritesAnEmptyContentArray(t *testing.T) {
	codecs := []struct {
		name    string
		marshal func(any) ([]byte, error)
	}{
		{"v1", json.Marshal},
		{"v2", func(v any) ([]byte, error) { return jsonv2.Marshal(v) }},
	}
	for _, tt := range []struct {
		name    string
		content []Content
	}{{"nil", nil}, {"empty", []Content{}}} {
		for _, codec := range codecs {
			t.Run(tt.name+"/"+codec.name, func(t *testing.T) {
				result := TaskResultResult{
					Content:           tt.content,
					StructuredContent: map[string]any{"ok": true},
				}
				data, err := codec.marshal(result)
				require.NoError(t, err)
				var fields map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(data, &fields))
				require.Contains(t, fields, "content", string(data))
				assert.JSONEq(t, `[]`, string(fields["content"]))
				assert.JSONEq(t, `{"ok":true}`, string(fields["structuredContent"]))
			})
		}
	}

	t.Run("parse", func(t *testing.T) {
		raw := json.RawMessage(`{"content":[],"structuredContent":{"ok":true}}`)
		result, err := ParseTaskResultResult(&raw)
		require.NoError(t, err)
		encoded, err := json.Marshal(result)
		require.NoError(t, err)
		assert.JSONEq(t, string(raw), string(encoded))
	})
}
