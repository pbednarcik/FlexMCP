package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A NotificationParams decoded into twice holds the second message alone: no
// _meta or field the second one did not send, and the maps handed out after
// the first decode are left as they were.
func TestNotificationParamsDecodedIntoTwiceHoldsTheSecondMessage(t *testing.T) {
	var p NotificationParams
	require.NoError(t, json.Unmarshal([]byte(`{"_meta":{"trace":"a"},"first":1}`), &p))
	firstMeta, firstFields := p.Meta, p.AdditionalFields

	require.NoError(t, json.Unmarshal([]byte(`{"second":2}`), &p))
	assert.Empty(t, p.Meta, "the first message's _meta")
	assert.Equal(t, map[string]any{"second": float64(2)}, p.AdditionalFields)

	assert.Equal(t, map[string]any{"trace": "a"}, firstMeta)
	assert.Equal(t, map[string]any{"first": float64(1)}, firstFields, "the second decode wrote into a map handed out earlier")
}

func TestNotificationParamsUnmarshal(t *testing.T) {
	tests := []struct {
		name       string
		data       string
		wantMeta   map[string]any
		wantFields map[string]any
	}{
		{"empty object", `{}`, map[string]any{}, map[string]any{}},
		{"meta and a field", `{"_meta":{"k":"v"},"x":"y"}`, map[string]any{"k": "v"}, map[string]any{"x": "y"}},
		{"a _meta that is not an object", `{"_meta":5,"x":"y"}`, map[string]any{}, map[string]any{"x": "y"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NotificationParams{Meta: map[string]any{"old": true}, AdditionalFields: map[string]any{"old": true}}
			require.NoError(t, json.Unmarshal([]byte(tt.data), &p))
			assert.Equal(t, tt.wantMeta, p.Meta)
			assert.Equal(t, tt.wantFields, p.AdditionalFields)
		})
	}
}

// null and malformed input leave the value as it was, as encoding/json does
// for null and for an error.
func TestNotificationParamsUnmarshalNullAndErrorsKeepTheValue(t *testing.T) {
	for _, data := range []string{`null`, `{"x":`, `[1]`} {
		t.Run(data, func(t *testing.T) {
			p := NotificationParams{Meta: map[string]any{"k": "v"}, AdditionalFields: map[string]any{"x": "y"}}
			_ = p.UnmarshalJSON([]byte(data))
			assert.Equal(t, map[string]any{"k": "v"}, p.Meta)
			assert.Equal(t, map[string]any{"x": "y"}, p.AdditionalFields)
		})
	}
}
