package mcp

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requestIDByValue is RequestId as it encoded before an integer was written
// straight out: its value, through json.Marshal.
type requestIDByValue RequestId

func (r requestIDByValue) MarshalJSON() ([]byte, error) { return json.Marshal(r.value) }

// encodeWith marshals value with one of the encoders.
func encodeWith(value any, encoder string) ([]byte, error) {
	switch encoder {
	case "v1 unescaped":
		var out bytes.Buffer
		enc := json.NewEncoder(&out)
		enc.SetEscapeHTML(false)
		err := enc.Encode(value)
		return bytes.TrimSuffix(out.Bytes(), []byte("\n")), err
	case "v2":
		return jsonv2.Marshal(value)
	case "v1 canonical ints":
		return jsonv2.Marshal(value, json.DefaultOptionsV1(), jsontext.CanonicalizeRawInts(true))
	}
	return json.Marshal(value)
}

// A request id is written as it always was, whatever its value and whatever
// the encoder.
func TestRequestIdMarshalsAsItsValueDid(t *testing.T) {
	for _, value := range []any{
		int64(0), int64(1), int64(-1), int64(255), int64(256), int64(math.MaxInt64), int64(math.MinInt64),
		"a", "<&>", "é", "", json.Number("9223372036854775808"), nil,
		7, uint64(math.MaxUint64), 2.0,
	} {
		id := NewRequestId(value)
		for _, encoder := range append(encoders, "v1 canonical ints") {
			want, err := encodeWith(struct {
				ID   requestIDByValue `json:"id"`
				Text string           `json:"text"`
			}{requestIDByValue(id), "x"}, encoder)
			require.NoError(t, err)
			got, err := encodeWith(struct {
				ID   RequestId `json:"id"`
				Text string    `json:"text"`
			}{id, "x"}, encoder)
			require.NoError(t, err)
			assert.Equal(t, string(want), string(got), "%#v inside %s", value, encoder)
		}
	}
}

// An integer id, the kind decoding gives and most clients send, is written
// into the response without a byte slice of its own.
func TestRequestIdIntegerMarshalAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	value := struct {
		ID RequestId `json:"id"`
	}{NewRequestId(int64(12345))}
	allocs := testing.AllocsPerRun(1000, func() {
		out.Reset()
		_ = enc.Encode(value)
	})
	assert.LessOrEqual(t, allocs, 3.0, "allocations per encoded integer id")
}

// The raw arguments are the call's own: changing the message they were read
// from leaves them as they were.
func TestCallToolParamsRawArgumentsAreTheirOwn(t *testing.T) {
	data := []byte(`{"name":"echo","arguments":{"region":"us-east-1"}}`)
	var params CallToolParams
	require.NoError(t, json.Unmarshal(data, &params))
	want := string(params.RawArguments)
	copy(data, bytes.Repeat([]byte("x"), len(data)))
	assert.Equal(t, `{"region":"us-east-1"}`, want)
	assert.Equal(t, want, string(params.RawArguments))
}

// The arguments are copied out of the message once: 12 allocations with the
// arguments read through json.Unmarshal into a map, 5 read in one pass.
func TestCallToolParamsUnmarshalAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	data := []byte(`{"name":"echo","arguments":{"region":"us-east-1"}}`)
	allocs := testing.AllocsPerRun(1000, func() {
		var params CallToolParams
		_ = json.Unmarshal(data, &params)
	})
	assert.LessOrEqual(t, allocs, 5.0, "allocations per tools/call params read")
}

// The raw structured content of a result is the result's own: changing the
// message it was read from leaves it as it was.
func TestResultRawStructuredContentIsItsOwn(t *testing.T) {
	data := []byte(`{"content":[],"structuredContent":{"n":1}}`)
	var result CallToolResult
	require.NoError(t, json.Unmarshal(data, &result))
	copy(data, bytes.Repeat([]byte("x"), len(data)))
	assert.Equal(t, `{"n":1}`, string(result.RawStructuredContent), "tool result")

	data = []byte(`{"type":"tool_result","toolUseId":"u","content":[],"structuredContent":{"n":1}}`)
	var content ToolResultContent
	require.NoError(t, json.Unmarshal(data, &content))
	copy(data, bytes.Repeat([]byte("x"), len(data)))
	assert.Equal(t, `{"n":1}`, string(content.RawStructuredContent), "tool result content")
}
