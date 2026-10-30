package mcp

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/pbednarcik/flexmcp/internal/argsdecode"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWholeInt(t *testing.T) {
	tests := []struct {
		name   string
		v      any
		bits   int
		text   bool
		want   int64
		reason error
	}{
		{"number 1.0", json.Number("1.0"), 64, false, 1, nil},
		{"number 1e2", json.Number("1e2"), 64, false, 100, nil},
		{"number -0", json.Number("-0"), 64, false, 0, nil},
		{"number 100e-2", json.Number("100e-2"), 64, false, 1, nil},
		{"number shifted into the integer part", json.Number("0.000000000000000000005e21"), 64, false, 5, nil},
		{"number zero mantissa, huge exponent", json.Number("0e999999999999999999999"), 64, false, 0, nil},
		{"number MaxInt64", json.Number("9223372036854775807"), 64, false, math.MaxInt64, nil},
		{"number MinInt64", json.Number("-9223372036854775808"), 64, false, math.MinInt64, nil},
		{"float64 3", float64(3), 64, false, 3, nil},
		{"float64 -0", math.Copysign(0, -1), 64, false, 0, nil},
		{"float32 7", float32(7), 64, false, 7, nil},

		{"fraction 1.5", json.Number("1.5"), 64, false, 0, ErrArgumentFraction},
		{"fraction 1.0000000000000001", json.Number("1.0000000000000001"), 64, false, 0, ErrArgumentFraction},
		{"fraction 1e-1", json.Number("1e-1"), 64, false, 0, ErrArgumentFraction},
		{"fraction 0.5", json.Number("0.5"), 64, false, 0, ErrArgumentFraction},
		{"fraction float64", float64(1.5), 64, false, 0, ErrArgumentFraction},
		{"fraction 1e-5, point before the digits", json.Number("1e-5"), 64, false, 0, ErrArgumentFraction},
		{"fraction -1e-2", json.Number("-1e-2"), 64, false, 0, ErrArgumentFraction},
		{"fraction, saturated negative exponent", json.Number("1e-999999999999999999999"), 64, false, 0, ErrArgumentFraction},

		{"range 1e19", json.Number("1e19"), 64, false, 0, ErrArgumentOutOfRange},
		{"range 2^63", json.Number("9223372036854775808"), 64, false, 0, ErrArgumentOutOfRange},
		{"range -2^63-1", json.Number("-9223372036854775809"), 64, false, 0, ErrArgumentOutOfRange},
		{"range 20 digits", json.Number("10000000000000000000"), 64, false, 0, ErrArgumentOutOfRange},
		{"range huge exponent", json.Number("1e999999999999999999999"), 64, false, 0, ErrArgumentOutOfRange},
		{"range 2^31 at 32", json.Number("2147483648"), 32, false, 0, ErrArgumentOutOfRange},
		{"range float64 1e19", float64(1e19), 64, false, 0, ErrArgumentOutOfRange},

		{"int32 max at 32", json.Number("2147483647"), 32, false, math.MaxInt32, nil},
		{"int32 min at 32", json.Number("-2147483648"), 32, false, math.MinInt32, nil},
		{"int32 min-1 at 32", json.Number("-2147483649"), 32, false, 0, ErrArgumentOutOfRange},
		{"float64 2^31 at 32", float64(2147483648), 32, false, 0, ErrArgumentOutOfRange},
		{"float64 int32 max at 32", float64(2147483647), 32, false, math.MaxInt32, nil},
		{"float64 int32 min at 32", float64(-2147483648), 32, false, math.MinInt32, nil},
		{"int64 2^31 at 32", int64(2147483648), 32, false, 0, ErrArgumentOutOfRange},
		{"uint64 2^31 at 32", uint64(2147483648), 32, false, 0, ErrArgumentOutOfRange},

		{"NaN", math.NaN(), 64, false, 0, ErrArgumentNotANumber},
		{"+Inf", math.Inf(1), 64, false, 0, ErrArgumentOutOfRange},
		{"-Inf", math.Inf(-1), 64, false, 0, ErrArgumentOutOfRange},
		{"int64 -5", int64(-5), 64, false, -5, nil},
		{"int 5", int(5), 64, false, 5, nil},
		{"int8 -5", int8(-5), 64, false, -5, nil},
		{"uint8 200", uint8(200), 64, false, 200, nil},
		{"uint 9", uint(9), 64, false, 9, nil},
		{"uint64 max", uint64(math.MaxUint64), 64, false, 0, ErrArgumentOutOfRange},
		{"uint64 past MaxInt64", uint64(math.MaxInt64) + 1, 64, false, 0, ErrArgumentOutOfRange},
		{"number +5", json.Number("+5"), 64, false, 0, ErrArgumentNotANumber},
		{"number abc", json.Number("abc"), 64, false, 0, ErrArgumentNotANumber},
		{"number empty", json.Number(""), 64, false, 0, ErrArgumentNotANumber},
		{"number leading zero", json.Number("01"), 64, false, 0, ErrArgumentNotANumber},
		{"number bare point", json.Number("1."), 64, false, 0, ErrArgumentNotANumber},
		{"number bare exponent", json.Number("1e"), 64, false, 0, ErrArgumentNotANumber},
		{"number trailing space", json.Number("1 "), 64, false, 0, ErrArgumentNotANumber},
		{"bool", true, 64, false, 0, ErrArgumentNotANumber},
		{"map", map[string]any{}, 64, false, 0, ErrArgumentNotANumber},
		{"slice", []any{}, 64, false, 0, ErrArgumentNotANumber},
		{"nil", nil, 64, false, 0, ErrArgumentNotANumber},

		{"text +5", "+5", 64, true, 5, nil},
		{"text 007", "007", 64, true, 7, nil},
		{"text -0", "-0", 64, true, 0, nil},
		{"text 42", "42", 64, true, 42, nil},
		{"text leading space", " 5", 64, true, 0, ErrArgumentBadText},
		{"text trailing space", "5 ", 64, true, 0, ErrArgumentBadText},
		{"text underscore", "1_000", 64, true, 0, ErrArgumentBadText},
		{"text arabic digits", "١٢", 64, true, 0, ErrArgumentBadText},
		{"text hex", "0x10", 64, true, 0, ErrArgumentBadText},
		{"text exponent", "1e2", 64, true, 0, ErrArgumentBadText},
		{"text fraction", "1.0", 64, true, 0, ErrArgumentBadText},
		{"text empty", "", 64, true, 0, ErrArgumentBadText},
		{"text 2^31 at 32", "2147483648", 32, true, 0, ErrArgumentOutOfRange},
		{"text int32 max at 32", "2147483647", 32, true, math.MaxInt32, nil},
		{"text 2^63 at 64", "9223372036854775808", 64, true, 0, ErrArgumentOutOfRange},

		{"string with text off", "5", 64, false, 0, ErrArgumentNotANumber},
		{"empty string with text off", "", 64, false, 0, ErrArgumentNotANumber},
		{"number with text on", json.Number("1.0"), 64, true, 1, nil},
	}
	for _, row := range tests {
		t.Run(row.name, func(t *testing.T) {
			got, err := wholeInt(row.v, row.bits, row.text)
			if row.reason == nil {
				require.NoError(t, err)
				assert.Equal(t, row.want, got)
				return
			}
			assert.ErrorIs(t, err, row.reason)
			var argErr *ArgumentError
			assert.False(t, errors.As(err, &argErr), "wholeInt returns a sentinel, never an ArgumentError")
			assert.Equal(t, row.want, got)
		})
	}

	t.Run("int 2^31 at 32", func(t *testing.T) {
		if strconv.IntSize != 64 {
			t.Skip("a 32-bit int cannot hold 2^31")
		}
		wide := int64(1) << 31
		_, err := wholeInt(int(wide), 32, false)
		assert.ErrorIs(t, err, ErrArgumentOutOfRange)
	})
}

// TestWholeIntTextMatchesAtoi pins "judged by exactly strconv.Atoi".
func TestWholeIntTextMatchesAtoi(t *testing.T) {
	inputs := []string{
		"+5", "007", "-0", " 5", "5 ", "1_000", "١٢", "0x10", "1e2", "",
		"2147483648", "12", "-7", "9223372036854775807", "9223372036854775808",
		"-9223372036854775808", "-9223372036854775809", "+0", "--1", "+", "-", "1.0",
	}
	for _, s := range inputs {
		want, wantErr := strconv.Atoi(s)
		got, err := wholeInt(s, strconv.IntSize, true)
		switch {
		case wantErr == nil:
			assert.NoError(t, err, "%q", s)
			assert.Equal(t, int64(want), got, "%q", s)
		case errors.Is(wantErr, strconv.ErrRange):
			assert.ErrorIs(t, err, ErrArgumentOutOfRange, "%q", s)
		default:
			assert.ErrorIs(t, err, ErrArgumentBadText, "%q", s)
		}
	}
}

func TestWholeLiteralAllocatesNothing(t *testing.T) {
	allocs := testing.AllocsPerRun(100, func() {
		_, _ = wholeLiteral("0.000000000000000000005e21", 64)
		_, _ = wholeLiteral("1.5", 64)
		_, _ = wholeLiteral("1e999999999999", 64)
		_, _ = wholeLiteral("abc", 64)
	})
	assert.Zero(t, allocs)
}

func TestRequireWholeInt(t *testing.T) {
	req := func(args map[string]any) CallToolRequest {
		return CallToolRequest{Params: CallToolParams{Arguments: args}}
	}

	t.Run("accepts a whole number", func(t *testing.T) {
		got, err := req(map[string]any{"limit": float64(25)}).RequireWholeInt("limit")
		require.NoError(t, err)
		assert.Equal(t, 25, got)
	})

	t.Run("a missing key is missing", func(t *testing.T) {
		_, err := req(map[string]any{"other": 1}).RequireWholeInt("limit")
		assert.ErrorIs(t, err, ErrArgumentMissing)
		var argErr *ArgumentError
		require.ErrorAs(t, err, &argErr)
		assert.Equal(t, "limit", argErr.Key)
	})

	t.Run("a nil argument map is missing", func(t *testing.T) {
		_, err := CallToolRequest{}.RequireWholeInt("limit")
		assert.ErrorIs(t, err, ErrArgumentMissing)
	})

	t.Run("null is missing", func(t *testing.T) {
		_, err := req(map[string]any{"limit": nil}).RequireWholeInt("limit")
		assert.ErrorIs(t, err, ErrArgumentMissing)
	})

	t.Run("a fraction is refused where GetInt truncates", func(t *testing.T) {
		r := req(map[string]any{"limit": float64(1.9)})
		assert.Equal(t, 1, r.GetInt("limit", 0))
		_, err := r.RequireWholeInt("limit")
		assert.ErrorIs(t, err, ErrArgumentFraction)
	})

	t.Run("text is off by default and on for the Text form", func(t *testing.T) {
		r := req(map[string]any{"limit": "+5"})
		_, err := r.RequireWholeInt("limit")
		assert.ErrorIs(t, err, ErrArgumentNotANumber)
		got, err := r.RequireWholeIntText("limit")
		require.NoError(t, err)
		assert.Equal(t, 5, got)
		_, err = req(map[string]any{"limit": " 5"}).RequireWholeIntText("limit")
		assert.ErrorIs(t, err, ErrArgumentBadText)
	})

	t.Run("a large string value is cut in the error", func(t *testing.T) {
		big := strings.Repeat("x", 1<<20)
		for name, call := range map[string]func(CallToolRequest, string) (int, error){
			"text off": CallToolRequest.RequireWholeInt,
			"text on":  CallToolRequest.RequireWholeIntText,
		} {
			_, err := call(req(map[string]any{"limit": big}), "limit")
			require.Error(t, err, name)
			var argErr *ArgumentError
			require.ErrorAs(t, err, &argErr, name)
			assert.Equal(t, "limit", argErr.Key, name)
			assert.LessOrEqual(t, len(argErr.Value), 64, name)
			assert.Contains(t, err.Error(), "limit", name)
			assert.LessOrEqual(t, strings.Count(err.Error(), "x"), 64, name)
			assert.Less(t, len(err.Error()), 300, name)
		}
	})

	t.Run("a number literal is cut in the error", func(t *testing.T) {
		big := json.Number("1" + strings.Repeat("0", 1<<16))
		_, err := req(map[string]any{"limit": big}).RequireWholeInt("limit")
		assert.ErrorIs(t, err, ErrArgumentOutOfRange)
		assert.Less(t, len(err.Error()), 300)
	})

	t.Run("a non-scalar value is not echoed", func(t *testing.T) {
		_, err := req(map[string]any{"limit": map[string]any{"a": strings.Repeat("y", 1000)}}).RequireWholeInt("limit")
		assert.ErrorIs(t, err, ErrArgumentNotANumber)
		var argErr *ArgumentError
		require.ErrorAs(t, err, &argErr)
		assert.Empty(t, argErr.Value)
		assert.NotContains(t, err.Error(), "yyy")
	})

	t.Run("the error names the key and the scalar value", func(t *testing.T) {
		_, err := req(map[string]any{"limit": json.Number("1.5")}).RequireWholeInt("limit")
		assert.ErrorContains(t, err, `"limit"`)
		assert.ErrorContains(t, err, "1.5")
	})

	t.Run("a cut keeps whole UTF-8 characters", func(t *testing.T) {
		_, err := req(map[string]any{"limit": strings.Repeat("é", 100)}).RequireWholeInt("limit")
		var argErr *ArgumentError
		require.ErrorAs(t, err, &argErr)
		assert.LessOrEqual(t, len(argErr.Value), 64)
		assert.True(t, strings.ToValidUTF8(argErr.Value, "") == argErr.Value)
	})
}

// replacedKey is the key "limit" and a U+FFFD, as decoding reads "limit" and
// a byte that is not valid UTF-8.
const replacedKey = "limit\xef\xbf\xbd"

func TestLastTopLevelLiteral(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		key  string
		want string
		ok   bool
	}{
		{"the only occurrence", `{"limit":25}`, "limit", "25", true},
		{"duplicate keys, the last wins", `{"limit":1,"x":2,"limit":9007199254740993}`, "limit", "9007199254740993", true},
		{"a nested key of the same name is skipped", `{"a":{"limit":5},"limit":7}`, "limit", "7", true},
		{"a nested key alone is not found", `{"a":{"limit":5},"b":[{"limit":6}]}`, "limit", "", false},
		{"an escaped key is found", `{"li` + `\` + `u006dit":9007199254740993}`, "limit", "9007199254740993", true},
		{"an escaped quote in a key does not match", `{"li\"mit":1}`, "limit", "", false},
		{"a key inside a string value is skipped", `{"a":"\"limit\":5, {","limit":3}`, "limit", "3", true},
		{"a key inside a string value alone is not found", `{"a":"\"limit\":5"}`, "limit", "", false},
		{"brackets in strings do not confuse the depth", `{"a":["]","}"],"b":{"c":"{"},"limit":4}`, "limit", "4", true},
		{"whitespace around the parts", " {\n\t\"limit\" :\r\n 12 , \"x\":1 }\n", "limit", "12", true},
		{"a string value is returned with its quotes", `{"limit":"5"}`, "limit", `"5"`, true},
		{"an exponent literal", `{"limit":1e2}`, "limit", "1e2", true},
		{"a negative literal at the end", `{"a":1,"limit":-7}`, "limit", "-7", true},
		{"an absent key", `{"other":1}`, "limit", "", false},
		{"not an object", `[1,2]`, "limit", "", false},
		{"a scalar", `5`, "limit", "", false},
		{"empty", ``, "limit", "", false},
		{"an empty object", `{}`, "limit", "", false},
		{"truncated", `{"limit":`, "limit", "", false},
		{"the key is empty", `{"":3}`, "", "3", true},
		{"a key of invalid UTF-8 reads as the replacement character", "{\"limit\xff\":9}", replacedKey, "9", true},
		{"a truncated rune reads as replacement characters", "{\"limit\xe2\x82\":9}", "limit\xef\xbf\xbd\xef\xbf\xbd", "9", true},
		{"a key of invalid UTF-8 is not the same key without it", "{\"limit\xff\":9}", "limit", "", false},
		{"duplicates, the invalid spelling last", "{\"" + replacedKey + "\":1,\"limit\xff\":2}", replacedKey, "2", true},
		{"duplicates, the invalid spelling first", "{\"limit\xff\":1,\"" + replacedKey + "\":2}", replacedKey, "2", true},
		{"duplicates, two invalid spellings", "{\"limit\xff\":1,\"limit\xfe\":2}", replacedKey, "2", true},
		{"duplicates, an escaped spelling last", "{\"limit\xff\":1,\"limit" + `\` + "ufffd\":2}", replacedKey, "2", true},
		{"duplicates, a lone surrogate last", "{\"" + replacedKey + "\":1,\"limit" + `\` + "ud800\":2}", replacedKey, "2", true},
		{"an escape beside invalid UTF-8", "{\"l" + `\` + "u0069mit\xff\":9}", replacedKey, "9", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := lastTopLevelLiteral(json.RawMessage(tc.raw), tc.key)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}

	t.Run("an unescaped key allocates nothing", func(t *testing.T) {
		raw := json.RawMessage(`{"a":{"limit":5},"b":"x","limit":9007199254740993,"c":[1,2]}`)
		allocs := testing.AllocsPerRun(100, func() { _, _ = lastTopLevelLiteral(raw, "limit") })
		assert.Zero(t, allocs)
	})

	t.Run("a valid key of many bytes allocates nothing", func(t *testing.T) {
		raw := json.RawMessage(`{"é":1,"日本語":2,"limit":3}`)
		allocs := testing.AllocsPerRun(100, func() { _, _ = lastTopLevelLiteral(raw, "日本語") })
		assert.Zero(t, allocs)
	})
}

// keyIs reads a key as decoding does, whatever its bytes: each key below is
// compared with the name the request's own decode gave it.
func TestKeyIsReadsAKeyAsDecodingDoes(t *testing.T) {
	escape := `\` + "u"
	keys := []string{
		"plain", "é", "日本語", "\xef\xbf\xbd",
		"a\xffb", "\xc0\x80", "\xe2\x82", "\xe2\x82a", "\xed\xa0\x80", "\xf4\x90\x80\x80", "\xf0\x9f\x98",
		escape + "0041", escape + "0041\xff", "\xff" + escape + "0041",
		escape + "d800", escape + "dc00", escape + "d83d" + escape + "de00", escape + "d83d" + escape + "0041", escape + "d83d\xff",
	}
	for b := 0x80; b <= 0xff; b++ {
		keys = append(keys, string([]byte{byte(b)}))
	}
	for _, quoted := range keys {
		var p CallToolParams
		require.NoError(t, json.Unmarshal([]byte(`{"name":"x","arguments":{"`+quoted+`":1}}`), &p), "%q", quoted)
		decoded := p.Arguments.(map[string]any)
		require.Len(t, decoded, 1, "%q", quoted)
		for name := range decoded {
			assert.True(t, keyIs([]byte(`"`+quoted+`"`), name), "%q decodes to %q", quoted, name)
			assert.False(t, keyIs([]byte(`"`+quoted+`"`), name+"x"), "%q", quoted)
		}
	}
}

// wholeParams is the params of a call as the server hands them to a handler:
// Arguments the decode of RawArguments, with edit applied to the map when it
// is not nil.
func wholeParams(t *testing.T, raw string, edit func(map[string]any)) CallToolParams {
	t.Helper()
	var p CallToolParams
	require.NoError(t, json.Unmarshal([]byte(`{"name":"x","arguments":`+raw+`}`), &p))
	if edit != nil {
		edit(p.Arguments.(map[string]any))
	}
	return p
}

func TestRequireWholeIntRefinesFromRaw(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("the rows are 64-bit values")
	}
	tests := []struct {
		name string
		raw  string
		edit func(map[string]any)
		want int64
		err  error
	}{
		{"a literal past 2^53", `{"limit":9007199254740993}`, nil, 9007199254740993, nil},
		{"MaxInt64", `{"limit":9223372036854775807}`, nil, math.MaxInt64, nil},
		{"MinInt64", `{"limit":-9223372036854775808}`, nil, math.MinInt64, nil},
		{"the last duplicate wins", `{"limit":1,"limit":9007199254740993}`, nil, 9007199254740993, nil},
		{"a nested key does not stand for the key", `{"a":{"limit":9007199254740993},"limit":9007199254740992}`, nil, 9007199254740992, nil},
		{"an escaped key is the key", `{"li` + `\` + `u006dit":9007199254740993}`, nil, 9007199254740993, nil},
		{"past the range", `{"limit":9223372036854775808}`, nil, 0, ErrArgumentOutOfRange},
		{"a fraction that rounds to a whole number", `{"limit":1.0000000000000001}`, nil, 0, ErrArgumentFraction},
		{"an exponent literal", `{"limit":1e2}`, nil, 100, nil},
		{"a fraction is a fraction", `{"limit":1.9}`, nil, 0, ErrArgumentFraction},
		{"a map value changed after the decode wins", `{"limit":100}`, func(m map[string]any) { m["limit"] = float64(1) }, 1, nil},
		{"a clamp of a large literal wins", `{"limit":9007199254740993}`, func(m map[string]any) { m["limit"] = float64(5) }, 5, nil},
		{"a key added after the decode has no literal", `{"other":1}`, func(m map[string]any) { m["limit"] = float64(1 << 53) }, 1 << 53, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := CallToolRequest{Params: wholeParams(t, tc.raw, tc.edit)}
			got, err := r.RequireWholeInt("limit")
			if tc.err != nil {
				assert.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, int64(got))
		})
	}

	t.Run("a 32-bit build judges the literal against its range", func(t *testing.T) {
		r := CallToolRequest{Params: wholeParams(t, `{"limit":9007199254740993}`, nil)}
		_, _, err := r.wholeArgument("limit", r.GetArguments()["limit"], 32, false)
		assert.ErrorIs(t, err, ErrArgumentOutOfRange)
		r = CallToolRequest{Params: wholeParams(t, `{"limit":2147483647}`, nil)}
		n, _, err := r.wholeArgument("limit", r.GetArguments()["limit"], 32, false)
		require.NoError(t, err)
		assert.Equal(t, int64(math.MaxInt32), n)
	})

	t.Run("the scan allocates nothing for a short literal", func(t *testing.T) {
		r := CallToolRequest{Params: wholeParams(t, `{"a":{"limit":5},"limit":9007199254740993}`, nil)}
		val := r.GetArguments()["limit"]
		allocs := testing.AllocsPerRun(100, func() { _, _, _ = r.wholeArgument("limit", val, 64, false) })
		assert.Zero(t, allocs)
	})
}

// A refusal judged from the client's literal names the literal, cut at 64
// bytes, not the float64 it rounds to, and keeps no hold on the request's
// bytes.
func TestRequireWholeIntNamesTheLiteral(t *testing.T) {
	long := "1." + strings.Repeat("0", 80) + "1"
	for literal, want := range map[string]string{
		"1.0000000000000001":  "1.0000000000000001",
		"12345678901234567.5": "12345678901234567.5",
		long:                  long[:64],
	} {
		p := wholeParams(t, `{"limit":`+literal+`}`, nil)
		_, err := CallToolRequest{Params: p}.RequireWholeInt("limit")
		assert.ErrorIs(t, err, ErrArgumentFraction, literal)
		var argErr *ArgumentError
		require.ErrorAs(t, err, &argErr, literal)
		assert.Equal(t, want, argErr.Value, literal)
		for i := range p.RawArguments {
			p.RawArguments[i] = ' '
		}
		assert.Equal(t, want, argErr.Value, "the value shares the request's bytes")
	}
}

func TestRequireWholeIntMarked(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("the rows are 64-bit values")
	}
	marked := func(v any) CallToolRequest {
		return CallToolRequest{Params: CallToolParams{Arguments: map[string]any{"limit": v}, roundTrip: true}}
	}

	t.Run("just below 2^53 is accepted", func(t *testing.T) {
		got, err := marked(float64(9007199254740991)).RequireWholeInt("limit")
		require.NoError(t, err)
		assert.Equal(t, int64(9007199254740991), int64(got))
		got, err = marked(-float64(9007199254740991)).RequireWholeInt("limit")
		require.NoError(t, err)
		assert.Equal(t, int64(-9007199254740991), int64(got))
	})

	t.Run("2^53 in size is inexact", func(t *testing.T) {
		for _, v := range []float64{1 << 53, -(1 << 53), 1 << 60, -(1 << 62)} {
			_, err := marked(v).RequireWholeInt("limit")
			assert.ErrorIs(t, err, ErrArgumentInexact, v)
			_, err = marked(v).RequireWholeIntText("limit")
			assert.ErrorIs(t, err, ErrArgumentInexact, v)
		}
	})

	t.Run("out of range wins over inexact", func(t *testing.T) {
		for _, v := range []float64{1 << 63, -(1 << 63) * 2, math.Inf(1), 1e300} {
			_, err := marked(v).RequireWholeInt("limit")
			assert.ErrorIs(t, err, ErrArgumentOutOfRange, v)
			assert.NotErrorIs(t, err, ErrArgumentInexact, v)
		}
	})

	t.Run("a fraction is still a fraction", func(t *testing.T) {
		_, err := marked(1.5).RequireWholeInt("limit")
		assert.ErrorIs(t, err, ErrArgumentFraction)
	})

	t.Run("inexact is for float64 only", func(t *testing.T) {
		wide := int64(1<<53 + 1)
		for _, v := range []any{wide, json.Number("9007199254740993"), uint64(wide), int(wide)} {
			got, err := marked(v).RequireWholeInt("limit")
			require.NoError(t, err, v)
			assert.Equal(t, wide, int64(got), v)
		}
	})

	t.Run("a marked request does not refine from the raw literal", func(t *testing.T) {
		p := wholeParams(t, `{"limit":9007199254740993}`, nil)
		p.roundTrip = true
		_, err := CallToolRequest{Params: p}.RequireWholeInt("limit")
		assert.ErrorIs(t, err, ErrArgumentInexact)

		p = wholeParams(t, `{"limit":1.0000000000000001}`, nil)
		p.roundTrip = true
		got, err := CallToolRequest{Params: p}.RequireWholeInt("limit")
		require.NoError(t, err)
		assert.Equal(t, 1, got)
	})

	t.Run("a 32-bit build: out of range wins", func(t *testing.T) {
		_, _, err := marked(float64(1<<53)).wholeArgument("limit", float64(1<<53), 32, false)
		assert.ErrorIs(t, err, ErrArgumentOutOfRange)
	})
}

func TestRequireWholeIntInProcessIsNeverMarked(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("the rows are 64-bit values")
	}
	t.Run("a request built in process carries no mark and no raw bytes", func(t *testing.T) {
		r := CallToolRequest{Params: CallToolParams{Arguments: map[string]any{"limit": float64(1 << 53)}}}
		got, err := r.RequireWholeInt("limit")
		require.NoError(t, err)
		assert.Equal(t, int64(1<<53), int64(got))
	})

	t.Run("a decoded request carries no mark", func(t *testing.T) {
		p := wholeParams(t, `{"limit":9007199254740992}`, nil)
		assert.False(t, roundTripped(p))
		got, err := CallToolRequest{Params: p}.RequireWholeInt("limit")
		require.NoError(t, err)
		assert.Equal(t, int64(1<<53), int64(got))
	})

	t.Run("the mark is set through argsdecode and a new decode clears it", func(t *testing.T) {
		p := wholeParams(t, `{"limit":9007199254740993}`, nil)
		argsdecode.MarkRoundTrip(&p)
		assert.True(t, roundTripped(p))
		_, err := CallToolRequest{Params: p}.RequireWholeInt("limit")
		assert.ErrorIs(t, err, ErrArgumentInexact)

		require.NoError(t, json.Unmarshal([]byte(`{"name":"x","arguments":{"limit":9007199254740993}}`), &p))
		assert.False(t, roundTripped(p))
		got, err := CallToolRequest{Params: p}.RequireWholeInt("limit")
		require.NoError(t, err)
		assert.Equal(t, int64(9007199254740993), int64(got))
	})
}

// BenchmarkRequireWholeIntRaw measures RequireWholeInt of a whole number past
// 2^53 that the client sent as an integer literal, which the getter reads
// exactly from the raw arguments. The scan walks the 256 KiB beside it: string
// is one string, array is a flat array of numbers.
func BenchmarkRequireWholeIntRaw(b *testing.B) {
	if strconv.IntSize != 64 {
		b.Skip("the literal is a 64-bit value")
	}
	const size = 256 << 10
	for _, leg := range []struct{ name, pad string }{
		{"string", `"` + strings.Repeat("x", size-2) + `"`},
		{"array", "[" + strings.Repeat("1234567,", size/8-1) + "1]"},
	} {
		b.Run(leg.name, func(b *testing.B) {
			raw := `{"pad":` + leg.pad + `,"limit":9007199254740993}`
			var p CallToolParams
			if err := json.Unmarshal([]byte(`{"name":"x","arguments":`+raw+`}`), &p); err != nil {
				b.Fatal(err)
			}
			r := CallToolRequest{Params: p}

			// One call outside the loop proves the getter reads the literal from
			// the raw bytes: the map holds the rounded float64.
			if got, err := r.RequireWholeInt("limit"); err != nil || int64(got) != 9007199254740993 {
				b.Fatalf("leg %s: got %d, %v", leg.name, got, err)
			}

			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			for b.Loop() {
				_, _ = r.RequireWholeInt("limit")
			}
		})
	}
}
