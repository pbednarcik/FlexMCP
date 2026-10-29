package server

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pointerRuns is how often each shape is validated in one process: the
// library's own location for a propertyNames node changes from run to run.
const pointerRuns = 200

// propertyNamesPointers is the pointer of each of f's propertyNames entries,
// in list order, "?" for an unknown one.
func propertyNamesPointers(f InputValidationFailure) []string {
	var pointers []string
	for _, v := range f.Violations {
		if !strings.Contains(v.SchemaLocation, "/propertyNames") {
			continue
		}
		if v.PointerUnknown {
			pointers = append(pointers, "?")
			continue
		}
		pointers = append(pointers, v.Pointer)
	}
	return pointers
}

// A propertyNames entry is placed at the object whose name failed, from the
// nearest anchor on its chain, or its pointer is unknown; never the library's
// own location, which a later sibling overwrites.
func TestProjectionPropertyNamesPointer(t *testing.T) {
	for _, tc := range []struct {
		name, schema, args string
		want               []string
	}{
		{"root", `{"propertyNames":{"minLength":5}}`, `{"bad":1}`, []string{""}},
		{"under properties, one failure", `{"properties":{"a":{"propertyNames":{"minLength":5}}}}`, `{"a":{"bad":1}}`, []string{"/a"}},
		{"under properties, two failures", `{"properties":{"a":{"propertyNames":{"minLength":5}}}}`, `{"a":{"bad":1,"bae":1}}`, []string{"/a", "/a"}},
		{"a $ref to a sibling", `{"properties":{"a":{"propertyNames":{"minLength":5}},"b":{"$ref":"#/properties/a"}}}`, `{"a":{"bad":1},"b":{"bad":1}}`, []string{"/a", "/b"}},
		{"a recursive $ref to the root", `{"propertyNames":{"maxLength":3},"additionalProperties":{"$ref":"#"}}`, `{"efgh":{"efgh":{}}}`, []string{"", "/efgh"}},
		{"a $ref into $defs", `{"properties":{"a":{"$ref":"#/$defs/o"}},"$defs":{"o":{"propertyNames":{"minLength":5}}}}`, `{"a":{"bad":1}}`, []string{"/a"}},
		{"a root anyOf", `{"anyOf":[{"propertyNames":{"minLength":5}},{"type":"string"}]}`, `{"bad":1}`, []string{""}},
		{"two $refs to one definition", `{"properties":{"a":{"$ref":"#/$defs/d"},"b":{"$ref":"#/$defs/d"}},"$defs":{"d":{"propertyNames":{"pattern":"^[a-z]+$"}}}}`, `{"a":{"Bad":1},"b":{"ok":1}}`, []string{"/a"}},
		{"items, which has no anchor", `{"items":{"propertyNames":{"enum":["ok"]}}}`, `[{"bad":1},{"ok":1}]`, []string{"?"}},
		{"items beside required, from the group", `{"items":{"propertyNames":{"enum":["ok"]},"required":["q"]}}`, `[{"bad":1},{"ok":1}]`, []string{"/0"}},
		{
			"a $ref into a definition that refers to its ancestor",
			`{"properties":{"top":{"$ref":"#/$defs/outer/properties/a"}},"$defs":{"outer":{"properties":{"a":{"propertyNames":{"enum":["b"]},"properties":{"b":{"$ref":"#/$defs/outer"}}}}}}}`,
			`{"top":{"b":{"a":{"zz":1}}}}`, []string{"/top/b/a"},
		},
		{
			"the same name failing at both levels",
			`{"properties":{"top":{"$ref":"#/$defs/outer/properties/a"}},"$defs":{"outer":{"properties":{"a":{"propertyNames":{"enum":["b"]},"properties":{"b":{"$ref":"#/$defs/outer"}}}}}}}`,
			`{"top":{"zz":1,"b":{"a":{"zz":1}}}}`, []string{"/top", "/top/b/a"},
		},
		{
			"a group kept by a root sibling",
			`{"required":["q"],"properties":{"a":{"propertyNames":{"enum":["b"]},"properties":{"b":{"$ref":"#"}}}}}`,
			`{"a":{"xx":1,"b":{"q":1,"a":{"zz":1}}}}`, []string{"/a", "/a/b/a"},
		},
		{
			"a group kept by a root sibling, the same name",
			`{"required":["q"],"properties":{"a":{"propertyNames":{"enum":["b"]},"properties":{"b":{"$ref":"#"}}}}}`,
			`{"a":{"zz":1,"b":{"q":1,"a":{"zz":1}}}}`, []string{"/a", "/a/b/a"},
		},
		{
			"a recursive anyOf",
			`{"anyOf":[{"propertyNames":{"enum":["b"]},"properties":{"b":{"$ref":"#"}}},{"type":"string"}]}`,
			`{"zz":1,"b":{"zz":1}}`, []string{"", "/b"},
		},
		{"allOf", `{"allOf":[{"properties":{"a":{"propertyNames":{"minLength":5}}}}]}`, `{"a":{"bad":1}}`, []string{"/a"}},
		{
			"names holding a slash, a tilde, a percent sign and an accent, through a $ref",
			`{"properties":{"a b%":{"$ref":"#/$defs/d"}},"$defs":{"d":{"properties":{"é#/~":{"propertyNames":{"minLength":5}}}}}}`,
			`{"a b%":{"é#/~":{"bad":1}}}`, []string{"/a b%/é#~1~0"},
		},
		{
			"a $ref into items, recursing through kids",
			`{"properties":{"l":{"items":{"$ref":"#/$defs/n"}}},"$defs":{"n":{"propertyNames":{"enum":["kids"]},"properties":{"kids":{"$ref":"#/properties/l"}}}}}`,
			`{"l":[{"bad":1,"kids":[{"zz":1}]},{"kids":[]}]}`, []string{"/l/0", "/l/0/kids/0"},
		},
		{
			"oneOf branches at different depths",
			`{"oneOf":[{"properties":{"a":{"propertyNames":{"minLength":5}}}},{"properties":{"a":{"properties":{"b":{"propertyNames":{"minLength":5}}}}}}]}`,
			`{"a":{"bad":1,"b":{"bad":1}}}`, []string{"/a", "/a", "/a/b"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for run := range pointerRuns {
				got := propertyNamesPointers(projectFailure(invalidCall(t, tc.schema, tc.args)))
				require.Equal(t, tc.want, got, "run %d", run)
			}
		})
	}
}

// A propertyNames entry keeps its leaf's keyword and row, and reads the name
// as the instance the leaf validated; its message names the root, as the
// default text does.
func TestProjectionPropertyNamesEntry(t *testing.T) {
	f := projectFailure(invalidCall(t, `{"properties":{"a":{"propertyNames":{"maxLength":2}}}}`, `{"a":{"bad":1}}`))
	assert.Equal(t, one(InputViolation{
		Pointer: "/a", Keyword: "maxLength", SchemaLocation: "/properties/a/propertyNames/maxLength", Property: "bad",
		Expected: ViolationExpected{Limit: new(2)}, Actual: ViolationActual{Kind: "string", Value: `"bad"`, Count: new(3)},
		Message: "<root>: maxLength: got 3, want 2",
	}), f)
}

// A member that unevaluatedProperties' own false refuses is reported as
// unevaluatedProperties, at the member and naming it; a leaf of a subschema
// under the keyword keeps its own row, and a property named
// unevaluatedProperties is a name, not the keyword.
func TestProjectionUnevaluatedProperties(t *testing.T) {
	for _, tc := range []struct {
		name, schema, args string
		want               InputValidationFailure
	}{
		{
			"false, one entry per member", `{"properties":{"a":{}},"unevaluatedProperties":false}`, `{"a":1,"y":true,"z":"s"}`,
			InputValidationFailure{Violations: []InputViolation{
				{Pointer: "/y", Keyword: "unevaluatedProperties", SchemaLocation: "/unevaluatedProperties", Property: "y",
					Actual: ViolationActual{Kind: "boolean", Value: "true"}, Message: "/y: false schema"},
				{Pointer: "/z", Keyword: "unevaluatedProperties", SchemaLocation: "/unevaluatedProperties", Property: "z",
					Actual: ViolationActual{Kind: "string", Value: `"s"`}, Message: "/z: false schema"},
			}},
		},
		{
			"a subschema keeps its leaf's row", `{"unevaluatedProperties":{"type":"string"}}`, `{"n":1}`,
			one(InputViolation{Pointer: "/n", Keyword: "type", SchemaLocation: "/unevaluatedProperties/type",
				Expected: ViolationExpected{Types: []string{"string"}}, Actual: ViolationActual{Kind: "number", Value: "1"},
				Message: "/n: got number, want string"}),
		},
		{
			"a false in a subschema stays false", `{"unevaluatedProperties":{"properties":{"x":false}}}`, `{"m":{"x":1}}`,
			one(InputViolation{Pointer: "/m/x", Keyword: "false", SchemaLocation: "/unevaluatedProperties/properties/x",
				Actual: ViolationActual{Kind: "number", Value: "1"}, Message: "/m/x: false schema"}),
		},
		{
			"dependentRequired in a subschema keeps its property", `{"unevaluatedProperties":{"dependentRequired":{"a":["b"]}}}`, `{"m":{"a":1}}`,
			one(InputViolation{Pointer: "/m", Keyword: "dependentRequired", SchemaLocation: "/unevaluatedProperties/dependentRequired/a", Property: "a",
				Expected: ViolationExpected{Names: []string{"b"}}, Actual: ViolationActual{Kind: "object"},
				Message: "/m: properties 'b' required, if 'a' exists"}),
		},
		{
			"a property named unevaluatedProperties is a plain false", `{"properties":{"unevaluatedProperties":false}}`, `{"unevaluatedProperties":1}`,
			one(InputViolation{Pointer: "/unevaluatedProperties", Keyword: "false", SchemaLocation: "/properties/unevaluatedProperties",
				Actual: ViolationActual{Kind: "number", Value: "1"}, Message: "/unevaluatedProperties: false schema"}),
		},
		{
			"a property named properties holding the keyword",
			`{"properties":{"properties":{"type":"object","unevaluatedProperties":false}}}`, `{"properties":{"x":1}}`,
			one(InputViolation{Pointer: "/properties/x", Keyword: "unevaluatedProperties",
				SchemaLocation: "/properties/properties/unevaluatedProperties", Property: "x",
				Actual: ViolationActual{Kind: "number", Value: "1"}, Message: "/properties/x: false schema"}),
		},
		{
			"a dependency named unevaluatedProperties is a plain false",
			`{"properties":{"a":{"dependencies":{"unevaluatedProperties":false}}}}`, `{"a":{"unevaluatedProperties":1}}`,
			one(InputViolation{Pointer: "/a", Keyword: "false", SchemaLocation: "/properties/a/dependencies/unevaluatedProperties",
				Actual: ViolationActual{Kind: "object"}, Message: "/a: false schema"}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for run := range pointerRuns {
				require.Equal(t, tc.want, projectFailure(invalidCall(t, tc.schema, tc.args)), "run %d", run)
			}
		})
	}
}

// A false reached by a reference to it is a plain false, wherever it sits: it
// validates the referring schema's value, and names no member. A reference to
// the schema that holds unevaluatedProperties leaves the keyword its own row.
func TestProjectionReferencedFalse(t *testing.T) {
	deny := `{"$defs":{"deny":{"unevaluatedProperties":false}},`
	falseRow := func(pointer, location string) InputValidationFailure {
		return one(InputViolation{Pointer: pointer, Keyword: "false", SchemaLocation: location,
			Actual: ViolationActual{Kind: "object"}, Message: pointer + ": false schema"})
	}
	unevaluated := func(pointer, location, property string) InputValidationFailure {
		return one(InputViolation{Pointer: pointer, Keyword: "unevaluatedProperties", SchemaLocation: location, Property: property,
			Actual: ViolationActual{Kind: "number", Value: "1"}, Message: pointer + ": false schema"})
	}
	for _, tc := range []struct {
		name, schema string
		want         InputValidationFailure
	}{
		{"a $ref to the keyword's false", deny + `"properties":{"payload":{"$ref":"#/$defs/deny/unevaluatedProperties"}}}`,
			falseRow("/payload", "/$defs/deny/unevaluatedProperties")},
		{"a $dynamicRef to the keyword's false", deny + `"properties":{"payload":{"$dynamicRef":"#/$defs/deny/unevaluatedProperties"}}}`,
			falseRow("/payload", "/$defs/deny/unevaluatedProperties")},
		{"a $ref through a $ref", `{"$defs":{"hop":{"$ref":"#/$defs/deny/unevaluatedProperties"},"deny":{"unevaluatedProperties":false}},"properties":{"payload":{"$ref":"#/$defs/hop"}}}`,
			falseRow("/payload", "/$defs/deny/unevaluatedProperties")},
		{"a $ref in an allOf", deny + `"properties":{"payload":{"allOf":[{"$ref":"#/$defs/deny/unevaluatedProperties"}]}}}`,
			falseRow("/payload", "/$defs/deny/unevaluatedProperties")},
		{"a $ref to the schema holding the keyword", deny + `"properties":{"payload":{"$ref":"#/$defs/deny"}}}`,
			unevaluated("/payload/x", "/$defs/deny/unevaluatedProperties", "x")},
		{"the keyword in place", `{"properties":{"payload":{"unevaluatedProperties":false}}}`,
			unevaluated("/payload/x", "/properties/payload/unevaluatedProperties", "x")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, projectFailure(invalidCall(t, tc.schema, `{"payload":{"x":1}}`)))
		})
	}
}

// A location is unevaluatedProperties' own subschema when its last token is
// the keyword in a keyword's place; a token is a keyword's place unless the
// keyword before it maps names or indexes to schemas.
func TestUnderUnevaluated(t *testing.T) {
	for _, tc := range []struct {
		location []string
		want     bool
	}{
		{nil, false},
		{[]string{"unevaluatedProperties"}, true},
		{[]string{"unevaluatedProperties", "type"}, false},
		{[]string{"unevaluatedProperties", "properties", "x"}, false},
		{[]string{"unevaluatedProperties", "properties", "unevaluatedProperties"}, false},
		{[]string{"properties", "unevaluatedProperties"}, false},
		{[]string{"properties", "properties", "unevaluatedProperties"}, true},
		{[]string{"patternProperties", "unevaluatedProperties"}, false},
		{[]string{"$defs", "unevaluatedProperties", "type"}, false},
		{[]string{"definitions", "d", "unevaluatedProperties"}, true},
		{[]string{"dependentSchemas", "unevaluatedProperties"}, false},
		{[]string{"dependencies", "unevaluatedProperties"}, false},
		{[]string{"allOf", "0", "unevaluatedProperties"}, true},
		{[]string{"anyOf", "1", "not", "unevaluatedProperties"}, true},
		{[]string{"prefixItems", "2", "unevaluatedProperties"}, true},
		{[]string{"items", "0", "unevaluatedProperties"}, true},
		{[]string{"items", "unevaluatedProperties"}, true},
		{[]string{"properties", "a", "propertyNames"}, false},
	} {
		assert.Equal(t, tc.want, underUnevaluated(tc.location), "%q", tc.location)
	}
}

// An entry whose pointer is unknown sorts after every known one, though the
// object it is about sorts before them.
func TestProjectionUnknownPointerSortsLast(t *testing.T) {
	schema := `{"required":["q"],"properties":{"l":{"items":{"propertyNames":{"maxLength":2}}},"z":{"type":"string"}}}`
	for run := range pointerRuns {
		f := projectFailure(invalidCall(t, schema, `{"l":[{"abc":1}],"z":1}`))
		type entry struct {
			pointer string
			unknown bool
			keyword string
		}
		got := make([]entry, len(f.Violations))
		for i, v := range f.Violations {
			got[i] = entry{v.Pointer, v.PointerUnknown, v.Keyword}
		}
		require.Equal(t, []entry{{"", false, "required"}, {"/z", false, "type"}, {"", true, "maxLength"}}, got, "run %d", run)
	}
}
