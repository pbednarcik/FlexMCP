package server

import (
	"encoding/json"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
)

// requestIDValueByUnmarshal is requestIDValue as it was before it kept its id
// on the stack: the id read through json.Unmarshal.
func requestIDValueByUnmarshal(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, true
	}
	var requestID mcp.RequestId
	if json.Unmarshal(raw, &requestID) != nil {
		return nil, false
	}
	switch value := requestID.Value(); value.(type) {
	case string, int64, json.Number:
		return value, true
	}
	return nil, false
}

// requestIDValue reads any id a decoder gives as json.Unmarshal read it: the
// same value, of the same type, or the same refusal.
func FuzzRequestIDValue(f *testing.F) {
	for _, id := range []string{
		`1`, `0`, `-1`, `255`, `256`, `9223372036854775807`, `9223372036854775808`, `-9223372036854775809`,
		`01`, `-0`, `1.0`, `1e3`, `-`, `"a"`, `""`, `"é"`, `"\ud800"`, `"<&>"`,
		`null`, `true`, `{}`, `[]`, `[1]`, `{"a":1}`,
	} {
		f.Add(id)
	}
	f.Fuzz(func(t *testing.T, id string) {
		var message struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal([]byte(`{"id":`+id+`}`), &message) != nil {
			t.Skip("not a value a decoder gives")
		}
		wantID, wantOK := requestIDValueByUnmarshal(message.ID)
		gotID, gotOK := requestIDValue(message.ID)
		assert.Equal(t, wantOK, gotOK, "ok for %s", message.ID)
		assert.Equal(t, wantID, gotID, "id for %s", message.ID)
	})
}

// An integer id is read without an allocation: a request is read twice over
// HTTP, by the transport and the core.
func TestRequestIDValueAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	raw := json.RawMessage(`1`)
	allocs := testing.AllocsPerRun(1000, func() { _, _ = requestIDValue(raw) })
	assert.Zero(t, allocs, "allocations per integer id read")
}
