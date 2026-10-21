package mcp

import (
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
	"math"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"weak"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// metaClaudeCode is the _meta of a tools/call sent by Claude Code 2.1.303,
// as captured, its tool-use id replaced by a made-up one of the same length.
const metaClaudeCode = `{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"claude-code","title":"Claude Code","version":"2.1.303","description":"Anthropic's agentic coding tool","websiteUrl":"https://claude.com/claude-code"},"io.modelcontextprotocol/clientCapabilities":{"roots":{"listChanged":true},"elicitation":{"form":{},"url":{}}},"claudecode/toolUseId":"toolu_01ABCDEFGHJKMNPQRSTUVWXY","progressToken":1}`

// metaDiffCorpus is the differential corpus: the _meta shapes clients send and
// the inputs where a hand decoder could part from encoding/json.
var metaDiffCorpus = []string{
	// what clients send: Claude Code, the FlexMCP client, a heavier host, an
	// unknown key
	metaClaudeCode,
	`{"io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"test-client","version":"1.0.0"},"io.modelcontextprotocol/protocolVersion":"2026-07-28","progressToken":"p-1"}`,
	`{"io.modelcontextprotocol/clientCapabilities":{"elicitation":{},"roots":{},"sampling":{}},"io.modelcontextprotocol/clientInfo":{"name":"mcp-go","version":"(devel)"},"io.modelcontextprotocol/protocolVersion":"2026-07-28"}`,
	`{"io.modelcontextprotocol/clientCapabilities":{"elicitation":{"form":{},"url":{}},"extensions":{"io.modelcontextprotocol/ui":{"mimeTypes":["text/html;profile=mcp-app"]}},"roots":{},"sampling":{"context":{},"tools":{}}},"io.modelcontextprotocol/clientInfo":{"name":"claude-code","title":"Claude Code","version":"2.1.97"},"io.modelcontextprotocol/protocolVersion":"2026-07-28","progressToken":7}`,
	`{"io.modelcontextprotocol/protocolVersion":"2025-11-25","io.modelcontextprotocol/logLevel":"warning","io.modelcontextprotocol/clientCapabilities":{}}`,
	`{"claudecode/toolUseId":"toolu_01ABCDEFGHJKMNPQRSTUVWXYZ","progressToken":"p-1"}`,
	// empty, null and whitespace
	`{}`, ` { } `, "\t{\n\"a\" : 1 ,\r\n\"b\":[ ]}\n", `null`, ` null `, `{"a":null}`, `{"a":{},"b":[],"c":""}`,
	`{"progressToken":null}`, `{"progressToken":""}`, `{"progressToken":{}}`, `{"progressToken":[]}`, `{"progressToken":true}`,
	// not an object, trailing data, malformed
	`[]`, `"x"`, `1`, `true`, ``, ` `, `{`, `{"a":1`, `{"a":1}x`, `{"a":1} {}`, `{"a":1,}`, `{"a" 1}`, `{a:1}`, `{"a":01}`, `{"a":.5}`,
	`{"a":1e}`, `{"a":-}`, `{"a":tru}`, `{"a":"\x"}`, `{"a":"\u12"}`, "{\"a\":\"\x01\"}", `{1:2}`, `{"a":[1,]}`, `{"a":[1 2]}`,
	// deep nesting, either side of the limit both decoders share
	strings.Repeat(`{"a":`, 200) + `1` + strings.Repeat(`}`, 200),
	`{"a":` + strings.Repeat(`[`, 300) + strings.Repeat(`]`, 300) + `}`,
	strings.Repeat(`{"a":`, 9999) + `1` + strings.Repeat(`}`, 9999),
	strings.Repeat(`{"a":`, 10000) + `1` + strings.Repeat(`}`, 10000),
	strings.Repeat(`{"a":`, 10001) + `1` + strings.Repeat(`}`, 10001),
	`{"a":` + strings.Repeat(`[`, 10001) + strings.Repeat(`]`, 10001) + `}`,
	// unicode and escapes
	`{"\u00e9t\u00e9":"caf\u00e9","emoji":"\ud83d\ude00","raw":"Ω≈ç√","esc":"\"\\\/\b\f\n\r\t"}`,
	`{"lone":"\ud800"}`, `{"lone2":"\udc00x"}`, `{"\ud800":1}`, `{"pair":"\ud83d"}`,
	`{"\u0070rogressToken":5}`, `{"progress\u0054oken":"x"}`, `{"name":"<>&\u2028\u2029"}`, `{"\u006eame":"\u0063"}`,
	// invalid UTF-8 in values and keys, raw and beside an escape
	"{\"a\":\"\xff\"}", "{\"\xfe\":1}", "{\"a\":\"ok\xc3\"}", "{\"a\":\"\xed\xa0\x80\"}", "{\"a\":\"\\n\xff\"}", "{\"\\n\xff\":1}",
	// duplicate keys: the last wins
	`{"a":1,"a":2}`, `{"a":{"x":1},"a":{"y":2}}`, `{"a":[1],"a":{"z":true}}`, `{"a":{"x":{"y":1},"x":{"z":2}}}`,
	`{"progressToken":"a","progressToken":5}`, `{"progressToken":5,"progressToken":"a"}`, `{"progressToken":1,"progressToken":2}`,
	// keys the slow path's case-insensitive re-read of a numeric token matches
	`{"progressToken":1,"ProgressToken":"x"}`, `{"PROGRESSTOKEN":3}`, `{"progresstoken":3,"progressToken":4}`,
	`{"progressToken":4,"progresstoken":3}`, `{"progreſsToken":1,"progressToken":2}`, `{"progressto\u212aen":1,"progressToken":2}`,
	// keys it does not match: delimiters count
	`{"progress_token":1,"progressToken":2}`, `{"progressToken":2,"progress-token":1}`,
	// numbers
	`{"a":0,"b":-0,"c":1.5,"d":-2,"e":1e2,"f":1E-2,"g":12345678901234567890,"h":1.7976931348623157e308}`,
	`{"a":1e400}`, `{"a":-1e400}`, `{"a":1e-400}`, `{"a":4.9e-324}`, `{"a":123456789012345678901234567890}`,
	`{"progressToken":12345678901234567890}`, `{"progressToken":9223372036854775807}`, `{"progressToken":-9223372036854775808}`,
	`{"progressToken":9223372036854775808}`, `{"progressToken":1.5}`, `{"progressToken":1e2}`, `{"progressToken":-0}`,
	`{"progressToken":0}`, `{"progressToken":1e400}`, `{"progressToken":-7}`, `{"progressToken":100000000000000000000000}`,
	`{"progressToken":300}`, `{"progressToken" : 42 }`, `{"progressToken":-0.0}`, `{"progressToken":-0e0}`,
	// negative zero, which reflect.DeepEqual takes for zero: nested and in
	// arrays
	`{"a":{"b":-0,"c":{"d":-0.0}},"e":[-0,0,-0e0,[-0]],"f":[{"g":-0}]}`,
	// integers of 308 and 309 digits: an int64 cannot hold either, a float64
	// holds the first and not the second, so the slow path fails on it
	`{"progressToken":` + strings.Repeat("9", 308) + `}`,
	`{"progressToken":2` + strings.Repeat("0", 308) + `}`,
	`{"progressToken":-2` + strings.Repeat("0", 308) + `}`,
	`{"a":2` + strings.Repeat("0", 308) + `}`,
	// arrays of everything
	`{"a":[null,true,false,0,"s",{},[],{"k":[1,"2",{"x":null}]}]}`,
	// long strings and many keys
	`{"long":"` + strings.Repeat("x", 300) + `","k":"` + strings.Repeat("y", 64) + `","k2":"` + strings.Repeat("z", 65) + `"}`,
	`{` + strings.Repeat(`"k":1,`, 50) + `"z":2}`,
}

// slowMeta decodes data as Meta.UnmarshalJSON did before the fast pass.
func slowMeta(data []byte) (*Meta, error) {
	m := &Meta{}
	return m, m.unmarshalJSONSlow(data)
}

// checkMetaDiff requires Meta.UnmarshalJSON to give for data the Meta and the
// error unmarshalJSONSlow gives, and the map json.Unmarshal gives, and
// reports whether the fast pass settled data on its own.
func checkMetaDiff(t *testing.T, data []byte) bool {
	t.Helper()
	want, wantErr := slowMeta(data)
	got := &Meta{}
	gotErr := got.UnmarshalJSON(data)
	if wantErr != nil {
		require.EqualError(t, gotErr, wantErr.Error(), "%q", data)
	} else {
		require.NoError(t, gotErr, "%q", data)
		require.Equal(t, want, got, "%q", data)
		requireSameFloatBits(t, want.AdditionalFields, got.AdditionalFields, data)
		requireSameFloatBits(t, want.ProgressToken, got.ProgressToken, data)
	}

	// The contract in its own terms: json.Unmarshal into map[string]any, the
	// progressToken key taken out.
	generic := make(map[string]any)
	genericErr := json.Unmarshal(data, &generic)
	require.Equal(t, genericErr == nil, gotErr == nil, "%q", data)
	if genericErr == nil {
		token := generic["progressToken"]
		delete(generic, "progressToken")
		require.Equal(t, generic, got.AdditionalFields, "%q", data)
		requireSameFloatBits(t, generic, got.AdditionalFields, data)
		switch f, number := token.(float64); {
		case foldsToProgressToken(generic):
			// A numeric token is re-read by a case-insensitive match, so a
			// folded key may give it: the slow path's comparison pins it.
		case number:
			require.True(t, sameNumber(got.ProgressToken, f), "%q: token %#v, want the number %v", data, got.ProgressToken, f)
		default:
			require.Equal(t, token, got.ProgressToken, "%q", data)
		}
	}
	return (&Meta{}).unmarshalFast(data)
}

// requireSameFloatBits requires got, a decoded value of want's shape, to hold
// every float64 want holds bit for bit: reflect.DeepEqual takes -0 for 0.
func requireSameFloatBits(t *testing.T, want, got any, data []byte) {
	t.Helper()
	switch w := want.(type) {
	case float64:
		g, ok := got.(float64)
		require.True(t, ok, "%q: %#v, want a float64", data, got)
		require.Equal(t, math.Float64bits(w), math.Float64bits(g), "%q: %v, want %v", data, g, w)
	case map[string]any:
		g, ok := got.(map[string]any)
		require.True(t, ok, "%q: %#v, want a map", data, got)
		require.Len(t, g, len(w), "%q", data)
		for key, value := range w {
			requireSameFloatBits(t, value, g[key], data)
		}
	case []any:
		g, ok := got.([]any)
		require.True(t, ok, "%q: %#v, want a slice", data, got)
		require.Len(t, g, len(w), "%q", data)
		for i, value := range w {
			requireSameFloatBits(t, value, g[i], data)
		}
	}
}

// foldsToProgressToken reports whether a key of m other than progressToken
// equals it under case folding.
func foldsToProgressToken(m map[string]any) bool {
	for key := range m {
		if strings.EqualFold(key, "progressToken") {
			return true
		}
	}
	return false
}

// sameNumber reports whether token, a progress token read from a number, is
// the number json.Unmarshal reads as f.
func sameNumber(token ProgressToken, f float64) bool {
	switch v := token.(type) {
	case int64:
		return float64(v) == f
	case json.Number:
		g, err := v.Float64()
		return err == nil && g == f
	case float64:
		return v == f
	}
	return false
}

// Meta.UnmarshalJSON gives what the decoder before it gave, for every input:
// the same Meta, the same error, and the map json.Unmarshal builds.
func TestMetaFastMatchesSlow(t *testing.T) {
	fast := 0
	for _, s := range metaDiffCorpus {
		checkMetaDiff(t, []byte(s))
		// Again: the pooled decoder may come from a decode that failed.
		if checkMetaDiff(t, []byte(s)) {
			fast++
		} else {
			t.Logf("slow path: %.60q", s)
		}
	}
	t.Logf("%d of %d corpus inputs took the fast path", fast, len(metaDiffCorpus))
	assert.True(t, (&Meta{}).unmarshalFast([]byte(metaClaudeCode)), "the captured Claude Code _meta fell back to the slow path")
	assert.GreaterOrEqual(t, fast, 53, "corpus inputs that took the fast path")
}

// Each decode gives maps and slices of its own: writing one does not reach a
// later decode.
func TestMetaUnmarshalResultIsOwned(t *testing.T) {
	data := []byte(`{"a":{"b":"c"},"l":["x",{"y":"z"}],"s":"t"}`)
	require.True(t, (&Meta{}).unmarshalFast(data))
	var first Meta
	require.NoError(t, first.UnmarshalJSON(data))
	first.AdditionalFields["a"].(map[string]any)["b"] = "changed"
	first.AdditionalFields["l"].([]any)[0] = "changed"
	first.AdditionalFields["l"].([]any)[1].(map[string]any)["y"] = "changed"
	first.AdditionalFields["new"] = true

	var second Meta
	require.NoError(t, second.UnmarshalJSON(data))
	assert.Equal(t, map[string]any{
		"a": map[string]any{"b": "c"},
		"l": []any{"x", map[string]any{"y": "z"}},
		"s": "t",
	}, second.AdditionalFields)
}

// A fast decode reads its input in place, but nothing it returns shares the
// input's bytes: overwriting the input afterwards changes no result, for a
// _meta or for a tool call's arguments.
func TestFastDecodeResultsDoNotAliasTheInput(t *testing.T) {
	long := strings.Repeat("L", 2*maxKeptString)
	doc := `{"progressToken":123456789012345678901234567890,"k":"v","long":"` + long +
		`","esc":"a\nb","n":1.5,"o":{"in":["x","` + long + `"]}}`
	overwrite := func(b []byte) {
		for i := range b {
			b[i] = 'X'
		}
	}

	t.Run("_meta", func(t *testing.T) {
		data := []byte(doc)
		var got Meta
		require.True(t, got.unmarshalFast(data))
		want, err := slowMeta([]byte(doc))
		require.NoError(t, err)
		overwrite(data)
		assert.Equal(t, *want, got)
	})
	t.Run("arguments", func(t *testing.T) {
		data := []byte(doc)
		got, ok := decodeArgumentsFast(data)
		require.True(t, ok)
		var want any
		require.NoError(t, json.Unmarshal([]byte(doc), &want))
		overwrite(data)
		assert.Equal(t, want, got)
	})
}

// A pooled decoder keeps none of its input alive once a decode is done.
func TestFastDecodeKeepsNoInput(t *testing.T) {
	for name, decode := range map[string]func([]byte) bool{
		"_meta": func(data []byte) bool {
			var m Meta
			return m.unmarshalFast(data)
		},
		"arguments": func(data []byte) bool {
			_, ok := decodeArgumentsFast(data)
			return ok
		},
	} {
		t.Run(name, func(t *testing.T) {
			// Past the tiny allocator's size, so the input is an object of its own.
			data := []byte(`{"k":"` + strings.Repeat("v", 100) + `"}`)
			input := weak.Make(&data[0])
			require.True(t, decode(data))
			runtime.GC()
			assert.Nil(t, input.Value(), "the input outlived its decode")
		})
	}
}

// The captured Claude Code _meta is read in one pass: 60 allocations through
// json.Unmarshal into a map and a second read for the numeric progress token,
// 25 in one pass with the well-known strings interned, 12 with the strings a
// client repeats kept by the decoder. A fresh toolUseId on each call, as
// Claude Code sends, costs its string and its box: 14.
func TestMetaUnmarshalClaudeCodeAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	const id = "toolu_01ABCDEFGHJKMNPQRSTUVWXY"
	// Sixteen times as many ids as a decoder keeps strings: each one is new
	// to it.
	rotating := make([][]byte, 16*maxKeptStrings)
	for i := range rotating {
		rotating[i] = []byte(strings.Replace(metaClaudeCode, id, fmt.Sprintf("toolu_01ABCDEFGHJKMNPQRSTU%04d", i), 1))
	}
	for _, tc := range []struct {
		name    string
		bodies  [][]byte
		ceiling float64
	}{
		{"the captured toolUseId", [][]byte{[]byte(metaClaudeCode)}, 12},
		{"a fresh toolUseId each call", rotating, 14},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := 0
			allocs := testing.AllocsPerRun(max(1000, len(tc.bodies)), func() {
				var m Meta
				_ = m.UnmarshalJSON(tc.bodies[i%len(tc.bodies)])
				i++
			})
			assert.LessOrEqual(t, allocs, tc.ceiling, "allocations per Meta.UnmarshalJSON of the captured Claude Code _meta")
		})
	}
}

// A decoder keeps at most maxKeptStrings strings, none longer than
// maxKeptString bytes, however many it reads.
func TestMetaStringCacheIsBounded(t *testing.T) {
	d := metaDecoders.New().(*valueDecoder)
	for i := range 4 * maxKeptStrings {
		want := "s" + strconv.Itoa(i)
		got, _, ok := d.text(jsontext.Value(strconv.Quote(want)), true)
		require.True(t, ok)
		require.Equal(t, want, got)
		require.LessOrEqual(t, len(d.strs), maxKeptStrings, "strings kept after %d read", i+1)
	}

	longest := strings.Repeat("y", maxKeptString)
	_, _, _ = d.text(jsontext.Value(strconv.Quote(longest)), true)
	assert.Contains(t, d.strs, longest)
	tooLong := strings.Repeat("z", maxKeptString+1)
	_, boxed, _ := d.text(jsontext.Value(strconv.Quote(tooLong)), true)
	assert.Nil(t, boxed)
	assert.NotContains(t, d.strs, tooLong)
}

// metaThrashBodies are _meta objects of the same 300 keys with values new on
// each call: more strings per call than a decoder keeps, so it forgets the
// keys before it reads them again.
func metaThrashBodies() [][]byte {
	bodies := make([][]byte, 64)
	for b := range bodies {
		var body strings.Builder
		body.WriteString(`{`)
		for i := range 300 {
			if i > 0 {
				body.WriteString(`,`)
			}
			fmt.Fprintf(&body, `"k%d":"v%d_%d"`, i, b, i)
		}
		body.WriteString(`}`)
		bodies[b] = []byte(body.String())
	}
	return bodies
}

// A call of keys a decoder no longer keeps costs no more than json.Unmarshal
// into a map: a key is kept as a string, never boxed.
func TestMetaUnmarshalNewKeysAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	bodies := metaThrashBodies()
	require.True(t, (&Meta{}).unmarshalFast(bodies[0]))
	i := 0
	fast := testing.AllocsPerRun(1000, func() {
		var m Meta
		_ = m.UnmarshalJSON(bodies[i%len(bodies)])
		i++
	})
	slow := testing.AllocsPerRun(1000, func() {
		var m Meta
		_ = m.unmarshalJSONSlow(bodies[i%len(bodies)])
		i++
	})
	assert.LessOrEqual(t, fast, slow, "allocations per Meta.UnmarshalJSON of 300 keys and new values, against the slow decoder")
}

// A _meta past maxFastInput goes straight to the slow decoder: a large
// one that fails at its end costs what json.Unmarshal's own check costs, not
// a pass that builds every value before the error.
func TestMetaUnmarshalLargeInvalidAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	data := []byte(`{"a":[` + strings.Repeat(`1,`, maxFastInput) + `x]}`)
	fast := testing.AllocsPerRun(20, func() {
		var m Meta
		_ = m.UnmarshalJSON(data)
	})
	slow := testing.AllocsPerRun(20, func() {
		var m Meta
		_ = m.unmarshalJSONSlow(data)
	})
	assert.LessOrEqual(t, fast, slow, "allocations per Meta.UnmarshalJSON of a large _meta that fails at its end, against the slow decoder")
}

// FuzzMetaFastMatchesSlow holds the differential for any input, from the
// corpus up.
func FuzzMetaFastMatchesSlow(f *testing.F) {
	for _, s := range metaDiffCorpus {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		checkMetaDiff(t, data)
	})
}
