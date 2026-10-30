package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A validated call is validated from the Arguments it decoded whenever every
// number in its RawArguments reads the same there (scanArguments), and
// from a second decode of the bytes otherwise. These tests hold the first to
// the second: the same failures with the same texts for every input, the
// Arguments left as they were.

func TestScanArgumentsReadsNumbersExactly(t *testing.T) {
	for raw, want := range map[string]bool{
		`{"limit":5}`:                    true,
		`123456789012345`:                true,  // 15 digits
		`-999999999999999`:               true,  // 15 digits: the sign is not one
		`99999999999999.9`:               true,  // 15 digits: the point is not one
		`1234567890123456`:               false, // 16 digits
		`9007199254740993`:               false, // 2^53+1
		`12345678901234567`:              false, // 17 digits
		`100000000000000.5`:              false, // 16 digits
		`0.00000000000001`:               true,  // 15 digits, the zeros counted
		`0.000000000000001`:              false, // 16 digits, the zeros counted
		`0.123456789012345`:              false, // 15 significant digits, 16 with the leading zero
		`0.30000000000000004`:            false,
		`1.500000000000000`:              false, // 16 digits, trailing zeros counted
		`0`:                              true,
		`-0`:                             true,
		`-0.0`:                           true,
		`1e3`:                            false,
		`1E3`:                            false,
		`1e-7`:                           false,
		`2.5E+2`:                         false,
		`0e0`:                            false,
		`[1,[2,[3,[1234567890123456]]]]`: false,
		`{"o":{"x":[[0.1],[0.3,12345678901234567]]}}`:        false,
		`{"o":{"x":[[0.1],[0.3,7.5]]}}`:                      true,
		`{"o":{"x":[[1e3]]}}`:                                false,
		`{"s":"1e400 12345678901234567 \" 1E3","n":5}`:       true,
		`{"s\"1e3":"\\","n":1}`:                              true,
		`{"s":"\\\"12345678901234567\\\\","n":1}`:            true,
		`{"s":"\\\\","n":12345678901234567}`:                 false,
		`{"12345678901234567":1}`:                            true,
		`"9007199254740993"`:                                 true,
		`[true,false,null,"1e3"]`:                            true,
		" [ 1 ,\n\t2 , 1234567890123456 ] ":                  false,
		" [ 1 ,\n\t2 ] ":                                     true,
		`{"a":[123456789012345,-123456789012345,0.5]}`:       true,
		`{"a":[123456789012345,-1234567890123456]}`:          false,
		`{"a":[5],"b":{"c":[[[[9007199254740993]]]]}}`:       false,
		`{"a":[5],"b":{"c":[[[[900719925474099]]]]}}`:        true,
		`{"a":[5],"b":{"c":[[["x",[-0.000000000000001]]]]}}`: false,
	} {
		if !json.Valid([]byte(raw)) {
			t.Fatalf("%s is not JSON", raw)
		}
		exact, _ := scanArguments([]byte(raw))
		assert.Equal(t, want, exact, raw)
	}
}

// The scan allocates nothing.
func TestScanArgumentsAllocations(t *testing.T) {
	raw := []byte(`{"a":[1,2.5,{"b":"c\"d"}],"n":123456789012345}`)
	assert.Zero(t, testing.AllocsPerRun(100, func() { scanArguments(raw) }))
}

// numbersSchema puts kw on the number n, on the items of the array a, and on
// the numbers two arrays deep in o.x.
func numbersSchema(typ, kw string) string {
	return fmt.Sprintf(`{"type":"object","properties":{`+
		`"n":{"type":%[1]q,%[2]s},`+
		`"a":{"type":"array","uniqueItems":true,"items":{%[2]s}},`+
		`"o":{"type":"object","properties":{"x":{"type":"array","items":{"type":"array","items":{%[2]s}}}}}}}`, typ, kw)
}

// numbersArgs places the literal n: at the top, in an array beside 5, twice
// in an array, two arrays deep, and in all three places at once.
func numbersArgs(n string) []string {
	return []string{
		`{"n":` + n + `}`,
		`{"a":[` + n + `,5]}`,
		`{"a":[` + n + `,` + n + `]}`,
		`{"o":{"x":[[1],[` + n + `]]}}`,
		`{"n":` + n + `,"a":[` + n + `],"o":{"x":[[` + n + `]]}}`,
	}
}

// validationOutcome renders what validation answered: whether it ran, and
// its failures as a set, since their order varies from call to call today.
func validationOutcome(ran bool, err error) string {
	if err == nil {
		return fmt.Sprintf("ran=%v ok", ran)
	}
	parts := strings.Split(strings.TrimPrefix(err.Error(), "input schema validation failed: "), "; ")
	slices.Sort(parts)
	return fmt.Sprintf("ran=%v %s", ran, strings.Join(parts, "; "))
}

// validated validates args against compiled and renders the outcome; a
// panic fails t. jsonschema v6.0.3 panics on a number big.Rat cannot read,
// such as 0e10000000000000000000, and validation must answer it as a
// failure instead.
func validated(t testing.TB, compiled *jsonschema.Schema, args callArguments) string {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("validation panicked: %v", r)
		}
	}()
	return validationOutcome(validateArguments(&cachedToolSchema{compiled: compiled}, args))
}

// compileFuzzed compiles schema, nil when it does not compile or when the
// compiler panics, as jsonschema v6.0.3's does on a bound big.Rat cannot
// read: no call reaches validation then.
func compileFuzzed(schema string) (compiled *jsonschema.Schema) {
	defer func() {
		if recover() != nil {
			compiled = nil
		}
	}()
	var err error
	if compiled, err = compileSchema("n", []byte(schema)); err != nil {
		return nil
	}
	return compiled
}

// numbersTally counts what checkNumbers saw.
type numbersTally struct {
	// decoded and sent count the calls validated from their Arguments and
	// from a second decode of their bytes.
	decoded, sent int
	// unguarded counts the calls validated from a second decode whose
	// Arguments would have been read differently: the cases the scan is for.
	unguarded int
}

// validatedAsBefore validates raw against compiled from the tree validation
// read before it read either the decoded Arguments or raw in one pass:
// jsonschema.UnmarshalJSON's.
func validatedAsBefore(t testing.TB, compiled *jsonschema.Schema, raw []byte) string {
	t.Helper()
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	require.NoError(t, err, "%s", raw)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("validation panicked: %v", r)
		}
	}()
	if err := validateTree(compiled, value); err != nil {
		return validationOutcome(true, formatValidationError(err))
	}
	return validationOutcome(true, nil)
}

// checkNumbers validates args against compiled as a call does, from its
// Arguments or from its bytes read in one pass, and as before, from
// jsonschema.UnmarshalJSON's tree, and fails t when the answers differ or
// validation changed Arguments.
func checkNumbers(t testing.TB, compiled *jsonschema.Schema, args string, tally *numbersTally) {
	t.Helper()
	var p mcp.CallToolParams
	if err := json.Unmarshal([]byte(`{"name":"n","arguments":`+args+`}`), &p); err != nil {
		return
	}
	var before any
	require.NoError(t, json.Unmarshal(p.RawArguments, &before))
	now := validated(t, compiled, callArguments{raw: p.RawArguments, decoded: p.Arguments})
	sent := validated(t, compiled, callArguments{raw: p.RawArguments})
	asBefore := validatedAsBefore(t, compiled, p.RawArguments)
	if now != asBefore || sent != asBefore {
		t.Fatalf("arguments %s:\n validated now  %s\n as sent now    %s\n as sent before %s", args, now, sent, asBefore)
	}
	if !reflect.DeepEqual(p.Arguments, before) {
		t.Fatalf("arguments %s: validation changed them to %#v", args, p.Arguments)
	}
	if tally == nil {
		return
	}
	if exact, _ := scanArguments(p.RawArguments); !exact {
		tally.sent++
		err := compiled.Validate(p.Arguments)
		if err != nil {
			err = formatValidationError(err)
		}
		if validationOutcome(true, err) != sent {
			tally.unguarded++
		}
		return
	}
	tally.decoded++
}

func compileNumbersSchema(t testing.TB, schema string) *jsonschema.Schema {
	t.Helper()
	compiled, err := compileSchema("n", []byte(schema))
	require.NoError(t, err, schema)
	return compiled
}

// numbersKeywords are the keywords that read numbers: the bounds at 2^53,
// multipleOf by decimals, enums and consts of numbers, and the bounds next
// to the smallest float64s and to zero.
var numbersKeywords = []string{
	`"maximum":9007199254740992`,
	`"minimum":9007199254740993`,
	`"exclusiveMaximum":9007199254740993`,
	`"exclusiveMinimum":9007199254740992`,
	`"maximum":123456789012345`,
	`"minimum":123456789012345.5`,
	`"maximum":99999999999999.9`,
	`"minimum":0.00000000000001`,
	`"multipleOf":0.1`,
	`"multipleOf":0.01`,
	`"multipleOf":0.3`,
	`"multipleOf":7.5`,
	`"multipleOf":1e-3`,
	`"enum":[0.1,0.3,9007199254740993,-0,"s"]`,
	`"enum":[9007199254740992,1.5]`,
	`"const":0.30000000000000004`,
	`"const":0.3`,
	`"const":9007199254740993`,
	`"maximum":4e-324`,
	`"exclusiveMinimum":0`,
	`"maximum":0`,
	`"minimum":-0`,
}

// numbersLiterals are 15-, 16- and 17-digit numbers, 2^53 and 2^53+1,
// exponents, minus zero, leading and trailing zeros, decimals like 0.1 and
// 0.3, and literals a float64 cannot hold exactly or at all.
var numbersLiterals = []string{
	`5`, `0`, `-0`, `-0.0`, `0.1`, `0.3`, `0.30000000000000004`, `7.5`, `22.5`, `1.50`,
	`123456789012345`, `123456789012345.5`, `999999999999999`, `-999999999999999`,
	`1234567890123456`, `9007199254740992`, `9007199254740993`, `-9007199254740993`,
	`12345678901234567`, `0.00000000000001`, `0.000000000000001`, `99999999999999.9`,
	`0.100000000000000`, `1e3`, `1E3`, `1e-7`, `2.5E+2`, `9.007199254740993e15`,
	`3e-324`, `4.9e-324`, `1e-400`, `1e308`, `0.1e1`, `-0e0`,
	`0e10000000000000000000`, `1e-1000001`, `0e1000000`, `-0.0e-9223372036854775808`,
}

// The differential: every keyword, both types, every literal in every place,
// and arguments whose Arguments came from encoding/json rather than the
// one-pass reader (invalid UTF-8, past 64 KiB).
func TestInputValidationOfTheDecodedArgumentsMatchesTheSecondDecode(t *testing.T) {
	var tally numbersTally
	pad := strings.Repeat("x", 70_000)
	for _, kw := range numbersKeywords {
		for _, typ := range []string{"number", "integer"} {
			compiled := compileNumbersSchema(t, numbersSchema(typ, kw))
			for _, n := range numbersLiterals {
				for _, args := range numbersArgs(n) {
					checkNumbers(t, compiled, args, &tally)
				}
				checkNumbers(t, compiled, `{"s":"`+"\xff"+`","n":`+n+`}`, &tally)
				checkNumbers(t, compiled, `{"s":"\ud800","a":[`+n+`]}`, &tally)
				checkNumbers(t, compiled, `{"s":"`+pad+`","o":{"x":[[`+n+`]]}}`, &tally)
			}
		}
	}
	t.Logf("validated from Arguments %d, from a second decode %d, of which Arguments would differ %d",
		tally.decoded, tally.sent, tally.unguarded)
	assert.Positive(t, tally.decoded, "no call was validated from its Arguments")
	assert.Positive(t, tally.unguarded, "no input shows the scan is needed")
}

// randomLiteral is a JSON number of 1 to 18 digits: a sign at times, a point
// at times, a leading zero at times, an exponent at times.
func randomLiteral(r *rand.Rand) string {
	var b strings.Builder
	if r.IntN(3) == 0 {
		b.WriteByte('-')
	}
	digits := 1 + r.IntN(18)
	if r.IntN(4) == 0 {
		b.WriteString("0.")
		for range digits - 1 {
			b.WriteByte(byte('0' + r.IntN(10)))
		}
		b.WriteByte(byte('1' + r.IntN(9)))
	} else {
		point := -1
		if digits > 1 && r.IntN(2) == 0 {
			point = 1 + r.IntN(digits-1)
		}
		for i := range digits {
			if i == point {
				b.WriteByte('.')
			}
			d := byte('0' + r.IntN(10))
			if i == 0 && digits > 1 && point != 1 && d == '0' {
				d = byte('1' + r.IntN(9))
			}
			b.WriteByte(d)
		}
	}
	if r.IntN(8) == 0 {
		fmt.Fprintf(&b, "e%d", r.IntN(60)-30)
	}
	return b.String()
}

// The differential over generated literals: random bounds under every
// keyword, the literal in one of numbersArgs' places at random.
func TestInputValidationOfTheDecodedArgumentsMatchesOnGeneratedNumbers(t *testing.T) {
	cases := 20_000
	if raceEnabled || testing.Short() {
		cases = 2_000
	}
	r := rand.New(rand.NewPCG(53, 1))
	keywords := []string{"maximum", "minimum", "exclusiveMaximum", "exclusiveMinimum", "multipleOf", "const", "enum"}
	var tally numbersTally
	for range cases {
		n, bound := randomLiteral(r), randomLiteral(r)
		if r.IntN(3) == 0 {
			bound = n
		}
		kw := keywords[r.IntN(len(keywords))]
		switch kw {
		case "multipleOf":
			if strings.HasPrefix(bound, "-") || strings.Trim(bound, "0.-") == "" || strings.ContainsAny(bound, "eE") {
				bound = []string{"0.1", "0.01", "3", "0.3", "7.5", "1e-3"}[r.IntN(6)]
			}
		case "enum":
			bound = "[" + bound + `,"s",2]`
		}
		typ := "number"
		if r.IntN(3) == 0 {
			typ = "integer"
		}
		compiled, err := compileSchema("n", []byte(numbersSchema(typ, fmt.Sprintf("%q:%s", kw, bound))))
		if err != nil {
			continue
		}
		places := numbersArgs(n)
		checkNumbers(t, compiled, places[r.IntN(len(places))], &tally)
	}
	t.Logf("validated from Arguments %d, from a second decode %d, of which Arguments would differ %d",
		tally.decoded, tally.sent, tally.unguarded)
	assert.Positive(t, tally.decoded, "no call was validated from its Arguments")
	assert.Positive(t, tally.unguarded, "no input shows the scan is needed")
}

func FuzzInputValidationOfTheDecodedArguments(f *testing.F) {
	f.Add("1.5", "0.5", uint8(0), uint8(0))
	f.Add("9007199254740993", "9007199254740992", uint8(0), uint8(3))
	f.Add("0.3", "0.1", uint8(4), uint8(1))
	f.Add("3e-324", "4e-324", uint8(0), uint8(4))
	f.Add("-0", "0", uint8(5), uint8(2))
	f.Add("0e10000000000000000000", "1", uint8(0), uint8(0))
	f.Add("1e-1000001", "1", uint8(1), uint8(3))
	f.Add("0", "0e10000000000000000000", uint8('t'), uint8(1))
	keywords := []string{"maximum", "minimum", "exclusiveMaximum", "exclusiveMinimum", "multipleOf", "const", "enum"}
	f.Fuzz(func(t *testing.T, n, bound string, kw, at uint8) {
		if !json.Valid([]byte(n)) || !json.Valid([]byte(bound)) {
			return
		}
		key := keywords[int(kw)%len(keywords)]
		if key == "enum" {
			bound = "[" + bound + "]"
		}
		typ := "number"
		if kw >= 128 {
			typ = "integer"
		}
		compiled := compileFuzzed(numbersSchema(typ, fmt.Sprintf("%q:%s", key, bound)))
		if compiled == nil {
			return
		}
		places := numbersArgs(n)
		checkNumbers(t, compiled, places[int(at)%len(places)], nil)
	})
}

// 2^53+1 two arrays deep is validated as sent, as it is at the top.
func TestInputValidationReadsNestedNumbersAsSent(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation())
	srv.AddTool(mcp.NewToolWithRawSchema("nested", "nested limits", json.RawMessage(
		numbersSchema("integer", `"maximum":9007199254740992`))), okHandler)
	call := func(arguments string) string {
		out, err := json.Marshal(srv.HandleMessage(t.Context(), json.RawMessage(
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"nested","arguments":`+arguments+`}}`)))
		require.NoError(t, err)
		return string(out)
	}
	assert.Contains(t, call(`{"o":{"x":[[1],[9007199254740993]]}}`), `"isError":true`)
	assert.Contains(t, call(`{"o":{"x":[[1],[9007199254740992]]}}`), `"text":"ok"`)
	assert.Contains(t, call(`{"a":[9007199254740993]}`), `"isError":true`)
}

// A handler sees the Arguments validation read, as they were decoded.
func TestInputValidationLeavesTheArgumentsAsDecoded(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation())
	var seen any
	srv.AddTool(mcp.NewToolWithRawSchema("kept", "keeps its arguments", json.RawMessage(
		numbersSchema("number", `"minimum":0`))), func(_ context.Context, r mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		seen = r.Params.Arguments
		return mcp.NewToolResultText("ok"), nil
	})
	arguments := `{"n":0.1,"a":[1.5,2],"o":{"x":[[3],[4.25]]}}`
	var want any
	require.NoError(t, json.Unmarshal([]byte(arguments), &want))
	out, err := json.Marshal(srv.HandleMessage(t.Context(), json.RawMessage(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"kept","arguments":`+arguments+`}}`)))
	require.NoError(t, err)
	require.Contains(t, string(out), `"text":"ok"`)
	assert.Equal(t, want, seen)
}

// complexValidationCall is a validated call of bench/compare's complex shape:
// twenty items of an id, a name and three tags, and forty string properties,
// against a schema of a hundred and thirty.
func complexValidationCall(t testing.TB, opts ...ServerOption) (*MCPServer, json.RawMessage) {
	t.Helper()
	var schema strings.Builder
	schema.WriteString(`{"type":"object","required":["region","items"],"properties":{"region":{"type":"string"},` +
		`"items":{"type":"array","items":{"type":"object","required":["id","name"],"properties":{` +
		`"id":{"type":"integer"},"name":{"type":"string"},"tags":{"type":"array","items":{"type":"string"}}}}}`)
	for i := range 130 {
		fmt.Fprintf(&schema, `,"p%03d":{"type":"string","description":"property number %03d"}`, i, i)
	}
	schema.WriteString(`}}`)
	var args strings.Builder
	args.WriteString(`{"items":[`)
	for i := range 20 {
		if i > 0 {
			args.WriteByte(',')
		}
		fmt.Fprintf(&args, `{"id":%d,"name":"item-%02d","tags":["alpha","beta","gamma"]}`, i, i)
	}
	args.WriteByte(']')
	for i := range 40 {
		fmt.Fprintf(&args, `,"p%03d":"value %03d"`, i, i)
	}
	args.WriteString(`,"region":"us-east-1"}`)
	srv := NewMCPServer("test", "1.0.0", append([]ServerOption{WithToolCapabilities(false), WithInputSchemaValidation()}, opts...)...)
	srv.AddTool(mcp.NewToolWithRawSchema("complex", "a call of many arguments", json.RawMessage(schema.String())), okHandler)
	return srv, json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"complex","arguments":` + args.String() + `}}`)
}

// A validated call whose numbers read the same as float64s is validated from
// the Arguments it decoded: 1562 allocations when the validator decoded the
// arguments a second time, 969 from the Arguments.
func TestInputValidationComplexCallAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	srv, msg := complexValidationCall(t)
	ctx := t.Context()
	out, err := json.Marshal(srv.HandleMessage(ctx, msg))
	require.NoError(t, err)
	require.Contains(t, string(out), `"text":"ok"`)
	refused, err := json.Marshal(srv.HandleMessage(ctx, json.RawMessage(strings.Replace(string(msg), `"region":"us-east-1"`, `"region":5`, 1))))
	require.NoError(t, err)
	require.Contains(t, string(refused), `"isError":true`)

	allocs := testing.AllocsPerRun(200, func() { srv.HandleMessage(ctx, msg) })
	assert.LessOrEqual(t, allocs, 969.0, "allocations per validated complex tools/call")
}

// A validated call the validator cannot read from its Arguments has its
// arguments read for the validator in one pass: a call whose numbers do not
// read exactly as float64s (an id written with an exponent), and a call a
// before-call hook could have changed: 1565 and 1650 allocations when
// jsonschema.UnmarshalJSON read them, 1138 and 1224 in one pass, the hook's
// call paying the round-trip mark's one allocation.
func TestInputValidationSecondReadAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	hooks := &Hooks{}
	hooks.AddBeforeCallTool(func(context.Context, any, *mcp.CallToolRequest) {})
	for _, tc := range []struct {
		name    string
		opts    []ServerOption
		ceiling float64
	}{
		{"an exponent", nil, 1138},
		{"a before-call hook", []ServerOption{WithHooks(hooks)}, 1224},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, msg := complexValidationCall(t, tc.opts...)
			if tc.opts == nil {
				msg = json.RawMessage(strings.Replace(string(msg), `"id":0,`, `"id":0e0,`, 1))
			}
			ctx := t.Context()
			out, err := json.Marshal(srv.HandleMessage(ctx, msg))
			require.NoError(t, err)
			require.Contains(t, string(out), `"text":"ok"`)
			allocs := testing.AllocsPerRun(200, func() { srv.HandleMessage(ctx, msg) })
			assert.LessOrEqual(t, allocs, tc.ceiling, "allocations per validated complex tools/call with %s", tc.name)
		})
	}
}
