package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

// coerceWant is what spf13/cast v1.7.1 returned for one input, recorded in
// coerce_golden_test.go; the coercion helpers must keep answering the same.
type coerceWant struct {
	Bool        bool
	Int64       int64
	Int32       int32
	Int16       int16
	Int8        int8
	Int         int
	Uint        uint
	Uint64      uint64
	Uint32      uint32
	Uint16      uint16
	Uint8       uint8
	Float32     float32
	Float64     float64
	String      string
	Float64E    float64
	Float64OK   bool
	Int64E      int64
	Int64OK     bool
	StringSlice []string
	StringMap   map[string]any
}

// coerceInputs mirrors the recorder's table (docs/Record/cast-golden, kept
// out of the module because it still needs spf13/cast); a new input needs a
// new recording: go run . ../../../mcp/coerce_golden_test.go from there.
var coerceInputs = []struct {
	name string
	v    any
}{
	{"nil", nil},
	{"true", true},
	{"false", false},
	{"int_42", int(42)},
	{"int_neg7", int(-7)},
	{"int8_neg1", int8(-1)},
	{"int64_big", int64(1 << 40)},
	{"uint8_200", uint8(200)},
	{"uint64_max", uint64(1<<64 - 1)},
	{"float_3", float64(3)},
	{"float_3_7", float64(3.7)},
	{"float_neg3_7", float64(-3.7)},
	{"float_300", float64(300)},
	{"float_1e10", float64(1e10)},
	{"float32_2_5", float32(2.5)},
	{"str_42", "42"},
	{"str_spaced_42", " 42 "},
	{"str_3_0", "3.0"},
	{"str_3_7", "3.7"},
	{"str_neg1", "-1"},
	{"str_hex", "0x1A"},
	{"str_octal", "010"},
	{"str_abc", "abc"},
	{"str_empty", ""},
	{"str_true", "true"},
	{"str_T", "T"},
	{"str_yes", "yes"},
	{"str_1", "1"},
	{"str_0", "0"},
	{"str_off", "off"},
	{"str_json_map", `{"a":1,"b":"x"}`},
	{"str_words", "user assistant"},
	{"number_12", json.Number("12")},
	{"number_2_5", json.Number("2.5")},
	{"bytes_7", []byte("7")},
	{"slice_any", []any{"user", 1, true}},
	{"slice_str", []string{"a", "b"}},
	{"slice_int", []int{1, 2}},
	{"map_any", map[string]any{"a": 1, "b": "x"}},
	{"map_str", map[string]string{"k": "v"}},
	{"map_anykey", map[any]any{"k": 1, 2: "two"}},
}

func TestCoerceParityWithCast(t *testing.T) {
	assert.Len(t, coerceGolden, len(coerceInputs), "every input needs a recording")
	for _, in := range coerceInputs {
		t.Run(in.name, func(t *testing.T) {
			want, ok := coerceGolden[in.name]
			if !assert.True(t, ok, "no recording for %q", in.name) {
				return
			}
			f64, f64err := toFloat64E(in.v)
			i64, i64err := toInt64E(in.v)
			got := coerceWant{
				Bool: toBool(in.v), Int64: toInt64(in.v), Int32: toInt32(in.v), Int16: toInt16(in.v), Int8: toInt8(in.v), Int: toInt(in.v),
				Uint: toUint(in.v), Uint64: toUint64(in.v), Uint32: toUint32(in.v), Uint16: toUint16(in.v), Uint8: toUint8(in.v),
				Float32: toFloat32(in.v), Float64: toFloat64(in.v), String: toString(in.v),
				Float64E: f64, Float64OK: f64err == nil, Int64E: i64, Int64OK: i64err == nil,
				StringSlice: toStringSlice(in.v), StringMap: toStringMap(in.v),
			}
			assert.Equal(t, want, got)
		})
	}
}

// TestParseHelpersUseCoercion pins that the exported Parse* wrappers go
// through the same coercion, with the argument map a tool call decodes to.
func TestParseHelpersUseCoercion(t *testing.T) {
	req := CallToolRequest{}
	req.Params.Arguments = map[string]any{"n": "42", "b": "1", "f": "2.5", "s": 7, "m": `{"a":1}`}
	assert.Equal(t, int64(42), ParseInt64(req, "n", 0))
	assert.Equal(t, 42, ParseInt(req, "n", 0))
	assert.Equal(t, uint8(42), ParseUInt8(req, "n", 0))
	assert.True(t, ParseBoolean(req, "b", false))
	assert.Equal(t, 2.5, ParseFloat64(req, "f", 0))
	assert.Equal(t, "7", ParseString(req, "s", ""))
	assert.Equal(t, map[string]any{"a": float64(1)}, ParseStringMap(req, "m", nil))
	assert.Equal(t, int64(9), ParseInt64(req, "missing", 9), "the default comes back through the same coercion")
}
