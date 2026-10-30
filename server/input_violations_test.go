package server

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// invalidCall validates args, a JSON text, against schema as a tools/call of
// the tool called "t" does, and returns the failure validation answers.
func invalidCall(t testing.TB, schema, args string) *inputInvalid {
	t.Helper()
	compiled, err := compileSchema("t", []byte(schema))
	require.NoError(t, err)
	var params mcp.CallToolParams
	require.NoError(t, json.Unmarshal([]byte(`{"name":"t","arguments":`+args+`}`), &params))
	return invalidArguments(t, compiled, callArguments{raw: params.RawArguments, decoded: params.Arguments})
}

// invalidArguments validates args against compiled and returns the failure.
func invalidArguments(t testing.TB, compiled *jsonschema.Schema, args callArguments) *inputInvalid {
	t.Helper()
	ran, err := validateArguments(&cachedToolSchema{compiled: compiled}, args)
	require.True(t, ran)
	inv, ok := errors.AsType[*inputInvalid](err)
	require.True(t, ok, "validation must fail with an *inputInvalid, got %v", err)
	return inv
}

// projectionRow is a failing call, the failure the hook sees and the text the
// default refusal answers.
type projectionRow struct {
	name, schema, args string
	// call, when set, makes the failure instead of schema and args.
	call func(t testing.TB) *inputInvalid
	text string
	want InputValidationFailure
}

func one(v InputViolation) InputValidationFailure {
	return InputValidationFailure{Violations: []InputViolation{v}}
}

const failed = "input schema validation failed: "

// projectionRows holds a call for each keyword the list knows; the placements
// of propertyNames and unevaluatedProperties are input_violations_pointer_test.go's.
func projectionRows() []projectionRow {
	sevens := "0." + strings.Repeat("7", 1100)
	widest := new(big.Rat).SetFrac(new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)), new(big.Int).Lsh(big.NewInt(1), 255)).FloatString(255)
	deep := strings.Repeat(`{"a":`, 128) + `{}` + strings.Repeat(`}`, 128)
	return []projectionRow{
		{
			name: "type", schema: `{"type":"object","properties":{"x":{"type":"string"}}}`, args: `{"x":5}`,
			text: failed + "/x: got number, want string",
			want: one(InputViolation{Pointer: "/x", Keyword: "type", SchemaLocation: "/properties/x/type",
				Expected: ViolationExpected{Types: []string{"string"}}, Actual: ViolationActual{Kind: "number", Value: "5"},
				Message: "/x: got number, want string"}),
		},
		{
			name: "type of arguments that are no object", schema: `{"type":"object"}`, args: `[]`,
			text: failed + "<root>: got array, want object",
			want: one(InputViolation{Keyword: "type", SchemaLocation: "/type",
				Expected: ViolationExpected{Types: []string{"object"}}, Actual: ViolationActual{Kind: "array"},
				Message: "<root>: got array, want object"}),
		},
		{
			name: "required", schema: `{"required":["a","b"]}`, args: `{}`,
			text: failed + "<root>: missing properties 'a', 'b'",
			want: one(InputViolation{Keyword: "required", SchemaLocation: "/required",
				Expected: ViolationExpected{Names: []string{"a", "b"}}, Actual: ViolationActual{Kind: "object"},
				Message: "<root>: missing properties 'a', 'b'"}),
		},
		{
			name: "additionalProperties", schema: `{"properties":{"a":{}},"additionalProperties":false}`, args: `{"z":1,"a":2}`,
			text: failed + "<root>: additional properties 'z' not allowed",
			want: one(InputViolation{Keyword: "additionalProperties", SchemaLocation: "/additionalProperties",
				Actual: ViolationActual{Kind: "object", Names: []string{"z"}}, Message: "<root>: additional properties 'z' not allowed"}),
		},
		{
			name: "propertyNames", schema: `{"propertyNames":{"minLength":5}}`, args: `{"bad":1}`,
			text: failed + "<root>: minLength: got 3, want 5",
			want: one(InputViolation{Keyword: "minLength", SchemaLocation: "/propertyNames/minLength", Property: "bad",
				Expected: ViolationExpected{Limit: new(5)}, Actual: ViolationActual{Kind: "string", Value: `"bad"`, Count: new(3)},
				Message: "<root>: minLength: got 3, want 5"}),
		},
		{
			name: "enum", schema: `{"properties":{"x":{"enum":["a",1]}}}`, args: `{"x":"z"}`,
			text: failed + "/x: value must be one of 'a', 1",
			want: one(InputViolation{Pointer: "/x", Keyword: "enum", SchemaLocation: "/properties/x/enum",
				Expected: ViolationExpected{Values: []string{`"a"`, `1`}}, Actual: ViolationActual{Kind: "string", Value: `"z"`},
				Message: "/x: value must be one of 'a', 1"}),
		},
		{
			name: "const", schema: `{"properties":{"x":{"const":"a"}}}`, args: `{"x":"b"}`,
			text: failed + "/x: value must be 'a'",
			want: one(InputViolation{Pointer: "/x", Keyword: "const", SchemaLocation: "/properties/x/const",
				Expected: ViolationExpected{Values: []string{`"a"`}}, Actual: ViolationActual{Kind: "string", Value: `"b"`},
				Message: "/x: value must be 'a'"}),
		},
		{
			name: "minimum of an exact decimal", schema: `{"properties":{"x":{"minimum":0.1}}}`, args: `{"x":0.05}`,
			text: failed + "/x: minimum: got 0.05, want 0.1",
			want: one(InputViolation{Pointer: "/x", Keyword: "minimum", SchemaLocation: "/properties/x/minimum",
				Expected: ViolationExpected{Bound: &ViolationBound{Text: "0.1", Exact: true}}, Actual: ViolationActual{Kind: "number", Value: "0.05"},
				Message: "/x: minimum: got 0.05, want 0.1"}),
		},
		{
			name: "minimum whose exact text is past 256 bytes", schema: `{"properties":{"x":{"minimum":` + widest + `}}}`, args: `{"x":1}`,
			text: failed + "/x: minimum: got 1, want 2",
			want: one(InputViolation{Pointer: "/x", Keyword: "minimum", SchemaLocation: "/properties/x/minimum",
				Expected: ViolationExpected{Bound: &ViolationBound{Text: "2"}}, Actual: ViolationActual{Kind: "number", Value: "1"},
				Message: "/x: minimum: got 1, want 2"}),
		},
		{
			name: "maximum under a $ref", schema: `{"properties":{"x":{"$ref":"#/$defs/limit"}},"$defs":{"limit":{"maximum":5}}}`, args: `{"x":6}`,
			text: failed + "/x: maximum: got 6, want 5",
			want: one(InputViolation{Pointer: "/x", Keyword: "maximum", SchemaLocation: "/$defs/limit/maximum",
				Expected: ViolationExpected{Bound: &ViolationBound{Text: "5", Exact: true}}, Actual: ViolationActual{Kind: "number", Value: "6"},
				Message: "/x: maximum: got 6, want 5"}),
		},
		{
			name: "exclusiveMinimum", schema: `{"properties":{"x":{"exclusiveMinimum":10}}}`, args: `{"x":10}`,
			text: failed + "/x: exclusiveMinimum: got 10, want 10",
			want: one(InputViolation{Pointer: "/x", Keyword: "exclusiveMinimum", SchemaLocation: "/properties/x/exclusiveMinimum",
				Expected: ViolationExpected{Bound: &ViolationBound{Text: "10", Exact: true}}, Actual: ViolationActual{Kind: "number", Value: "10"},
				Message: "/x: exclusiveMinimum: got 10, want 10"}),
		},
		{
			name: "exclusiveMaximum of draft 4", schema: `{` + draft4 + `,"properties":{"x":{"maximum":10,"exclusiveMaximum":true}}}`, args: `{"x":10}`,
			text: failed + "/x: exclusiveMaximum: got 10, want 10",
			want: one(InputViolation{Pointer: "/x", Keyword: "exclusiveMaximum", SchemaLocation: "/properties/x/maximum",
				Expected: ViolationExpected{Bound: &ViolationBound{Text: "10", Exact: true}}, Actual: ViolationActual{Kind: "number", Value: "10"},
				Message: "/x: exclusiveMaximum: got 10, want 10"}),
		},
		{
			name: "multipleOf", schema: `{"properties":{"x":{"multipleOf":3}}}`, args: `{"x":10}`,
			text: failed + "/x: multipleOf: got 10, want 3",
			want: one(InputViolation{Pointer: "/x", Keyword: "multipleOf", SchemaLocation: "/properties/x/multipleOf",
				Expected: ViolationExpected{Bound: &ViolationBound{Text: "3", Exact: true}}, Actual: ViolationActual{Kind: "number", Value: "10"},
				Message: "/x: multipleOf: got 10, want 3"}),
		},
		{
			name: "maxLength", schema: `{"properties":{"x":{"maxLength":2}}}`, args: `{"x":"héllo"}`,
			text: failed + "/x: maxLength: got 5, want 2",
			want: one(InputViolation{Pointer: "/x", Keyword: "maxLength", SchemaLocation: "/properties/x/maxLength",
				Expected: ViolationExpected{Limit: new(2)}, Actual: ViolationActual{Kind: "string", Value: `"héllo"`, Count: new(5)},
				Message: "/x: maxLength: got 5, want 2"}),
		},
		{
			name: "pattern", schema: `{"properties":{"x":{"pattern":"^a"}}}`, args: `{"x":"b"}`,
			text: failed + "/x: 'b' does not match pattern '^a'",
			want: one(InputViolation{Pointer: "/x", Keyword: "pattern", SchemaLocation: "/properties/x/pattern",
				Expected: ViolationExpected{Pattern: "^a"}, Actual: ViolationActual{Kind: "string", Value: `"b"`},
				Message: "/x: 'b' does not match pattern '^a'"}),
		},
		{
			name: "format", schema: `{` + draft7 + `,"properties":{"x":{"format":"email"}}}`, args: `{"x":"no"}`,
			text: failed + "/x: 'no' is not valid email: missing @",
			want: one(InputViolation{Pointer: "/x", Keyword: "format", SchemaLocation: "/properties/x/format",
				Expected: ViolationExpected{Format: "email"}, Actual: ViolationActual{Kind: "string", Value: `"no"`},
				Message: "/x: 'no' is not valid email: missing @"}),
		},
		{
			name: "minItems", schema: `{"properties":{"a":{"minItems":2}}}`, args: `{"a":[1]}`,
			text: failed + "/a: minItems: got 1, want 2",
			want: one(InputViolation{Pointer: "/a", Keyword: "minItems", SchemaLocation: "/properties/a/minItems",
				Expected: ViolationExpected{Limit: new(2)}, Actual: ViolationActual{Kind: "array", Count: new(1)},
				Message: "/a: minItems: got 1, want 2"}),
		},
		{
			name: "minContains with one match", schema: `{"properties":{"a":{"contains":{"type":"string"},"minContains":2}}}`, args: `{"a":["s",1,2]}`,
			text: failed + "/a/1: got number, want string; /a/2: got number, want string",
			want: one(InputViolation{Pointer: "/a", Keyword: "minContains", SchemaLocation: "/properties/a/minContains",
				Expected: ViolationExpected{Limit: new(2)}, Actual: ViolationActual{Kind: "array", Count: new(1), Indexes: []int{0}},
				Message: "/a: min 2 items required to match contains schema, but matched 1 items at 0"}),
		},
		{
			name: "contains with no match", schema: `{"properties":{"a":{"contains":{"type":"string"}}}}`, args: `{"a":[1,2]}`,
			text: failed + "/a/0: got number, want string; /a/1: got number, want string",
			want: one(InputViolation{Pointer: "/a", Keyword: "contains", SchemaLocation: "/properties/a/contains",
				Actual: ViolationActual{Kind: "array", Count: new(0)}, Message: "/a: no items match contains schema"}),
		},
		{
			name: "maxContains", schema: `{"properties":{"a":{"contains":{"type":"string"},"maxContains":1}}}`, args: `{"a":["s","t",1]}`,
			text: failed + "/a: max 1 items required to match contains schema, but matched 2 items at 0 1",
			want: one(InputViolation{Pointer: "/a", Keyword: "maxContains", SchemaLocation: "/properties/a/maxContains",
				Expected: ViolationExpected{Limit: new(1)}, Actual: ViolationActual{Kind: "array", Count: new(2), Indexes: []int{0, 1}},
				Message: "/a: max 1 items required to match contains schema, but matched 2 items at 0 1"}),
		},
		{
			name: "uniqueItems", schema: `{"properties":{"a":{"uniqueItems":true}}}`, args: `{"a":[1,2,1]}`,
			text: failed + "/a: items at 0 and 2 are equal",
			want: one(InputViolation{Pointer: "/a", Keyword: "uniqueItems", SchemaLocation: "/properties/a/uniqueItems",
				Actual: ViolationActual{Kind: "array", Indexes: []int{0, 2}, Values: []string{"1"}}, Message: "/a: items at 0 and 2 are equal"}),
		},
		{
			name: "anyOf", schema: `{"properties":{"x":{"anyOf":[{"type":"string"},{"minimum":10}]}}}`, args: `{"x":5}`,
			text: failed + "/x: got number, want string; /x: minimum: got 5, want 10",
			want: InputValidationFailure{Violations: []InputViolation{
				{Pointer: "/x", Keyword: "type", SchemaLocation: "/properties/x/anyOf/0/type",
					Expected: ViolationExpected{Types: []string{"string"}}, Actual: ViolationActual{Kind: "number", Value: "5"}, Message: "/x: got number, want string"},
				{Pointer: "/x", Keyword: "minimum", SchemaLocation: "/properties/x/anyOf/1/minimum",
					Expected: ViolationExpected{Bound: &ViolationBound{Text: "10", Exact: true}}, Actual: ViolationActual{Kind: "number", Value: "5"}, Message: "/x: minimum: got 5, want 10"},
			}},
		},
		{
			name: "allOf", schema: `{"properties":{"x":{"allOf":[{"type":"string"},{"minimum":10}]}}}`, args: `{"x":5}`,
			text: failed + "/x: got number, want string; /x: minimum: got 5, want 10",
			want: InputValidationFailure{Violations: []InputViolation{
				{Pointer: "/x", Keyword: "type", SchemaLocation: "/properties/x/allOf/0/type",
					Expected: ViolationExpected{Types: []string{"string"}}, Actual: ViolationActual{Kind: "number", Value: "5"}, Message: "/x: got number, want string"},
				{Pointer: "/x", Keyword: "minimum", SchemaLocation: "/properties/x/allOf/1/minimum",
					Expected: ViolationExpected{Bound: &ViolationBound{Text: "10", Exact: true}}, Actual: ViolationActual{Kind: "number", Value: "5"}, Message: "/x: minimum: got 5, want 10"},
			}},
		},
		{
			name: "oneOf with none matched", schema: `{"properties":{"x":{"oneOf":[{"type":"string"},{"minimum":10}]}}}`, args: `{"x":5}`,
			text: failed + "/x: got number, want string; /x: minimum: got 5, want 10",
			want: InputValidationFailure{Violations: []InputViolation{
				{Pointer: "/x", Keyword: "type", SchemaLocation: "/properties/x/oneOf/0/type",
					Expected: ViolationExpected{Types: []string{"string"}}, Actual: ViolationActual{Kind: "number", Value: "5"}, Message: "/x: got number, want string"},
				{Pointer: "/x", Keyword: "minimum", SchemaLocation: "/properties/x/oneOf/1/minimum",
					Expected: ViolationExpected{Bound: &ViolationBound{Text: "10", Exact: true}}, Actual: ViolationActual{Kind: "number", Value: "5"}, Message: "/x: minimum: got 5, want 10"},
			}},
		},
		{
			name: "oneOf matched twice", schema: `{"properties":{"x":{"oneOf":[{"type":"integer"},{"minimum":1}]}}}`, args: `{"x":5}`,
			text: failed + "/x: 'oneOf' failed, subschemas 0, 1 matched",
			want: one(InputViolation{Pointer: "/x", Keyword: "oneOf", SchemaLocation: "/properties/x/oneOf",
				Expected: ViolationExpected{Indexes: []int{0, 1}}, Actual: ViolationActual{Kind: "number", Value: "5"},
				Message: "/x: 'oneOf' failed, subschemas 0, 1 matched"}),
		},
		{
			name: "not", schema: `{"properties":{"x":{"not":{"type":"number"}}}}`, args: `{"x":5}`,
			text: failed + "/x: 'not' failed",
			want: one(InputViolation{Pointer: "/x", Keyword: "not", SchemaLocation: "/properties/x/not",
				Actual: ViolationActual{Kind: "number", Value: "5"}, Message: "/x: 'not' failed"}),
		},
		{
			name: "false", schema: `{"properties":{"x":false}}`, args: `{"x":5}`,
			text: failed + "/x: false schema",
			want: one(InputViolation{Pointer: "/x", Keyword: "false", SchemaLocation: "/properties/x",
				Actual: ViolationActual{Kind: "number", Value: "5"}, Message: "/x: false schema"}),
		},
		{
			name: "minProperties", schema: `{"minProperties":2}`, args: `{"a":1}`,
			text: failed + "<root>: minProperties: got 1, want 2",
			want: one(InputViolation{Keyword: "minProperties", SchemaLocation: "/minProperties",
				Expected: ViolationExpected{Limit: new(2)}, Actual: ViolationActual{Kind: "object", Count: new(1)},
				Message: "<root>: minProperties: got 1, want 2"}),
		},
		{
			name: "dependentRequired", schema: `{"dependentRequired":{"a":["b"]}}`, args: `{"a":1}`,
			text: failed + "<root>: properties 'b' required, if 'a' exists",
			want: one(InputViolation{Keyword: "dependentRequired", SchemaLocation: "/dependentRequired/a", Property: "a",
				Expected: ViolationExpected{Names: []string{"b"}}, Actual: ViolationActual{Kind: "object"},
				Message: "<root>: properties 'b' required, if 'a' exists"}),
		},
		{
			name: "dependencies of draft 7", schema: `{` + draft7 + `,"dependencies":{"a":["b"]}}`, args: `{"a":1}`,
			text: failed + "<root>: properties 'b' required, if 'a' exists",
			want: one(InputViolation{Keyword: "dependencies", SchemaLocation: "/dependencies/a", Property: "a",
				Expected: ViolationExpected{Names: []string{"b"}}, Actual: ViolationActual{Kind: "object"},
				Message: "<root>: properties 'b' required, if 'a' exists"}),
		},
		{
			name: "additionalItems", schema: `{` + draft7 + `,"properties":{"a":{"items":[{}],"additionalItems":false}}}`, args: `{"a":[1,2]}`,
			text: failed + "/a: last 1 additionalItem(s) not allowed",
			want: one(InputViolation{Pointer: "/a", Keyword: "additionalItems", SchemaLocation: "/properties/a/additionalItems",
				Actual: ViolationActual{Kind: "array", Count: new(1)}, Message: "/a: last 1 additionalItem(s) not allowed"}),
		},
		{
			name: "numberOutOfRange", schema: `{"type":"object"}`, args: `{"n":` + sevens + `}`,
			text: failed + "/n: number " + sevens[:64] + "... is out of range",
			want: InputValidationFailure{Incomplete: true, Cut: true, Violations: []InputViolation{{Pointer: "/n", Keyword: "numberOutOfRange",
				Actual: ViolationActual{Kind: "number", Value: sevens[:64]}, Message: "/n: number " + sevens[:64] + "... is out of range", Cut: true}}},
		},
		{
			name: "nestedTooDeep", schema: `{"type":"object"}`, args: deep,
			text: failed + strings.Repeat("/a", 128) + ": value is nested deeper than 128 levels",
			want: InputValidationFailure{Incomplete: true, Violations: []InputViolation{{Pointer: strings.Repeat("/a", 128), Keyword: "nestedTooDeep",
				Actual: ViolationActual{Kind: "object"}, Message: strings.Repeat("/a", 128) + ": value is nested deeper than 128 levels"}}},
		},
		{
			name: "unencodable",
			call: func(t testing.TB) *inputInvalid {
				compiled, err := compileSchema("t", []byte(`{"type":"object"}`))
				require.NoError(t, err)
				return invalidArguments(t, compiled, callArguments{decoded: map[string]any{"x": math.NaN()}})
			},
			text: "invalid arguments: json: unsupported value: NaN",
			want: InputValidationFailure{Incomplete: true, Violations: []InputViolation{{Keyword: "unencodable",
				Message: "invalid arguments: json: unsupported value: NaN"}}},
		},
	}
}

func (row projectionRow) invalid(t testing.TB) *inputInvalid {
	if row.call != nil {
		return row.call(t)
	}
	return invalidCall(t, row.schema, row.args)
}

// Each keyword projects with the fields it has.
func TestProjectionKeywordRows(t *testing.T) {
	for _, row := range projectionRows() {
		t.Run(row.name, func(t *testing.T) {
			assert.Equal(t, row.want, projectFailure(row.invalid(t)))
		})
	}
}

// The refusal's text is the default refusal's for each row, pinned.
func TestProjectionTextUnchanged(t *testing.T) {
	for _, row := range projectionRows() {
		t.Run(row.name, func(t *testing.T) {
			inv := row.invalid(t)
			assert.Equal(t, row.text, inv.Error())
			assert.Equal(t, row.text, validationToolResult(inv).Content[0].(mcp.TextContent).Text)
		})
	}
}

// A bound with no finite decimal is the shortest form of its float64, not
// exact; none comes from a JSON literal, so the tree is built by hand.
func TestProjectionBoundOfAThird(t *testing.T) {
	const url = "mem:///flexmcp/tools/t/input-schema.json"
	inv := &inputInvalid{
		cause: &jsonschema.ValidationError{SchemaURL: url + "#", ErrorKind: &kind.Schema{Location: url + "#"}, Causes: []*jsonschema.ValidationError{{
			SchemaURL: url + "#/properties/x", InstanceLocation: []string{"x"},
			ErrorKind: &kind.Minimum{Got: big.NewRat(1, 4), Want: big.NewRat(1, 3)},
		}}},
		instance:    map[string]any{"x": 0.25},
		resourceURL: url,
	}
	assert.Equal(t, one(InputViolation{Pointer: "/x", Keyword: "minimum", SchemaLocation: "/properties/x/minimum",
		Expected: ViolationExpected{Bound: &ViolationBound{Text: "0.3333333333333333"}}, Actual: ViolationActual{Kind: "number", Value: "0.25"},
		Message: "/x: minimum: got 0.25, want 0.3333333333333333"}), projectFailure(inv))
}

// additionalProperties lists the 20 least names in byte order, whatever the
// library's order, and counts the rest.
func TestProjectionAdditionalPropertiesListsTheLeastNames(t *testing.T) {
	members := make([]string, 25)
	for i := range members {
		members[i] = `"n` + strconv.Itoa(100+(i*7)%25) + `":1`
	}
	f := projectFailure(invalidCall(t, `{"additionalProperties":false}`, `{`+strings.Join(members, ",")+`}`))
	require.Len(t, f.Violations, 1)
	want := make([]string, 20)
	for i := range want {
		want[i] = "n" + strconv.Itoa(100+i)
	}
	assert.Equal(t, want, f.Violations[0].Actual.Names)
	assert.Equal(t, 5, f.Violations[0].Actual.Omitted)
	assert.True(t, f.Violations[0].Cut)
	assert.True(t, f.Cut)
}

// previewNodes counts the values a preview, JSON text or a prefix of one,
// writes: each scalar, array and object, and each member's value.
func previewNodes(text string) int {
	nodes := 0
	var open []byte
	key := false
	for i := 0; i < len(text); i++ {
		switch c := text[i]; c {
		case '"':
			j := i + 1
			for j < len(text) && text[j] != '"' {
				if text[j] == '\\' {
					j++
				}
				j++
			}
			if !key {
				nodes++
			}
			i = j
		case '[', '{':
			nodes++
			open = append(open, c)
			key = c == '{'
		case ']', '}':
			open = open[:len(open)-1]
			key = false
		case ',':
			key = len(open) > 0 && open[len(open)-1] == '{'
		case ':':
			key = false
		default:
			nodes++
			for i+1 < len(text) && strings.IndexByte(",]}:", text[i+1]) < 0 {
				i++
			}
		}
	}
	return nodes
}

// violationStrings are the strings of v but its message.
func violationStrings(v InputViolation) []string {
	strs := []string{v.Pointer, v.Keyword, v.SchemaLocation, v.Property, v.Expected.Pattern, v.Expected.Format, v.Actual.Kind, v.Actual.Value}
	strs = append(strs, v.Expected.Types...)
	strs = append(strs, v.Expected.Names...)
	strs = append(strs, v.Expected.Values...)
	strs = append(strs, v.Actual.Names...)
	strs = append(strs, v.Actual.Values...)
	if v.Expected.Bound != nil {
		strs = append(strs, v.Expected.Bound.Text)
	}
	return strs
}

// violationBytes is what v's field values take: each string's bytes and each
// number's decimal digits.
func violationBytes(v InputViolation) int {
	size := len(v.Message)
	for _, s := range violationStrings(v) {
		size += len(s)
	}
	numbers := slices.Concat(v.Expected.Indexes, v.Actual.Indexes)
	for _, n := range []*int{v.Expected.Limit, v.Actual.Count} {
		if n != nil {
			numbers = append(numbers, *n)
		}
	}
	for _, n := range numbers {
		size += len(strconv.Itoa(n))
	}
	return size
}

// assertWithinBudgets checks f against the list's budgets, in bytes of the
// field values: the list at most 16 KiB and 512 preview nodes, each violation
// at most 2 KiB, each string at most 256 bytes and each message at most 512
// and a "...".
func assertWithinBudgets(t *testing.T, f InputValidationFailure) {
	t.Helper()
	assert.LessOrEqual(t, len(f.Violations), 10)
	nodes, total := 0, 0
	for _, v := range f.Violations {
		size := violationBytes(v)
		total += size
		assert.LessOrEqual(t, size, 2<<10, "a violation's bytes")
		assert.LessOrEqual(t, len(v.Message), 512+len("..."))
		for _, s := range violationStrings(v) {
			assert.LessOrEqual(t, len(s), 256)
		}
		assert.LessOrEqual(t, len(v.Expected.Types)+len(v.Expected.Names)+len(v.Expected.Values)+len(v.Expected.Indexes), 20)
		assert.LessOrEqual(t, len(v.Actual.Names)+len(v.Actual.Values)+len(v.Actual.Indexes), 20)
		for _, p := range append(slices.Concat(v.Expected.Values, v.Actual.Values), v.Actual.Value) {
			nodes += previewNodes(p)
		}
	}
	assert.LessOrEqual(t, total, 16<<10, "the list's bytes")
	assert.LessOrEqual(t, nodes, 512, "preview nodes")
}

// Each shape that stresses a budget stays within the budgets, and Cut
// says where one bit.
func TestProjectionBudgets(t *testing.T) {
	zeros := "[" + strings.TrimSuffix(strings.Repeat("0,", 1<<20), ",") + "]"
	var numbers strings.Builder
	for i := range 1 << 20 {
		if i > 0 {
			numbers.WriteByte(',')
		}
		numbers.WriteString(strconv.Itoa(i))
	}
	huge := strings.Repeat("a", 1<<20)
	tilde := strings.Repeat("~", 1<<20)
	members := func(n int, member func(i int) string) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = member(i)
		}
		return "{" + strings.Join(parts, ",") + "}"
	}
	branches := `{"type":"string"},{"type":"boolean"},{"type":"null"},{"type":"array"},{"type":"object"},` +
		`{"minimum":100},{"maximum":0},{"multipleOf":7},{"const":2},{"enum":[3]}`
	items := "[" + strings.TrimSuffix(strings.Repeat("1,", 5000), ",") + "]"
	// whole checks that each violation of an escape-heavy key keeps its sides.
	whole := func(t *testing.T, f InputValidationFailure) {
		require.Len(t, f.Violations, 10)
		for _, v := range f.Violations {
			assert.Equal(t, []string{"string"}, v.Expected.Types)
			assert.Equal(t, "number", v.Actual.Kind)
			assert.Equal(t, "1", v.Actual.Value)
			assert.Len(t, v.Pointer, 256)
			assert.True(t, v.Cut)
		}
		assert.True(t, f.Cut)
	}

	for _, tc := range []struct {
		name, schema, args string
		check              func(t *testing.T, f InputValidationFailure)
	}{
		{"an enum entry of 2^20 zeros", `{"properties":{"x":{"enum":[` + zeros + `]}}}`, `{"x":0}`, func(t *testing.T, f InputValidationFailure) {
			require.Len(t, f.Violations, 1)
			require.Len(t, f.Violations[0].Expected.Values, 1)
			assert.True(t, strings.HasPrefix(f.Violations[0].Expected.Values[0], "[0,0,0"))
			assert.Equal(t, "/x: 'enum' failed", f.Violations[0].Message)
			assert.True(t, f.Violations[0].Cut)
			assert.True(t, f.Cut)
		}},
		{"ten members against an enum entry of 2^20 zeros", `{"additionalProperties":{"enum":[` + zeros + `]}}`,
			members(10, func(i int) string { return `"m` + strconv.Itoa(i) + `":0` }), func(t *testing.T, f InputValidationFailure) {
				require.Len(t, f.Violations, 10)
				assert.True(t, f.Cut)
			}},
		{"an enum of 2^20 primitives", `{"properties":{"x":{"enum":[` + numbers.String() + `]}}}`, `{"x":"z"}`, func(t *testing.T, f InputValidationFailure) {
			require.Len(t, f.Violations, 1)
			want := make([]string, 20)
			for i := range want {
				want[i] = strconv.Itoa(i)
			}
			assert.Equal(t, want, f.Violations[0].Expected.Values)
			assert.Equal(t, 1<<20-20, f.Violations[0].Expected.Omitted)
			assert.True(t, strings.HasSuffix(f.Violations[0].Message, "..."))
			assert.True(t, f.Cut)
		}},
		{"ten keys of 301 bytes against an enum of twenty 200-byte strings",
			`{"additionalProperties":{"enum":[` + strings.TrimSuffix(strings.Repeat(`"`+strings.Repeat("e", 200)+`",`, 20), ",") + `]}}`,
			members(10, func(i int) string { return `"` + strings.Repeat("k", 300) + strconv.Itoa(i) + `":1` }), func(t *testing.T, f InputValidationFailure) {
				assert.Less(t, len(f.Violations), 10, "the 16 KiB budget leaves violations out")
				assert.Equal(t, 10, len(f.Violations)+f.Omitted)
				assert.True(t, f.Cut)
			}},
		{"an enum of twenty 200-byte strings", `{"properties":{"x":{"enum":[` + strings.TrimSuffix(strings.Repeat(`"`+strings.Repeat("e", 200)+`",`, 20), ",") + `]}}}`, `{"x":1}`, func(t *testing.T, f InputValidationFailure) {
			require.Len(t, f.Violations, 1)
			v := f.Violations[0]
			assert.NotEmpty(t, v.Expected.Values)
			assert.Equal(t, 20, len(v.Expected.Values)+v.Expected.Omitted)
			assert.Positive(t, v.Expected.Omitted, "the 2 KiB budget leaves entries out")
			assert.True(t, v.Cut)
		}},
		{"a missing name of 1 MiB", `{"required":["` + huge + `"]}`, `{}`, func(t *testing.T, f InputValidationFailure) {
			require.Len(t, f.Violations, 1)
			assert.Equal(t, []string{huge[:256]}, f.Violations[0].Expected.Names)
			assert.True(t, strings.HasSuffix(f.Violations[0].Message, "..."))
			assert.True(t, f.Violations[0].Cut)
		}},
		{"a key of 1 MiB of tildes with ten failing leaves", `{"additionalProperties":{"items":{"type":"string"}}}`,
			`{"` + tilde + `":[1,2,3,4,5,6,7,8,9,10]}`, func(t *testing.T, f InputValidationFailure) {
				require.Len(t, f.Violations, 10)
				assert.Equal(t, 0, f.Omitted)
				for _, v := range f.Violations {
					assert.True(t, strings.HasPrefix(v.Pointer, "/~0~0"))
					assert.True(t, v.Cut)
				}
				assert.True(t, f.Cut)
			}},
		{"ten keys of 1,000 '<' characters", `{"additionalProperties":{"type":"string"}}`,
			members(10, func(i int) string { return `"` + strings.Repeat("<", 1000) + strconv.Itoa(i) + `":1` }), whole},
		{"ten keys of 1,000 control characters", `{"additionalProperties":{"type":"string"}}`,
			members(10, func(i int) string { return `"` + strings.Repeat(escapeU+"0001", 1000) + strconv.Itoa(i) + `":1` }), whole},
		{"twenty names of 20 KB against a ten-branch anyOf", `{"additionalProperties":{"anyOf":[` + branches + `]}}`,
			members(20, func(i int) string { return `"` + strings.Repeat(string(rune('a'+i)), 20000) + `":1` }), func(t *testing.T, f InputValidationFailure) {
				require.Len(t, f.Violations, 10)
				assert.Equal(t, 190, f.Omitted)
				assert.True(t, f.Cut)
			}},
		{"a string of 1 MiB against format regex", `{` + draft7 + `,"properties":{"x":{"format":"regex"}}}`, `{"x":"(` + huge + `"}`, func(t *testing.T, f InputValidationFailure) {
			require.Len(t, f.Violations, 1)
			assert.Equal(t, "regex", f.Violations[0].Expected.Format)
			assert.True(t, strings.HasPrefix(f.Violations[0].Actual.Value, `"(aaa`))
			assert.True(t, f.Violations[0].Cut)
		}},
		{"5,000 failing items", `{"properties":{"a":{"items":{"type":"string"}}}}`, `{"a":` + items + `}`, func(t *testing.T, f InputValidationFailure) {
			require.Len(t, f.Violations, 10)
			assert.Equal(t, 4990, f.Omitted)
			assert.False(t, f.Incomplete)
			assert.True(t, f.Cut)
			pointers := make([]string, len(f.Violations))
			for i, v := range f.Violations {
				pointers[i] = v.Pointer
			}
			assert.Equal(t, []string{"/a/0", "/a/1", "/a/10", "/a/100", "/a/1000", "/a/1001", "/a/1002", "/a/1003", "/a/1004", "/a/1005"}, pointers)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := projectFailure(invalidCall(t, tc.schema, tc.args))
			assertWithinBudgets(t, f)
			tc.check(t, f)
		})
	}
}

// The list is the same from run to run, though the library walks an object's
// members in map order: the 10 least by pointer segments, then schema
// location, then keyword (TestProjectionUnknownPointerSortsLast for an
// unknown pointer).
func TestProjectionOrderIsStable(t *testing.T) {
	schema := `{"required":["q"],"additionalProperties":{"minimum":10,"multipleOf":3}}`
	args := `{"m00":1,"m01":1,"m02":1,"m03":1,"m04":1,"m05":1,"m06":1}`
	type entry struct{ pointer, location string }
	want := []entry{{"", "/required"}}
	for _, member := range []string{"m00", "m01", "m02", "m03", "m04"} {
		want = append(want, entry{"/" + member, "/additionalProperties/minimum"}, entry{"/" + member, "/additionalProperties/multipleOf"})
	}
	want = want[:10]
	first := projectFailure(invalidCall(t, schema, args))
	got := make([]entry, len(first.Violations))
	for i, v := range first.Violations {
		got[i] = entry{v.Pointer, v.SchemaLocation}
	}
	assert.Equal(t, want, got)
	assert.Equal(t, 5, first.Omitted)
	for range 100 {
		require.Equal(t, first, projectFailure(invalidCall(t, schema, args)))
	}
}

// boundsOf is the bound of each violation of f, in order.
func boundsOf(f InputValidationFailure) []string {
	bounds := make([]string, len(f.Violations))
	for i, v := range f.Violations {
		if v.Expected.Bound != nil {
			bounds[i] = v.Expected.Bound.Text
		}
	}
	return bounds
}

// The least violations are chosen by their complete schema locations, not the
// 256 bytes of each that are shown: past those, the branches differ only in
// their index, and in byte order 10 sorts before 2.
func TestProjectionOrdersByTheCompleteLocation(t *testing.T) {
	key := strings.Repeat("x", 260)
	branches := make([]string, 20)
	for i := range branches {
		branches[i] = `{"maximum":` + strconv.Itoa(i) + `}`
	}
	schema := `{"properties":{"` + key + `":{"allOf":[` + strings.Join(branches, ",") + `]}}}`
	f := projectFailure(invalidCall(t, schema, `{"`+key+`":100}`))
	assert.Equal(t, []string{"0", "1", "10", "11", "12", "13", "14", "15", "16", "17"}, boundsOf(f))
	assert.Equal(t, 10, f.Omitted)
	assertWithinBudgets(t, f)
	for _, v := range f.Violations {
		assert.Equal(t, "/properties/"+key[:244], v.SchemaLocation)
		assert.True(t, v.Cut)
	}
}

// Locations are compared whole, before any cut: one that does not fit is shown
// cut to fewer than 256 bytes when a character straddles the cut, which must
// not make it sort as a prefix of the other. The locations are compared as they are
// shown, percent-encoding undone: the euro sign's first byte, 0xE2, follows
// "a", where its %E2 in the URL would precede it.
func TestProjectionComparesLocationsAsShown(t *testing.T) {
	prefix := strings.Repeat("x", 256-len("/$defs/")-1)
	euro, plain := prefix+"€", prefix+"aa"
	schema := `{"$defs":{"` + euro + `":{"maximum":1},"` + plain + `":{"maximum":2}},` +
		`"properties":{"p":{"allOf":[{"$ref":"#/$defs/` + euro + `"},{"$ref":"#/$defs/` + plain + `"}]}}}`
	f := projectFailure(invalidCall(t, schema, `{"p":100}`))
	assert.Equal(t, []string{"2", "1"}, boundsOf(f))
	require.Len(t, f.Violations, 2)
	assert.Len(t, f.Violations[0].SchemaLocation, 256)
	assert.Len(t, f.Violations[1].SchemaLocation, 255, "the cut does not split the character")

	// A location that is a prefix of another sorts first, whichever is read
	// first.
	for _, branches := range []string{`{"$ref":"#/$defs/xy"},{"$ref":"#/$defs/x"}`, `{"$ref":"#/$defs/x"},{"$ref":"#/$defs/xy"}`} {
		f = projectFailure(invalidCall(t, `{"$defs":{"x":false,"xy":{"maximum":1}},"properties":{"p":{"allOf":[`+branches+`]}}}`, `{"p":2}`))
		require.Len(t, f.Violations, 2)
		assert.Equal(t, "/$defs/x", f.Violations[0].SchemaLocation, branches)
		assert.Equal(t, "/$defs/xy/maximum", f.Violations[1].SchemaLocation, branches)
	}

	// A keyword token is escaped as a pointer's token.
	f = projectFailure(invalidCall(t, `{"dependentRequired":{"a/b~c":["q"]}}`, `{"a/b~c":1}`))
	require.Len(t, f.Violations, 1)
	assert.Equal(t, "/dependentRequired/a~1b~0c", f.Violations[0].SchemaLocation)

	// A location of exactly 256 bytes is shown whole; one byte more is cut.
	for extra, cut := range []bool{false, true} {
		name := strings.Repeat("x", 256-len("/properties/")-len("/maximum")+extra)
		f := projectFailure(invalidCall(t, `{"properties":{"`+name+`":{"maximum":1}}}`, `{"`+name+`":2}`))
		require.Len(t, f.Violations, 1)
		assert.Len(t, f.Violations[0].SchemaLocation, 256)
		assert.Equal(t, cut, f.Violations[0].Cut)
	}
}

// exclusiveBranches is a schema of n allOf branches that each refuse 0, and
// the failure of arguments that are 0 against it: n failures at one pointer,
// each an exclusive bound.
func exclusiveBranches(t *testing.T, n int) *inputInvalid {
	t.Helper()
	branches := make([]string, n)
	for i := range branches {
		branches[i] = `{"exclusiveMinimum":` + strconv.Itoa(i) + `}`
	}
	return invalidCall(t, `{"properties":{"x":{"allOf":[`+strings.Join(branches, ",")+`]}}}`, `{"x":0}`)
}

// draft4ExclusiveBranches is exclusiveBranches written in draft 4.
func draft4ExclusiveBranches(t *testing.T, n int) *inputInvalid {
	t.Helper()
	branches := make([]string, n)
	for i := range branches {
		branches[i] = `{"minimum":` + strconv.Itoa(i) + `,"exclusiveMinimum":true}`
	}
	return invalidCall(t, `{`+draft4+`,"properties":{"x":{"allOf":[`+strings.Join(branches, ",")+`]}}}`, `{"x":0}`)
}

// The projection's work grows with the failures and the schema, not with their
// product: at four times both, it allocates about four times as much, where
// walking the schema once per exclusive bound allocates sixteen times as much.
func TestProjectionAllocationsGrowLinearly(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	for name, branches := range map[string]func(*testing.T, int) *inputInvalid{
		"draft 2020": exclusiveBranches,
		"draft 4":    draft4ExclusiveBranches,
	} {
		t.Run(name, func(t *testing.T) {
			allocs := func(n int) float64 {
				inv := branches(t, n)
				return testing.AllocsPerRun(5, func() { projectFailure(inv) })
			}
			small, large := allocs(100), allocs(400)
			assert.Less(t, large, 6*small, "allocations at 100 failures: %.0f, at 400: %.0f", small, large)
		})
	}
}

// A failing exclusive bound costs the same at any schema size: nothing per
// call walks the schema.
func TestProjectionExclusiveBoundCostsTheSameAtAnySchemaSize(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	allocs := func(properties int) float64 {
		filler := make([]string, properties)
		for i := range filler {
			filler[i] = `"p` + strconv.Itoa(i) + `":{"type":"string"}`
		}
		inv := invalidCall(t, `{"properties":{"n":{"exclusiveMaximum":5},`+strings.Join(filler, ",")+`}}`, `{"n":9}`)
		return testing.AllocsPerRun(5, func() { projectFailure(inv) })
	}
	assert.Equal(t, allocs(10), allocs(1000), "allocations at 10 properties, then at 1,000")
}

// An exclusive bound is held by maximum or minimum where its own schema is of
// draft 4, whatever the draft of the schema around it.
func TestProjectionExclusiveBoundFollowsItsSchemasDraft(t *testing.T) {
	const later = `"$schema":"https://json-schema.org/draft/2020-12/schema"`
	for _, tc := range []struct{ name, schema, location string }{
		{"draft 4", `{` + draft4 + `,"properties":{"x":{"minimum":0,"exclusiveMinimum":true}}}`, "/properties/x/minimum"},
		{"draft 2020", `{` + later + `,"properties":{"x":{"exclusiveMinimum":0}}}`, "/properties/x/exclusiveMinimum"},
		{"draft 2020 in a draft-4 root", `{` + draft4 + `,"definitions":{"s":{"id":"urn:s",` + later + `,"$id":"urn:s","exclusiveMinimum":0}},"properties":{"x":{"$ref":"urn:s"}}}`, "/definitions/s/exclusiveMinimum"},
		{"draft 4 in a draft-2020 root", `{` + later + `,"$defs":{"s":{"$id":"urn:s","id":"urn:s",` + draft4 + `,"minimum":0,"exclusiveMinimum":true}},"properties":{"x":{"$ref":"urn:s"}}}`, "/$defs/s/minimum"},
		{"draft 2020 beside draft 4 in a draft-2020 root", `{` + later + `,"$defs":{"s":{"$id":"urn:s","id":"urn:s",` + draft4 + `,"minimum":0,"exclusiveMinimum":true}},"properties":{"x":{"exclusiveMinimum":0},"y":{"$ref":"urn:s"}}}`, "/properties/x/exclusiveMinimum"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := projectFailure(invalidCall(t, tc.schema, `{"x":0}`))
			require.Len(t, f.Violations, 1)
			assert.Equal(t, "exclusiveMinimum", f.Violations[0].Keyword)
			assert.Equal(t, tc.location, f.Violations[0].SchemaLocation)
		})
	}
}

// The schemas are indexed by location only for a failure that asks, an
// exclusive bound's, of a schema that holds a draft-4 schema. A projection
// builds the index at most once (TestProjectionAllocationsGrowLinearly).
func TestProjectionIndexesTheSchemaOnDemand(t *testing.T) {
	for _, tc := range []struct {
		name       string
		inv        *inputInvalid
		violations int
		indexed    bool
	}{
		{"an exclusive bound without draft 4", exclusiveBranches(t, 12), 10, false},
		{"an exclusive bound in draft 4", draft4ExclusiveBranches(t, 12), 10, true},
		{"no other keyword", invalidCall(t, `{`+draft4+`,"properties":{"x":{"allOf":[{"minimum":1},{"maximum":-1}]}}}`, `{"x":0}`), 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &projection{inv: tc.inv}
			verr, ok := errors.AsType[*jsonschema.ValidationError](p.inv.cause)
			require.True(t, ok)
			require.Len(t, p.tree(verr).Violations, tc.violations)
			assert.Equal(t, tc.indexed, p.locations != nil)
		})
	}
}

// A failure's list is sized once, and each violation is written in place: ten
// violations cost the list once, not its growth and a copy of each.
func TestProjectionListIsSizedOnce(t *testing.T) {
	inv := invalidCall(t, `{"properties":{"a":{"items":{"type":"integer"}}}}`, `{"a":["x","x","x","x","x","x","x","x","x","x"]}`)
	f := projectFailure(inv)
	require.Len(t, f.Violations, 10)
	assert.Equal(t, 10, cap(f.Violations))
	if raceEnabled {
		return
	}
	measure := func() uint64 {
		before := allocatedBytes()
		projectFailure(inv)
		return allocatedBytes() - before
	}
	assert.Less(t, min(measure(), measure(), measure()), uint64(16<<10), "bytes of a failure of 10 violations")
}

// A failing call without a handler builds no violation list, only the
// default refusal: at most 48 allocations for one failure and 104 for three.
func TestInputValidationFailingCallAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation())
	srv.AddTool(mcp.NewToolWithRawSchema("checked", "", json.RawMessage(`{"type":"object","properties":{"a":{"type":"string"},"b":{"minimum":10},"c":{"enum":["x"]}}}`)),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})
	ctx := t.Context()
	for _, tc := range []struct {
		arguments string
		ceiling   float64
	}{
		{`{"a":1}`, 48},
		{`{"a":1,"b":5,"c":"y"}`, 104},
	} {
		msg := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"checked","arguments":` + tc.arguments + `}}`)
		out, err := json.Marshal(srv.HandleMessage(ctx, msg))
		require.NoError(t, err)
		require.Contains(t, string(out), `"isError":true`)
		allocs := testing.AllocsPerRun(1000, func() { srv.HandleMessage(ctx, msg) })
		assert.LessOrEqual(t, allocs, tc.ceiling, "allocations per failing tools/call with %s", tc.arguments)
	}
}

// Arguments that cannot be encoded are refused with the default text and the
// marshal error, in at most 17 allocations in validateArguments. Through a
// before-call hook that writes the NaN, the call takes at most 41, the
// round-trip mark's copy of the params among them (TestMarkRoundTripAllocations).
func TestInputValidationUnencodableCallAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	hooks := &Hooks{}
	hooks.AddBeforeCallTool(func(_ context.Context, _ any, r *mcp.CallToolRequest) {
		r.Params.Arguments = map[string]any{"x": math.NaN()}
	})
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation(), WithHooks(hooks))
	srv.AddTool(mcp.NewToolWithRawSchema("checked", "", json.RawMessage(`{"type":"object"}`)),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})
	ctx := t.Context()
	msg := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"checked","arguments":{"x":1}}}`)
	out, err := json.Marshal(srv.HandleMessage(ctx, msg))
	require.NoError(t, err)
	require.Contains(t, string(out), `"text":"invalid arguments: json: unsupported value: NaN"`)
	allocs := testing.AllocsPerRun(1000, func() { srv.HandleMessage(ctx, msg) })
	assert.LessOrEqual(t, allocs, 41.0, "allocations per tools/call with unencodable arguments")

	compiled, err := compileSchema("t", []byte(`{"type":"object"}`))
	require.NoError(t, err)
	args := callArguments{decoded: map[string]any{"x": math.NaN()}}
	inv := invalidArguments(t, compiled, args)
	assert.EqualError(t, inv, "invalid arguments: json: unsupported value: NaN")
	_, isMarshalError := errors.AsType[*json.UnsupportedValueError](inv)
	assert.True(t, isMarshalError, "the refusal wraps the marshal error")
	schema := &cachedToolSchema{compiled: compiled}
	allocs = testing.AllocsPerRun(1000, func() { _, _ = validateArguments(schema, args) })
	assert.LessOrEqual(t, allocs, 17.0, "allocations to refuse unencodable arguments")
}
