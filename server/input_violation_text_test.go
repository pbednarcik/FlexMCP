package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/message"
)

// escapeU is the start of a JSON unicode escape, written in two pieces so
// this file holds no unicode escape of its own.
const escapeU = "\\" + "u"

func TestCappedWriter(t *testing.T) {
	t.Run("text that fits is kept whole", func(t *testing.T) {
		w := newCappedWriter(5)
		assert.True(t, w.WriteString("hel"))
		assert.True(t, w.WriteString("lo"))
		assert.Equal(t, "hello", w.String())
		assert.False(t, w.Cut(), "exactly the limit is not a cut")
	})
	t.Run("text past the limit is cut and the writer stops", func(t *testing.T) {
		w := newCappedWriter(5)
		assert.True(t, w.WriteString("abc"))
		assert.False(t, w.WriteString("defg"))
		assert.Equal(t, "abcde", w.String())
		assert.True(t, w.Cut())
		assert.False(t, w.WriteString("x"), "a cut writer takes nothing more")
		assert.False(t, w.WriteString(""), "not even nothing")
		assert.Equal(t, "abcde", w.String())
	})
	t.Run("the cut is on a character", func(t *testing.T) {
		w := newCappedWriter(4)
		assert.False(t, w.WriteString("ab中"))
		assert.Equal(t, "ab", w.String(), "the three bytes of 中 do not fit in the two left")
		assert.True(t, w.Cut())
	})
	t.Run("a character that fits exactly is kept", func(t *testing.T) {
		w := newCappedWriter(6)
		assert.True(t, w.WriteString("abc"))
		assert.True(t, w.WriteString("中"))
		assert.False(t, w.Cut())
		assert.False(t, w.WriteString("x"))
		assert.Equal(t, "abc中", w.String())
	})
	t.Run("no limit takes nothing", func(t *testing.T) {
		w := newCappedWriter(0)
		assert.False(t, w.WriteString("x"))
		assert.Equal(t, "", w.String())
		assert.True(t, w.Cut())
	})
}

func ratOf(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	require.True(t, ok, s)
	return r
}

func TestBoundText(t *testing.T) {
	pow2 := func(n uint) *big.Int { return new(big.Int).Lsh(big.NewInt(1), n) }
	pow5 := func(n int64) *big.Int { return new(big.Int).Exp(big.NewInt(5), big.NewInt(n), nil) }
	// padded is the digits of n written to width, a fraction's tail.
	padded := func(n *big.Int, width int) string {
		s := n.String()
		return strings.Repeat("0", width-len(s)) + s
	}
	float := func(f float64) string { return strconv.FormatFloat(f, 'e', -1, 64) }
	tests := []struct {
		name  string
		rat   *big.Rat
		text  string
		exact bool
	}{
		{"a tenth", ratOf(t, "0.1"), "0.1", true},
		{"zero", new(big.Rat), "0", true},
		{"an integer", ratOf(t, "5"), "5", true},
		{"a negative integer", ratOf(t, "-300"), "-300", true},
		{"an eighth", ratOf(t, "0.125"), "0.125", true},
		{"a negative fraction", ratOf(t, "-0.5"), "-0.5", true},
		{"a fraction with a long tail", ratOf(t, "0.0001220703125"), "0.0001220703125", true},
		{"two to the 200", new(big.Rat).SetInt(pow2(200)), pow2(200).String(), true},
		{"one over five to the 110th, the last power of five that fits", new(big.Rat).SetFrac(big.NewInt(1), pow5(110)), "0." + padded(pow2(110), 110), true},
		{"one over two to the 254, exactly 256 bytes", new(big.Rat).SetFrac(big.NewInt(1), pow2(254)), "0." + padded(pow5(254), 254), true},
		{"a third", big.NewRat(1, 3), "0.3333333333333333", false},
		{"a seventh", big.NewRat(1, 7), "0.14285714285714285", false},
		{"one over two to the 255, 257 bytes", new(big.Rat).SetFrac(big.NewInt(1), pow2(255)), float(math.Ldexp(1, -255)), false},
		{"2^256-1 over 2^255, 257 bytes", new(big.Rat).SetFrac(new(big.Int).Sub(pow2(256), big.NewInt(1)), pow2(255)), "2", false},
		{"a numerator of 257 bits", new(big.Rat).SetInt(pow2(256)), float(math.Ldexp(1, 256)), false},
		{"a denominator of 257 bits", new(big.Rat).SetFrac(big.NewInt(1), pow2(256)), float(math.Ldexp(1, -256)), false},
		{"one over five to the 111th, 258 bits", new(big.Rat).SetFrac(big.NewInt(1), pow5(111)), "", false},
		{"a ratio that is not a decimal", new(big.Rat).SetFrac(big.NewInt(1), big.NewInt(3*1024)), "", false},
		{"the smallest float64, read exactly", new(big.Rat).SetFloat64(5e-324), "5e-324", false},
		{"a number past float64", ratOf(t, "1"+strings.Repeat("0", 400)), "+Inf", false},
		{"a number below float64", ratOf(t, "1e-1100"), "0", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			text, exact := boundText(tc.rat)
			assert.Equal(t, tc.exact, exact)
			assert.LessOrEqual(t, len(text), 256)
			if tc.text != "" {
				assert.Equal(t, tc.text, text)
			}
			if tc.exact {
				back, ok := new(big.Rat).SetString(text)
				require.True(t, ok)
				assert.Zero(t, back.Cmp(tc.rat), "the exact text names the same number")
			}
		})
	}
	t.Run("the bound a schema holds never reads past 256 bytes", func(t *testing.T) {
		for _, s := range []string{"1e-1100", "-1e-1100", "1.5e-1100", "0." + strings.Repeat("7", 1099), "1" + strings.Repeat("0", 308)} {
			text, _ := boundText(ratOf(t, s))
			assert.LessOrEqual(t, len(text), 256, s)
		}
	})
	t.Run("the rat it reads is left as it was", func(t *testing.T) {
		r := ratOf(t, "0.1")
		boundText(r)
		assert.Equal(t, "1/10", r.String())
	})
}

func TestPreview(t *testing.T) {
	roomy := func() *previewBudget { return &previewBudget{nodes: 1 << 30, bytes: 1 << 30} }
	t.Run("an array of a million zeros is cut at the limit", func(t *testing.T) {
		zeros := make([]any, 1<<20)
		for i := range zeros {
			zeros[i] = float64(0)
		}
		budget := roomy()
		text, cut := preview(zeros, 256, budget)
		assert.True(t, cut)
		assert.Len(t, text, 256)
		assert.True(t, strings.HasPrefix(text, "[0,0,0,"), text)
		assert.Equal(t, 1<<30-256, budget.bytes, "the bytes written come out of the budget")
		assert.LessOrEqual(t, 1<<30-budget.nodes, 131, "the nodes spent stop with the writing")
	})
	t.Run("an object of ten thousand keys writes its least keys", func(t *testing.T) {
		object := make(map[string]any, 10000)
		for i := range 10000 {
			object[fmt.Sprintf("k%05d", i)] = float64(0)
		}
		budget := &previewBudget{nodes: 32, bytes: 1 << 20}
		text, cut := preview(object, 1<<20, budget)
		var want strings.Builder
		want.WriteString("{")
		for i := range 31 {
			if i > 0 {
				want.WriteString(",")
			}
			fmt.Fprintf(&want, `"k%05d":0`, i)
		}
		assert.Equal(t, want.String(), text, "the object, then 31 members, with no closing brace")
		assert.True(t, cut)
		assert.Zero(t, budget.nodes)
		allocs := testing.AllocsPerRun(10, func() {
			b := &previewBudget{nodes: 32, bytes: 1 << 20}
			preview(object, 1<<20, b)
		})
		assert.LessOrEqual(t, allocs, 32.0, "no more allocations than nodes")
	})
	t.Run("an object that fits is whole and in key order", func(t *testing.T) {
		value := map[string]any{"b": float64(1), "a": []any{true, nil, "x\ny"}, "c": map[string]any{}, "": []any{}}
		text, cut := preview(value, 256, roomy())
		assert.Equal(t, `{"":[],"a":[true,null,"x\ny"],"b":1,"c":{}}`, text)
		assert.False(t, cut)
	})
	t.Run("each value is a node", func(t *testing.T) {
		budget := &previewBudget{nodes: 3, bytes: 100}
		text, cut := preview([]any{float64(1), float64(2), float64(3), float64(4)}, 100, budget)
		assert.Equal(t, "[1,2", text, "the array and two members")
		assert.True(t, cut)
		assert.Zero(t, budget.nodes)

		budget = &previewBudget{nodes: 4, bytes: 100}
		text, cut = preview(map[string]any{"a": float64(1), "b": float64(2), "c": float64(3)}, 100, budget)
		assert.Equal(t, `{"a":1,"b":2,"c":3}`, text, "a key is not a value, and the object and its three members are four")
		assert.False(t, cut)
		assert.Zero(t, budget.nodes)

		text, cut = preview(float64(1), 100, &previewBudget{nodes: 0, bytes: 100})
		assert.Equal(t, "", text)
		assert.True(t, cut)
	})
	t.Run("the bytes budget and the limit, whichever is less", func(t *testing.T) {
		budget := &previewBudget{nodes: 100, bytes: 5}
		text, cut := preview([]any{"abcdef"}, 256, budget)
		assert.Equal(t, `["abc`, text)
		assert.True(t, cut)
		assert.Zero(t, budget.bytes)

		budget = &previewBudget{nodes: 100, bytes: 100}
		text, cut = preview([]any{"abcdef"}, 4, budget)
		assert.Equal(t, `["ab`, text)
		assert.True(t, cut)
		assert.Equal(t, 96, budget.bytes)

		text, cut = preview("x", 256, &previewBudget{nodes: 1, bytes: 0})
		assert.Equal(t, "", text)
		assert.True(t, cut)
	})
	t.Run("a number keeps the digits it came with", func(t *testing.T) {
		tests := []struct {
			value any
			want  string
		}{
			{json.Number("1.50"), "1.50"},
			{json.Number("1e400"), "1e400"},
			{float64(0.1), "0.1"},
			{float64(1000000), "1000000"},
			{float64(1e21), "1e+21"},
			{float64(1e-7), "1e-7"},
			{float64(-2.5), "-2.5"},
			{true, "true"},
			{false, "false"},
			{nil, "null"},
		}
		for _, tc := range tests {
			text, cut := preview(tc.value, 256, roomy())
			assert.Equal(t, tc.want, text, "%T %v", tc.value, tc.value)
			assert.False(t, cut)
		}
	})
	t.Run("a string is escaped as JSON", func(t *testing.T) {
		text, cut := preview("a\"b\\c\n\t\r\x01é中", 256, roomy())
		assert.Equal(t, `"a\"b\\c\n\t\r`+escapeU+`0001é中"`, text)
		assert.False(t, cut)
		text, _ = preview("a\xffb", 256, roomy())
		assert.Equal(t, "\"a\xef\xbf\xbdb\"", text, "a byte that is no character reads as the replacement character")
	})
	t.Run("a long string is cut at the limit, on a character", func(t *testing.T) {
		text, cut := preview(strings.Repeat("a", 1<<20), 256, roomy())
		assert.Equal(t, `"`+strings.Repeat("a", 255), text)
		assert.True(t, cut)
		text, cut = preview(strings.Repeat("中", 1<<16), 256, roomy())
		assert.Equal(t, `"`+strings.Repeat("中", 85), text, "85 characters of three bytes, and the quote, is 256")
		assert.True(t, cut)
		text, cut = preview(strings.Repeat("\n", 1<<20), 256, roomy())
		assert.Len(t, text, 256)
		assert.True(t, cut)
	})
	t.Run("a string that fits but for its closing quote is cut", func(t *testing.T) {
		text, cut := preview(strings.Repeat("a", 255), 256, roomy())
		assert.Equal(t, `"`+strings.Repeat("a", 255), text)
		assert.True(t, cut)
	})
	t.Run("a type no decode makes is not written, and cuts the preview", func(t *testing.T) {
		text, cut := preview(struct{ X int }{1}, 256, roomy())
		assert.Equal(t, "", text)
		assert.True(t, cut)
		text, cut = preview([]any{float64(1), 7, float64(2)}, 256, roomy())
		assert.Equal(t, "[1,", text, "an int is none of the decoded types")
		assert.True(t, cut)
	})
	t.Run("no budget is no node or byte budget", func(t *testing.T) {
		text, cut := preview([]any{float64(1), "a"}, 256, nil)
		assert.Equal(t, `[1,"a"]`, text)
		assert.False(t, cut)
	})
	t.Run("a huge key allocates a limit, not a key", func(t *testing.T) {
		object := map[string]any{strings.Repeat("~", 1<<20): float64(1)}
		measure := func() uint64 {
			before := allocatedBytes()
			preview(object, 256, roomy())
			return allocatedBytes() - before
		}
		assert.Less(t, min(measure(), measure(), measure()), uint64(16<<10))
		text, cut := preview(object, 256, roomy())
		assert.Len(t, text, 256)
		assert.True(t, cut)
	})
}

// allocatedBytes is the bytes the process has allocated so far.
func allocatedBytes() uint64 {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.TotalAlloc
}

// lineTree validates instance against schema, formats and content asserted,
// and returns the failure.
func lineTree(t *testing.T, schema, instance string) *jsonschema.ValidationError {
	t.Helper()
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(schema))
	require.NoError(t, err)
	value, err := jsonschema.UnmarshalJSON(strings.NewReader(instance))
	require.NoError(t, err)
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	c.AssertContent()
	require.NoError(t, c.AddResource("mem:///t/schema.json", doc))
	compiled, err := c.Compile("mem:///t/schema.json")
	require.NoError(t, err)
	verr, ok := errors.AsType[*jsonschema.ValidationError](compiled.Validate(value))
	require.True(t, ok, "%s against %s must fail", instance, schema)
	return verr
}

func eachNode(verr *jsonschema.ValidationError, f func(*jsonschema.ValidationError)) {
	f(verr)
	for _, cause := range verr.Causes {
		eachNode(cause, f)
	}
}

// kindName is the name of the kind's type in kind/kind.go.
func kindName(k jsonschema.ErrorKind) string {
	return reflect.TypeOf(k).Elem().Name()
}

// everyKind names each error kind kind/kind.go declares.
var everyKind = []string{
	"InvalidJsonValue", "Schema", "Group", "Not", "AllOf", "AnyOf", "OneOf", "FalseSchema", "RefCycle", "Type",
	"Enum", "Const", "Format", "Reference", "MinProperties", "MaxProperties", "MinItems", "MaxItems",
	"AdditionalItems", "Required", "Dependency", "DependentRequired", "AdditionalProperties", "PropertyNames",
	"UniqueItems", "Contains", "MinContains", "MaxContains", "MinLength", "MaxLength", "Pattern",
	"ContentEncoding", "ContentMediaType", "ContentSchema", "Minimum", "Maximum", "ExclusiveMinimum",
	"ExclusiveMaximum", "MultipleOf",
}

const (
	draft7  = `"$schema":"http://json-schema.org/draft-07/schema#"`
	draft4  = `"$schema":"http://json-schema.org/draft-04/schema#"`
	draft19 = `"$schema":"https://json-schema.org/draft/2019-09/schema"`
)

// smallFailures are small real validations, each with a failure of one or
// more kinds of kind/kind.go.
func smallFailures() []struct{ name, schema, instance string } {
	return []struct{ name, schema, instance string }{
		{"type", `{"type":"string"}`, `5`},
		{"types", `{"type":["string","null"]}`, `5`},
		{"enum", `{"enum":["a","b",1,null,true,2.5]}`, `"z"`},
		{"enum of one", `{"enum":["it's \"quoted\""]}`, `"z"`},
		{"enum of containers", `{"enum":[[1],{"a":1}]}`, `"z"`},
		{"enum of a mix", `{"enum":["a",{"a":1}]}`, `"z"`},
		{"const", `{"const":"a'b"}`, `"z"`},
		{"const null", `{"const":null}`, `1`},
		{"const object", `{"const":{"a":1}}`, `1`},
		{"format", `{"format":"email"}`, `"not an email"`},
		{"format regex", `{"format":"regex"}`, `"("`},
		{"format uri", `{"format":"uri"}`, `"not a uri"`},
		{"format duration", `{"format":"duration"}`, `"x"`},
		{"minProperties", `{"minProperties":2}`, `{}`},
		{"maxProperties", `{"maxProperties":1}`, `{"a":1,"b":2}`},
		{"minItems", `{"minItems":2}`, `[1]`},
		{"maxItems", `{"maxItems":1}`, `[1,2]`},
		{"additionalItems", `{` + draft7 + `,"items":[{}],"additionalItems":false}`, `[1,2]`},
		{"required one", `{"required":["a"]}`, `{}`},
		{"required three", `{"required":["a","b'c","d\"e"]}`, `{}`},
		{"dependencies", `{` + draft7 + `,"dependencies":{"a":["b","c"]}}`, `{"a":1}`},
		{"dependentRequired", `{"dependentRequired":{"a":["b","c"]}}`, `{"a":1}`},
		{"additionalProperties", `{"additionalProperties":false}`, `{"x":1}`},
		{"propertyNames", `{"propertyNames":{"minLength":3}}`, `{"ab":1}`},
		{"uniqueItems", `{"uniqueItems":true}`, `[1,2,1]`},
		{"contains", `{"contains":{"type":"string"}}`, `[1]`},
		{"minContains", `{"contains":{"type":"string"},"minContains":2}`, `["a",1]`},
		{"minContains none", `{"contains":{"type":"string"},"minContains":2}`, `[1]`},
		{"minContains 2019", `{` + draft19 + `,"contains":{"type":"string"},"minContains":2}`, `["a",1]`},
		{"maxContains", `{"contains":{"type":"string"},"maxContains":1}`, `["a","b",1,"c"]`},
		{"minLength", `{"minLength":3}`, `"ab"`},
		{"maxLength", `{"maxLength":1}`, `"ab"`},
		{"pattern", `{"pattern":"^a"}`, `"b'c\"d"`},
		{"contentEncoding", `{"contentEncoding":"base64"}`, `"!!!"`},
		{"contentMediaType", `{"contentMediaType":"application/json"}`, `"{"`},
		{"contentSchema", `{"contentMediaType":"application/json","contentSchema":{"type":"string"}}`, `"5"`},
		{"minimum", `{"minimum":10}`, `5`},
		{"maximum", `{"maximum":10}`, `5000`},
		{"maximum draft 4", `{` + draft4 + `,"maximum":10,"exclusiveMaximum":true}`, `10`},
		{"exclusiveMinimum", `{"exclusiveMinimum":10}`, `10`},
		{"exclusiveMaximum", `{"exclusiveMaximum":10}`, `10`},
		{"multipleOf", `{"multipleOf":3}`, `10`},
		{"not", `{"not":{"type":"string"}}`, `"a"`},
		{"allOf", `{"allOf":[{"type":"string"},{"minLength":3}]}`, `5`},
		{"anyOf", `{"anyOf":[{"type":"string"},{"type":"null"}]}`, `5`},
		{"oneOf none", `{"oneOf":[{"type":"string"},{"type":"null"}]}`, `5`},
		{"oneOf two", `{"oneOf":[{"type":"integer"},{"minimum":1}]}`, `5`},
		{"false schema", `{"properties":{"x":false}}`, `{"x":1}`},
		{"ref", `{"$ref":"#/$defs/a","$defs":{"a":{"type":"string"}}}`, `1`},
		{"nested", `{"properties":{"a":{"items":{"anyOf":[{"enum":["x"]},{"type":"object","required":["q"]}]}}}}`, `{"a":[1]}`},
		{"group", `{"anyOf":[{"minLength":5,"pattern":"^z"},{"type":"null"}]}`, `"ab"`},
	}
}

// lineLocations are instance locations of every size the line's layout cares
// about, built once.
var lineLocations = sync.OnceValue(func() map[string][]string {
	return map[string][]string{
		"root":        nil,
		"short":       {"a"},
		"escaped":     {"a/b", "~", "c"},
		"empty":       {""},
		"long key":    {strings.Repeat("k", 300)},
		"1 MiB tilde": {strings.Repeat("~", 1<<20)},
		"64 KiB 中":    {strings.Repeat("中", 64<<10)},
		"deep":        {"a", "b", "c", "d", "e", "f", strings.Repeat("g", 100)},
	}
})

func TestCappedValidationLineMatchesValidationLine(t *testing.T) {
	seen := map[string]bool{}
	check := func(t *testing.T, at []string, k jsonschema.ErrorKind) {
		t.Helper()
		seen[kindName(k)] = true
		want := validationLine(at, k.LocalizedString(validationMessages))
		got := cappedValidationLine(at, func(w *cappedWriter) { kindText(k, w) })
		assert.LessOrEqual(t, len(got), maxValidationMessageBytes+len("..."))
		assert.Equal(t, want, got, "%s", kindName(k))
	}
	for _, tc := range smallFailures() {
		t.Run(tc.name, func(t *testing.T) {
			eachNode(lineTree(t, tc.schema, tc.instance), func(node *jsonschema.ValidationError) {
				check(t, node.InstanceLocation, node.ErrorKind)
				for name, at := range lineLocations() {
					t.Run(name, func(t *testing.T) { check(t, at, node.ErrorKind) })
				}
			})
		})
	}
	long := strings.Repeat("x", 1<<20)
	names := make([]string, 50000)
	for i := range names {
		names[i] = "name-" + strconv.Itoa(i)
	}
	indexes := make([]int, 50000)
	for i := range indexes {
		indexes[i] = i * 7
	}
	wants := make([]any, 5000)
	for i := range wants {
		wants[i] = "value-" + strconv.Itoa(i)
	}
	odd := "a\x00\x7f" + string(rune(0xa0)) + "\xff\xfe" + string(rune(0x2028)) + "b" + string(rune(0x1f600))
	constructed := map[string]jsonschema.ErrorKind{
		"RefCycle":                &kind.RefCycle{URL: "mem:///a#", KeywordLocation1: "/a/$ref", KeywordLocation2: "/b/$ref"},
		"InvalidJsonValue":        &kind.InvalidJsonValue{Value: struct{}{}},
		"Schema":                  &kind.Schema{Location: "mem:///schema.json#/$defs/a'b"},
		"enum of thousands":       &kind.Enum{Got: "z", Want: wants},
		"enum of megabytes":       &kind.Enum{Got: "z", Want: []any{long, long}},
		"enum of one megabyte":    &kind.Enum{Got: "z", Want: []any{long}},
		"const of a megabyte":     &kind.Const{Got: 1, Want: long},
		"required many":           &kind.Required{Missing: names},
		"required one huge":       &kind.Required{Missing: []string{long}},
		"required none":           &kind.Required{},
		"dependency many":         &kind.Dependency{Prop: "p", Missing: names},
		"dependency huge prop":    &kind.Dependency{Prop: long, Missing: []string{"a"}},
		"dependentRequired many":  &kind.DependentRequired{Prop: "p'q", Missing: names},
		"additional many":         &kind.AdditionalProperties{Properties: names},
		"additional huge":         &kind.AdditionalProperties{Properties: []string{long}},
		"pattern huge got":        &kind.Pattern{Got: long, Want: "^a"},
		"pattern huge want":       &kind.Pattern{Got: "b", Want: long},
		"pattern controls":        &kind.Pattern{Got: odd, Want: "\\d+'\"\t"},
		"format huge got":         &kind.Format{Got: long, Want: "email", Err: errors.New("bad " + long)},
		"format huge want":        &kind.Format{Got: "a", Want: long, Err: errors.New("bad")},
		"format a number":         &kind.Format{Got: json.Number("1e400"), Want: "int32", Err: errors.New("overflow")},
		"format a float":          &kind.Format{Got: 1e21, Want: "int32", Err: errors.New("overflow")},
		"format a null":           &kind.Format{Got: nil, Want: "x", Err: errors.New("e")},
		"format a container":      &kind.Format{Got: []any{1}, Want: "x", Err: errors.New("e")},
		"format medium err":       &kind.Format{Got: strings.Repeat("y", 300), Want: "regex", Err: errors.New("error parsing regexp: " + strings.Repeat("y", 300))},
		"minContains many":        &kind.MinContains{Got: indexes, Want: 1234567},
		"minContains none":        &kind.MinContains{Want: 1234},
		"maxContains many":        &kind.MaxContains{Got: indexes, Want: 1234567},
		"maxContains none":        &kind.MaxContains{Want: 0},
		"minProperties grouped":   &kind.MinProperties{Got: 1234, Want: 5678},
		"type":                    &kind.Type{Got: "string", Want: []string{"integer", "null"}},
		"uniqueItems":             &kind.UniqueItems{Duplicates: [2]int{1, 2000}},
		"oneOf grouped":           &kind.OneOf{Subschemas: []int{1, 2}},
		"minimum floats":          &kind.Minimum{Got: big.NewRat(1, 3), Want: big.NewRat(1234567, 1)},
		"maximum floats":          &kind.Maximum{Got: big.NewRat(1, 3), Want: big.NewRat(1, 1<<40)},
		"exclusiveMinimum floats": &kind.ExclusiveMinimum{Got: big.NewRat(5, 1), Want: big.NewRat(5, 1)},
		"exclusiveMaximum floats": &kind.ExclusiveMaximum{Got: big.NewRat(5, 1), Want: big.NewRat(5, 1)},
		"multipleOf floats":       &kind.MultipleOf{Got: big.NewRat(5, 1), Want: big.NewRat(1, 3)},
	}
	for name, k := range constructed {
		t.Run(name, func(t *testing.T) {
			for atName, at := range lineLocations() {
				t.Run(atName, func(t *testing.T) { check(t, at, k) })
			}
		})
	}
	t.Run("every kind", func(t *testing.T) {
		for _, name := range everyKind {
			assert.True(t, seen[name], "no row reaches kind.%s", name)
		}
	})
}

// TestCappedValidationLineLayout sweeps the lengths of the text and of the
// place, in one-byte and three-byte characters, across the points where
// validationLine changes how it cuts.
func TestCappedValidationLineLayout(t *testing.T) {
	textLengths := []int{0, 1, 100, 400, 440, 442, 443, 444, 445, 500, 505, 506, 507, 508, 509, 510, 511, 512, 513, 514, 515, 516, 517, 518, 600, 5000}
	keyLengths := []int{1, 5, 55, 60, 61, 62, 63, 64, 65, 66, 100, 300, 440, 500, 505, 506, 507, 508, 509, 510, 511, 512, 513, 514, 515, 520, 1000, 70000}
	characters := []string{"a", "中"}
	for _, textChar := range characters {
		for _, keyChar := range append(characters, "~") {
			for _, offset := range []string{"", "x", "xy"} {
				for _, textLength := range textLengths {
					body := offset + strings.Repeat(textChar, textLength)
					for _, keyLength := range keyLengths {
						key := strings.Repeat(keyChar, keyLength)
						at := []string{key}
						want := validationLine(at, body)
						got := cappedValidationLine(at, func(w *cappedWriter) { w.WriteString(body) })
						if want != got {
							require.Equal(t, want, got, "text %q x %d (+%q), key %q x %d", textChar, textLength, offset, keyChar, keyLength)
						}
					}
				}
			}
		}
	}
	for _, textLength := range textLengths {
		body := strings.Repeat("t", textLength)
		assert.Equal(t, validationLine(nil, body), cappedValidationLine(nil, func(w *cappedWriter) { w.WriteString(body) }), "root, text %d", textLength)
	}
}

func TestCappedValidationLineBuildsNothingWhole(t *testing.T) {
	names := make([]string, 100000)
	for i := range names {
		names[i] = "name-" + strconv.Itoa(i)
	}
	huge := strings.Repeat("~", 1<<20)
	kinds := []jsonschema.ErrorKind{
		&kind.Required{Missing: names},
		&kind.AdditionalProperties{Properties: names},
		&kind.Pattern{Got: huge, Want: huge},
		&kind.Format{Got: huge, Want: huge, Err: errors.New(huge)},
	}
	for _, k := range kinds {
		measure := func() uint64 {
			before := allocatedBytes()
			cappedValidationLine([]string{huge}, func(w *cappedWriter) { kindText(k, w) })
			return allocatedBytes() - before
		}
		lowest := min(measure(), measure(), measure())
		assert.Less(t, lowest, uint64(16<<10), "%s, a 1 MiB place and a text of megabytes", kindName(k))
	}
}

// TestKindTextStopsPastTheLimit pins that a text too big for the line is the
// start of the library's text, and stops where the writer does.
func TestKindTextStopsPastTheLimit(t *testing.T) {
	long := strings.Repeat("x", 1<<20)
	names := make([]string, 100000)
	for i := range names {
		names[i] = "name-" + strconv.Itoa(i)
	}
	indexes := make([]int, 100000)
	wants := make([]any, 100000)
	for i := range wants {
		wants[i] = "value-" + strconv.Itoa(i)
		indexes[i] = i
	}
	kinds := []jsonschema.ErrorKind{
		&kind.Enum{Want: wants},
		&kind.Enum{Want: []any{long}},
		&kind.Const{Want: long},
		&kind.Required{Missing: names},
		&kind.Dependency{Prop: "p", Missing: names},
		&kind.DependentRequired{Prop: "p", Missing: names},
		&kind.AdditionalProperties{Properties: names},
		&kind.Pattern{Got: long, Want: long},
		&kind.Format{Got: long, Want: "regex", Err: errors.New(long)},
		&kind.MinContains{Got: indexes, Want: 1},
		&kind.MaxContains{Got: indexes, Want: 1},
	}
	for _, k := range kinds {
		whole := k.LocalizedString(validationMessages)
		require.Greater(t, len(whole), 1000, kindName(k))
		w := newCappedWriter(maxValidationMessageBytes + 4)
		kindText(k, w)
		got := w.String()
		assert.LessOrEqual(t, len(got), maxValidationMessageBytes+4, kindName(k))
		assert.Greater(t, len(got), maxValidationMessageBytes, "%s, enough for the line to cut it", kindName(k))
		assert.True(t, strings.HasPrefix(whole, got), "%s: %q", kindName(k), got)
	}
	t.Run("a writer with no limit stops past 512 bytes between pieces", func(t *testing.T) {
		for _, k := range []jsonschema.ErrorKind{
			&kind.Enum{Want: wants}, &kind.Required{Missing: names}, &kind.Dependency{Prop: "p", Missing: names},
			&kind.AdditionalProperties{Properties: names}, &kind.MinContains{Got: indexes, Want: 1}, &kind.MaxContains{Got: indexes, Want: 1},
		} {
			w := newCappedWriter(1 << 30)
			kindText(k, w)
			got := w.String()
			assert.Greater(t, len(got), maxValidationMessageBytes, kindName(k))
			assert.Less(t, len(got), maxValidationMessageBytes+64, kindName(k))
			assert.True(t, strings.HasPrefix(k.LocalizedString(validationMessages), got), kindName(k))
		}
	})
}

// countedError counts the times its text is asked for.
type countedError struct {
	text  string
	calls *int
}

func (e countedError) Error() string {
	*e.calls++
	return e.text
}

// localizedFailure answers a printer in its own words.
type localizedFailure struct{}

func (localizedFailure) Error() string { return "plain" }

func (localizedFailure) LocalizedError(p *message.Printer) string {
	return p.Sprintf("localized %d", 1234)
}

func TestKindTextFormatWritesItsErrOnlyWhenTheValueLeftRoom(t *testing.T) {
	calls := 0
	render := func(k *kind.Format) string {
		w := newCappedWriter(maxValidationMessageBytes + 4)
		kindText(k, w)
		return w.String()
	}
	assert.Equal(t, "'ab' is not valid email: bad", render(&kind.Format{Got: "ab", Want: "email", Err: countedError{"bad", &calls}}))
	assert.Equal(t, 1, calls)

	calls = 0
	got := render(&kind.Format{Got: strings.Repeat("a", 1<<20), Want: "email", Err: countedError{"bad", &calls}})
	assert.Len(t, got, maxValidationMessageBytes+4)
	assert.Zero(t, calls, "a value that fills the line leaves no room for its err, and the err is not asked for")

	calls = 0
	got = render(&kind.Format{Got: json.Number(strings.Repeat("1", 1<<20)), Want: "email", Err: countedError{"bad", &calls}})
	assert.Len(t, got, maxValidationMessageBytes+4)
	assert.Zero(t, calls)

	calls = 0
	got = render(&kind.Format{Got: "ab", Want: strings.Repeat("w", 1<<20), Err: countedError{"bad", &calls}})
	assert.Len(t, got, maxValidationMessageBytes+4)
	assert.Zero(t, calls, "a format name that fills the line does the same")

	// A value that fills the writer to the byte leaves the err no room, and
	// that is a cut: 516 = 1 + 493 + 1 + len(" is not valid email: ").
	calls = 0
	w := newCappedWriter(maxValidationMessageBytes + 4)
	kindText(&kind.Format{Got: strings.Repeat("a", 493), Want: "email", Err: countedError{"bad", &calls}}, w)
	assert.Len(t, w.String(), maxValidationMessageBytes+4)
	assert.True(t, w.Cut(), "the err was dropped")
	assert.Zero(t, calls)
	w = newCappedWriter(maxValidationMessageBytes + 4)
	kindText(&kind.Format{Got: strings.Repeat("a", 492), Want: "email", Err: countedError{"b", &calls}}, w)
	assert.Len(t, w.String(), maxValidationMessageBytes+4, "one byte of room takes the err whole")
	assert.False(t, w.Cut())
	w = newCappedWriter(maxValidationMessageBytes + 4)
	kindText(&kind.Format{Got: strings.Repeat("a", 493), Want: "email"}, w)
	assert.False(t, w.Cut(), "no err, nothing dropped")

	assert.Equal(t, "'ab' is not valid email: localized 1,234", render(&kind.Format{Got: "ab", Want: "email", Err: localizedFailure{}}),
		"an err with its own localization is asked in the printer's words")
	assert.Equal(t, "'ab' is not valid email: ", render(&kind.Format{Got: "ab", Want: "email"}), "no err is no text")
}
