package mcp

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParseTaskResultPreservesStructuredJSONPrecision checks that a parsed
// task result re-encodes its structured content with the numbers exactly as
// received, through encoding/json and through encoding/json/v2.
func TestParseTaskResultPreservesStructuredJSONPrecision(t *testing.T) {
	raw := json.RawMessage(`{"structuredContent":{"id":9007199254740993,"fraction":1.234567890123456789},"content":[]}`)
	result, err := ParseTaskResultResult(&raw)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"id": 9007199254740993.0, "fraction": 1.234567890123456789}, result.StructuredContent)
	for name, marshal := range map[string]func(any) ([]byte, error){
		"v1": json.Marshal,
		"v2": func(v any) ([]byte, error) { return jsonv2.Marshal(v) },
	} {
		t.Run(name, func(t *testing.T) {
			encoded, err := marshal(result)
			require.NoError(t, err)
			var got map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(encoded, &got))
			require.JSONEq(t, `{"id":9007199254740993,"fraction":1.234567890123456789}`, string(got["structuredContent"]))
			require.Contains(t, string(got["structuredContent"]), "9007199254740993")
			require.Contains(t, string(got["structuredContent"]), "1.234567890123456789")
		})
	}
}

// Without raw structured content, a task result is encoded by the encoder's
// own rules: encoding/json/v2 writes a nil slice as [] and honours its options,
// as it did before the result kept raw bytes.
func TestTaskResultIsEncodedByTheEncodersRules(t *testing.T) {
	result := TaskResultResult{StructuredContent: map[string]any{"items": []string(nil), "n": 5}}

	v1, err := json.Marshal(result)
	require.NoError(t, err)
	require.Contains(t, string(v1), `"items":null`)

	v2, err := jsonv2.Marshal(result)
	require.NoError(t, err)
	require.Contains(t, string(v2), `"items":[]`)

	stringified, err := jsonv2.Marshal(result, jsonv2.StringifyNumbers(true))
	require.NoError(t, err)
	require.Contains(t, string(stringified), `"n":"5"`)
}
