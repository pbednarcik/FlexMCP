package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// benchPayloads are bench/compare's tools/call messages.
var benchPayloads = []string{"modern-small.json", "modern-complex.json"}

// benchPayloadParams returns the params of one of bench/compare's tools/call
// messages and the arguments in them.
func benchPayloadParams(t testing.TB, payload string) (params, arguments []byte) {
	t.Helper()
	msg, err := os.ReadFile(filepath.Join("..", "bench", "compare", "harness", "testdata", payload))
	require.NoError(t, err)
	var envelope struct {
		Params json.RawMessage `json:"params"`
	}
	require.NoError(t, json.Unmarshal(msg, &envelope))
	var inner struct {
		Arguments json.RawMessage `json:"arguments"`
	}
	require.NoError(t, json.Unmarshal(envelope.Params, &inner))
	require.NotEmpty(t, inner.Arguments)
	return envelope.Params, inner.Arguments
}

// nested returns depth objects, or arrays, one inside the other.
func nested(depth int, arrays bool) string {
	if arrays {
		return strings.Repeat(`[`, depth) + strings.Repeat(`]`, depth)
	}
	return strings.Repeat(`{"a":`, depth) + `1` + strings.Repeat(`}`, depth)
}

// sizedArray returns an array of one string, n bytes in all.
func sizedArray(n int) string {
	return `["` + strings.Repeat("x", n-4) + `"]`
}

// argumentsDiffCorpus is the differential corpus of a tool call's arguments:
// bench/compare's payloads, the _meta corpus, values that are not objects,
// and the inputs where a hand decoder could part from encoding/json.
func argumentsDiffCorpus(t testing.TB) []string {
	t.Helper()
	long := func(n int) string { return strings.Repeat("s", n) }
	corpus := []string{
		// not an object
		`[]`, ` [ ] `, `[[]]`, `[{}]`, `[[],[[]],{}]`, `[1,"a",null,true,false,{"b":[]}]`, `"x"`, `""`, `"\u00e9"`,
		`0`, `-0`, `-0.0`, `-0e0`, `1.5`, `1e2`, `1E+2`, `-7`, `true`, `false`, `null`, ` null `, "\t\n1\r\n",
		// malformed, or not one value
		``, ` `, `[`, `]`, `[1,]`, `[1 2]`, `{"a":1}x`, `[] []`, `tru`, `nul`, `"\x"`, `01`, `.5`, `-`, `1e`, `+1`,
		`{"a":1,}`, `{"a" 1}`, `{a:1}`, `"\u12"`, "\"\x01\"", `[1]]`, `}`,
		// numbers: 308 and 309 digits, a float64's ends, -0 everywhere
		strings.Repeat("9", 308), strings.Repeat("9", 309), `-` + strings.Repeat("9", 308), `-` + strings.Repeat("9", 309),
		`1` + strings.Repeat("0", 308), `2` + strings.Repeat("0", 308), `[` + strings.Repeat("9", 309) + `]`,
		`1.7976931348623157e308`, `1.7976931348623159e308`, `1e308`, `1e309`, `-1e309`, `4.9e-324`, `5e-325`, `1e-400`,
		`0.1`, `9007199254740993`, `123456789012345678901234567890`, `0e0`, `-0E-0`, `1e-0`, `0.0000000000000000000001`,
		`{"a":-0,"b":[-0,0,-0.0,[-0e0]],"c":{"d":-0}}`, `[-0]`, `{"a":1e400}`, `{"a":1,"b":1e400,"c":2}`, `[1,1e400,2]`,
		// escapes and surrogates, in values and in keys
		`"\"\\\/\b\f\n\r\t"`, `"\ud83d\ude00"`, `"\ud800"`, `"\udc00"`, `"\ud800\u0041"`, `"\ud83d"`, `"\ud83dx"`,
		`{"\u00e9t\u00e9":"caf\u00e9","\ud800":"\udc00"}`, `["<>&\u2028\u2029","\u0000"]`, `{"\u0061":1,"a":2}`,
		// invalid UTF-8, raw and beside an escape
		"\"\xff\"", "[\"ok\xc3\"]", "{\"\xff\":1}", "\"\xed\xa0\x80\"", "\"\\n\xff\"", "{\"a\":[\"\xfe\"]}",
		// duplicate names, the last one winning, at every level
		`{"a":1,"a":2}`, `{"x":{"a":1,"a":2}}`, `{"x":[{"a":{"b":1,"b":2}}]}`, `[{"a":1,"a":2}]`, `[[{"a":[{"b":1,"b":{"c":2}}]}]]`,
		`{"a":{"b":1},"a":2}`, `{"a":2,"a":{"b":1}}`, `{"a":[1],"a":[]}`, `{"a":{"x":1},"a":{"y":2}}`,
		`{"x":{"y":{"z":{"a":1,"a":-0}}}}`, `{"a":1,"\u0061":-0}`,
		// deep nesting either side of the limit both decoders share
		nested(9999, false), nested(10000, false), nested(10001, false),
		nested(9999, true), nested(10000, true), nested(10001, true),
		`{"a":` + nested(9998, true) + `}`, `{"a":` + nested(9999, true) + `}`, `{"a":` + nested(10000, true) + `}`,
		// strings either side of the longest a decoder keeps, each read twice
		`["` + long(63) + `","` + long(63) + `"]`, `["` + long(64) + `","` + long(64) + `"]`, `["` + long(65) + `","` + long(65) + `"]`,
		`{"` + long(64) + `":"` + long(64) + `","` + long(65) + `":"` + long(65) + `","k":"` + long(64) + `"}`,
		`["` + long(62) + `\n","` + long(62) + `\n","` + long(63) + `\n"]`,
		// either side of the longest input the single pass reads
		sizedArray(maxFastInput - 1), sizedArray(maxFastInput), sizedArray(maxFastInput + 1),
		// many keys
		`{` + strings.Repeat(`"k":1,`, 50) + `"z":2}`,
	}
	for _, payload := range benchPayloads {
		_, arguments := benchPayloadParams(t, payload)
		corpus = append(corpus, string(arguments))
	}
	return append(corpus, metaDiffCorpus...)
}

// callToolParamsBefore decodes data as CallToolParams.UnmarshalJSON did before
// the arguments were read in one pass.
func callToolParamsBefore(data []byte) (CallToolParams, error) {
	type params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      *Meta           `json:"_meta,omitempty"`
		Task      *TaskParams     `json:"task,omitempty"`
		MultiRoundTripParams
	}
	var p CallToolParams
	var raw params
	if err := json.Unmarshal(data, &raw); err != nil {
		return p, err
	}
	p.Name = raw.Name
	p.Meta = raw.Meta
	p.Task = raw.Task
	p.MultiRoundTripParams = raw.MultiRoundTripParams
	if len(raw.Arguments) == 0 {
		return p, nil
	}
	p.RawArguments = raw.Arguments
	return p, json.Unmarshal(raw.Arguments, &p.Arguments)
}

// checkCallToolParamsDiff requires CallToolParams.UnmarshalJSON to give for
// data the params and the error the decode before it gave: the same
// Arguments, float64s bit for bit, the same RawArguments, the same text.
func checkCallToolParamsDiff(t *testing.T, data []byte) {
	t.Helper()
	want, wantErr := callToolParamsBefore(data)
	var got CallToolParams
	gotErr := got.UnmarshalJSON(data)
	if wantErr != nil {
		require.EqualError(t, gotErr, wantErr.Error(), "%q", data)
	} else {
		require.NoError(t, gotErr, "%q", data)
	}
	require.Equal(t, want, got, "%q", data)
	requireSameFloatBits(t, want.Arguments, got.Arguments, data)
}

// checkArgumentsDiff requires the single pass to give for arguments what
// json.Unmarshal into an any gives, whenever it settles them, and the params
// that carry them to decode as before; it reports whether the pass settled
// them.
func checkArgumentsDiff(t *testing.T, arguments []byte) bool {
	t.Helper()
	var want any
	wantErr := json.Unmarshal(arguments, &want)
	got, fast := decodeArgumentsFast(arguments)
	if fast {
		require.NoError(t, wantErr, "%q: the single pass settled what json.Unmarshal refuses", arguments)
		require.Equal(t, want, got, "%q", arguments)
		requireSameFloatBits(t, want, got, arguments)
	}
	params := make([]byte, 0, len(arguments)+64)
	params = append(params, `{"name":"echo","arguments":`...)
	params = append(params, arguments...)
	params = append(params, '}')
	checkCallToolParamsDiff(t, params)
	return fast
}

// A tool call's arguments decode as they did before the single pass, for
// every input: the same Arguments, RawArguments and error, and the value
// json.Unmarshal into an any builds.
func TestArgumentsFastMatchesSlow(t *testing.T) {
	corpus := argumentsDiffCorpus(t)
	fast := 0
	for _, s := range corpus {
		checkArgumentsDiff(t, []byte(s))
		// Again: the pooled decoder may come from a decode that failed.
		if checkArgumentsDiff(t, []byte(s)) {
			fast++
		} else {
			t.Logf("slow path: %.60q", s)
		}
	}
	t.Logf("%d of %d corpus inputs took the fast path", fast, len(corpus))
	assert.GreaterOrEqual(t, fast, 139, "corpus inputs that took the fast path")

	for _, payload := range benchPayloads {
		params, arguments := benchPayloadParams(t, payload)
		_, ok := decodeArgumentsFast(arguments)
		assert.True(t, ok, "the arguments of %s fell back to the slow path", payload)
		checkCallToolParamsDiff(t, params)
	}
	for _, params := range []string{
		`{"name":"echo"}`, `{"name":"echo","arguments":null}`, `{"name":"echo","arguments":{}}`,
		`{"name":"echo","arguments":[1],"arguments":{"a":1}}`, `{"name":"echo","arguments":{"a":1},"arguments":null}`,
		`{"name":"echo","arguments":{"a":1e400,"b":2}}`, `{"name":"echo","arguments":{"a":1}}x`, `[]`, `null`, `{"name":5}`,
		`{"name":"echo","arguments":{"a":"\ud800","b":"` + "\xff" + `"},"_meta":{"progressToken":7}}`,
	} {
		checkCallToolParamsDiff(t, []byte(params))
	}
}

// The single pass reads an input of up to maxFastInput bytes; a longer one
// goes to json.Unmarshal.
func TestArgumentsFastInputBound(t *testing.T) {
	_, ok := decodeArgumentsFast([]byte(sizedArray(maxFastInput)))
	assert.True(t, ok, "an input of maxFastInput bytes")
	_, ok = decodeArgumentsFast([]byte(sizedArray(maxFastInput + 1)))
	assert.False(t, ok, "an input past maxFastInput")
}

// Each decode gives maps and slices of its own: writing one does not reach a
// later decode.
func TestArgumentsUnmarshalResultIsOwned(t *testing.T) {
	data := []byte(`{"name":"echo","arguments":{"a":{"b":"c"},"l":["x",{"y":"z"},[]],"s":"t"}}`)
	var first CallToolParams
	require.NoError(t, first.UnmarshalJSON(data))
	arguments := first.Arguments.(map[string]any)
	arguments["a"].(map[string]any)["b"] = "changed"
	arguments["l"].([]any)[0] = "changed"
	arguments["l"].([]any)[1].(map[string]any)["y"] = "changed"
	arguments["l"].([]any)[2] = append(arguments["l"].([]any)[2].([]any), "added")
	arguments["new"] = true

	var second CallToolParams
	require.NoError(t, second.UnmarshalJSON(data))
	assert.Equal(t, map[string]any{
		"a": map[string]any{"b": "c"},
		"l": []any{"x", map[string]any{"y": "z"}, []any{}},
		"s": "t",
	}, second.Arguments)
}

// argumentsThrashParams are tools/call params of the same 300 argument names
// with values new on each call: more strings per call than a decoder keeps,
// so it forgets the names before it reads them again.
func argumentsThrashParams() [][]byte {
	bodies := make([][]byte, 64)
	for b := range bodies {
		var body strings.Builder
		body.WriteString(`{"name":"echo","arguments":{`)
		for i := range 300 {
			if i > 0 {
				body.WriteString(`,`)
			}
			fmt.Fprintf(&body, `"k%d":"v%d_%d"`, i, b, i)
		}
		body.WriteString(`}}`)
		bodies[b] = []byte(body.String())
	}
	return bodies
}

// Arguments a decoder no longer keeps cost no more than json.Unmarshal into
// an any: a name is kept as a string, never boxed.
func TestArgumentsUnmarshalNewValuesAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	bodies := argumentsThrashParams()
	i := 0
	fast := testing.AllocsPerRun(1000, func() {
		var p CallToolParams
		_ = p.UnmarshalJSON(bodies[i%len(bodies)])
		i++
	})
	slow := testing.AllocsPerRun(1000, func() {
		_, _ = callToolParamsBefore(bodies[i%len(bodies)])
		i++
	})
	assert.LessOrEqual(t, fast, slow, "allocations per tools/call params of 300 new values, against json.Unmarshal")
}

// Arguments have decoders, and so string caches, of their own: calls whose
// arguments are all new do not push out the strings a client repeats in every
// _meta, so the captured Claude Code _meta read between them costs what it
// costs alone.
func TestArgumentsChurnKeepsMetaStrings(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	bodies := argumentsThrashParams()
	meta := []byte(metaClaudeCode)
	i := 0
	churn := func() {
		var p CallToolParams
		_ = p.UnmarshalJSON(bodies[i%len(bodies)])
		i++
	}
	runs := 16 * len(bodies)
	alone := testing.AllocsPerRun(runs, churn)
	i = 0
	withMeta := testing.AllocsPerRun(runs, func() {
		churn()
		var m Meta
		_ = m.UnmarshalJSON(meta)
	})
	assert.LessOrEqual(t, withMeta-alone, 12.0, "allocations per Claude Code _meta read between tools/call params of new arguments")
}

// Arguments past maxFastInput go straight to json.Unmarshal: a large
// input that fails at its end costs what json.Unmarshal's own decode costs,
// not a pass that builds every value before the error.
func TestArgumentsUnmarshalLargeInvalidAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	data := []byte(`{"name":"echo","arguments":[` + strings.Repeat(`1,`, maxFastInput) + `1e400]}`)
	fast := testing.AllocsPerRun(20, func() {
		var p CallToolParams
		_ = p.UnmarshalJSON(data)
	})
	slow := testing.AllocsPerRun(20, func() { _, _ = callToolParamsBefore(data) })
	assert.LessOrEqual(t, fast, slow, "allocations per tools/call params of large arguments that fail at their end, against json.Unmarshal")
}

// The arguments of bench/compare's payloads are read in one pass: 18 and 580
// allocations per params decode through json.Unmarshal into an any, 11 and
// 164 in one pass with the strings a client repeats kept by the decoder.
func TestCallToolParamsBenchPayloadAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	for _, tc := range []struct {
		payload string
		ceiling float64
	}{
		{"modern-small.json", 11},
		{"modern-complex.json", 164},
	} {
		t.Run(tc.payload, func(t *testing.T) {
			params, _ := benchPayloadParams(t, tc.payload)
			allocs := testing.AllocsPerRun(1000, func() {
				var p CallToolParams
				_ = json.Unmarshal(params, &p)
			})
			assert.LessOrEqual(t, allocs, tc.ceiling, "allocations per tools/call params read of %s", tc.payload)
		})
	}
}

// FuzzArgumentsFastMatchesSlow holds the differential for any arguments, from
// the corpus up.
func FuzzArgumentsFastMatchesSlow(f *testing.F) {
	for _, s := range argumentsDiffCorpus(f) {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		checkArgumentsDiff(t, data)
	})
}
