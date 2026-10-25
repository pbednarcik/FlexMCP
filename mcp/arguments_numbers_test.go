package mcp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/internal/argsdecode"
)

// numbersDiffCorpus is the arguments corpus with the numbers the input
// validator reads from a second read: either side of the 15 digits that are
// read exactly as float64s, exponents in both cases, out of a float64's range,
// a number big.Rat cannot read, and literals a float64 would rewrite.
func numbersDiffCorpus(t testing.TB) []string {
	t.Helper()
	corpus := argumentsDiffCorpus(t)
	for _, n := range []string{
		`123456789012345`, `1234567890123456`, `9007199254740992`, `9007199254740993`, `12345678901234567`,
		`0.00000000000001`, `0.000000000000001`, `0.30000000000000004`, `1.50`, `1.500000000000000`,
		`1e3`, `1E3`, `1e+3`, `1E-3`, `2.5e-7`, `-0e0`, `-0.0E+0`, `0e10000000000000000000`, `1e-1000001`,
		`1e400`, `-1e400`, `4.9e-324`, `3e-324`, `1e308`, `100e-2`, `0.1e1`,
	} {
		corpus = append(corpus, n, `{"n":`+n+`}`, `[`+n+`,[`+n+`]]`, `{"o":{"x":[[1],[`+n+`]]},"n":`+n+`,"n":5}`)
	}
	return corpus
}

// checkNumbersDiff requires the one-pass read for the validator to give for
// arguments what jsonschema.UnmarshalJSON gives, whenever it settles them:
// json.Number strings equal byte for byte, maps and slices alike, empty ones
// empty and not nil. It reports whether the pass settled them.
func checkNumbersDiff(t *testing.T, arguments []byte) bool {
	t.Helper()
	want, wantErr := jsonschema.UnmarshalJSON(bytes.NewReader(arguments))
	got, fast := decodeArgumentsNumbers(arguments)
	if fast {
		require.NoError(t, wantErr, "%q: the single pass settled what jsonschema.UnmarshalJSON refuses", arguments)
		require.Equal(t, want, got, "%q", arguments)
	}
	return fast
}

// The validator's read of a tool call's arguments in one pass builds what
// jsonschema.UnmarshalJSON builds, for every input it settles.
func TestArgumentsNumbersMatchJSONSchema(t *testing.T) {
	corpus := numbersDiffCorpus(t)
	fast := 0
	for _, s := range corpus {
		checkNumbersDiff(t, []byte(s))
		// Again: the pooled decoder may come from a decode that failed.
		if checkNumbersDiff(t, []byte(s)) {
			fast++
		}
	}
	t.Logf("%d of %d corpus inputs took the single pass", fast, len(corpus))
	assert.GreaterOrEqual(t, fast, 250, "corpus inputs that took the single pass")

	for _, tc := range []struct {
		in   string
		want any
	}{
		{`{"n":1E+3,"m":1.50,"z":-0}`, map[string]any{"n": json.Number("1E+3"), "m": json.Number("1.50"), "z": json.Number("-0")}},
		{`{"a":1,"a":2}`, map[string]any{"a": json.Number("2")}},
		{`{"a":[],"b":{}}`, map[string]any{"a": []any{}, "b": map[string]any{}}},
		{`{"limit":0e10000000000000000000,"big":1e400}`, map[string]any{"limit": json.Number("0e10000000000000000000"), "big": json.Number("1e400")}},
	} {
		got, ok := decodeArgumentsNumbers([]byte(tc.in))
		require.True(t, ok, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
	got, ok := argsdecode.Numbers([]byte(`[9007199254740993]`))
	require.True(t, ok)
	assert.Equal(t, []any{json.Number("9007199254740993")}, got, "the server reaches the same read")
}

// The single pass for the validator reads up to maxFastInput bytes, as the
// one for Arguments does.
func TestArgumentsNumbersInputBound(t *testing.T) {
	_, ok := decodeArgumentsNumbers([]byte(sizedArray(maxFastInput)))
	assert.True(t, ok, "an input of maxFastInput bytes")
	_, ok = decodeArgumentsNumbers([]byte(sizedArray(maxFastInput + 1)))
	assert.False(t, ok, "an input past maxFastInput")
}

// Reading numbers for the validator leaves the decoders of Arguments alone:
// they still read numbers as float64s.
func TestArgumentsNumbersKeepToTheirDecoders(t *testing.T) {
	for range 3 {
		_, _ = decodeArgumentsNumbers([]byte(`{"n":1.5}`))
		got, ok := decodeArgumentsFast([]byte(`{"n":1.5}`))
		require.True(t, ok)
		assert.Equal(t, map[string]any{"n": 1.5}, got)
	}
}

func FuzzArgumentsNumbersMatchJSONSchema(f *testing.F) {
	for _, s := range numbersDiffCorpus(f) {
		if len(s) < 4096 && !strings.Contains(s, "[[[[[[") {
			f.Add([]byte(s))
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		checkNumbersDiff(t, data)
	})
}
