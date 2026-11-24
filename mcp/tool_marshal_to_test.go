package mcp

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// toolByMap is a Tool that encodes through MarshalJSON alone, the map it
// builds: what every encoder wrote before Tool had MarshalJSONTo.
type toolByMap Tool

func (t toolByMap) MarshalJSON() ([]byte, error) { return Tool(t).MarshalJSON() }

// schemaByMap is a ToolArgumentsSchema that encodes through MarshalJSON alone.
type schemaByMap ToolArgumentsSchema

func (s schemaByMap) MarshalJSON() ([]byte, error) { return ToolArgumentsSchema(s).MarshalJSON() }

// encodeIn encodes value with one of the encoders a tool is carried by:
// encoding/json's Marshal, a v1 Encoder that does not escape HTML, and
// encoding/json/v2 with its own defaults.
func encodeIn(encoder string, value any) ([]byte, error) {
	switch encoder {
	case "v1 unescaped":
		var out bytes.Buffer
		enc := json.NewEncoder(&out)
		enc.SetEscapeHTML(false)
		err := enc.Encode(value)
		return out.Bytes(), err
	case "v2":
		return jsonv2.Marshal(value)
	}
	return json.Marshal(value)
}

var draft04 = "http://json-schema.org/draft-04/schema#"

// toolSchemaShapes are schemas of every shape a tool's schemas take.
func toolSchemaShapes() map[string]ToolArgumentsSchema {
	return map[string]ToolArgumentsSchema{
		"zero":         {},
		"object":       {Type: "object"},
		"type array":   {Types: []string{"object", "null"}},
		"empty types":  {Types: []string{}},
		"type wins":    {Type: "object", Types: []string{"array"}},
		"empty fields": {Type: "object", Properties: map[string]any{}, Required: []string{}, Defs: map[string]any{}},
		"full": {
			Type:                 "object",
			Defs:                 map[string]any{"d": map[string]any{"type": "string", "pattern": "<a&b>"}},
			Properties:           map[string]any{"b": map[string]any{"type": "integer", "maximum": json.Number("9007199254740993")}, "a": map[string]any{"$ref": "#/$defs/d"}},
			Required:             []string{"b", "a"},
			AdditionalProperties: false,
			AdditionalFields: map[string]any{
				"$schema": "https://json-schema.org/draft/2020-12/schema", "title": "T <&>", "oneOf": []any{map[string]any{"required": []any{"a"}}},
				"type": "ignored", "properties": "ignored", "required": "ignored", "additionalProperties": "ignored", "$defs": "ignored",
				"\U0001F600": "x", "�": 1.5, "é": " ", "<k>": json.RawMessage(`{"z": 1, "a": [1, 2]}`), "": nil,
			},
		},
		"draft-04 no required": {Type: "object", AdditionalFields: map[string]any{"$schema": draft04}},
		"draft-04 required":    {Type: "object", Required: []string{"x"}, AdditionalFields: map[string]any{"$schema": draft04}},
		"ordered": {
			Type:          "object",
			Properties:    map[string]any{"z": 1, "y": 2, "x": 3, "w": 4, "<v>": 5},
			PropertyOrder: []string{"y", "missing", "z", "y", "<v>"},
		},
		"order of nothing": {Type: "object", PropertyOrder: []string{"a"}},
		"long order": func() ToolArgumentsSchema {
			s := ToolArgumentsSchema{Type: "object", Properties: map[string]any{}}
			for i := range 3 * orderedPropertiesSeen {
				name := strings.Repeat("p", i%7+1) + string(rune('a'+i%26)) + string(rune('A'+i/26))
				s.Properties[name] = i
				if i%3 != 0 {
					s.PropertyOrder = append(s.PropertyOrder, name, name)
				}
			}
			s.PropertyOrder = append(s.PropertyOrder, "missing")
			return s
		}(),
		"additional properties schema": {Type: "object", AdditionalProperties: map[string]any{"type": "string"}},
	}
}

// toolShapes are tools of every shape: raw and built input and output schemas,
// annotations, _meta, icons, execution and task support.
func toolShapes() map[string]Tool {
	tools := map[string]Tool{
		"plain": NewTool("plain"),
		"built": NewTool("built <&>", WithDescription("does <b>things</b> & more"), WithTitleAnnotation("T"),
			WithString("region", Required(), Description("the region")), WithNumber("n"),
			WithReadOnlyHintAnnotation(true), WithDestructiveHintAnnotation(false), WithIdempotentHintAnnotation(true),
			WithOpenWorldHintAnnotation(false)),
		"raw": NewToolWithRawSchema("raw", "raw <schema>",
			json.RawMessage(`{"type":"object", "properties":{"a":{"type":"string","description":"<x>"}}}`)),
		"everything": {
			Name: "meta", Title: "Title <t>", Description: "  \xff", DeferLoading: true,
			Meta:           &Meta{ProgressToken: "p", AdditionalFields: map[string]any{"k": "<v>", "a": 1}},
			RawInputSchema: json.RawMessage(`{"type": "object"}`), RawOutputSchema: json.RawMessage(` {"type":"object"} `),
			Icons:     []Icon{{Src: "https://x/i.png", MIMEType: "image/png", Sizes: []string{"48x48"}}},
			Execution: &ToolExecution{TaskSupport: TaskSupportOptional},
		},
		"task required":   {Name: "t", InputSchema: ToolInputSchema{Type: "object"}, Execution: &ToolExecution{TaskSupport: TaskSupportRequired}},
		"empty execution": {Name: "e", Execution: &ToolExecution{}},
		"empty icons":     {Name: "i", Icons: []Icon{}},
		"empty _meta":     {Name: "m", Meta: &Meta{}},
		"untyped output":  {Name: "o", InputSchema: ToolInputSchema{Type: "object"}, OutputSchema: ToolOutputSchema{Properties: map[string]any{"x": map[string]any{"type": "integer"}}}},
		"typed output":    {Name: "o", OutputSchema: ToolOutputSchema{Types: []string{"object"}, Required: []string{"x"}}},
	}
	for name, schema := range toolSchemaShapes() {
		tools["input "+name] = Tool{Name: name, InputSchema: ToolInputSchema(schema)}
		if !ToolOutputSchema(schema).IsZero() {
			tools["output "+name] = Tool{Name: name, OutputSchema: ToolOutputSchema(schema)}
		}
	}
	return tools
}

var toolEncoders = []string{"v1", "v1 unescaped", "v2"}

// A tool encodes as it did through MarshalJSON's map, in each encoder that
// carries it: alone, as a field of a struct and in a tools/list result,
// through MarshalJSONTo; by value, which a v1 encoder cannot address, through
// MarshalJSON. So does a schema.
func TestToolMarshalJSONToWritesWhatTheMapWrote(t *testing.T) {
	for name, tool := range toolShapes() {
		t.Run(name, func(t *testing.T) {
			for _, encoder := range toolEncoders {
				want, err := encodeIn(encoder, toolByMap(tool))
				require.NoError(t, err)
				got, err := encodeIn(encoder, &tool)
				require.NoError(t, err)
				assert.Equal(t, string(want), string(got), "inside %s", encoder)
				byValue, err := encodeIn(encoder, tool)
				require.NoError(t, err)
				assert.Equal(t, string(want), string(byValue), "by value, through MarshalJSON in v1, inside %s", encoder)
				wantField, err := encodeIn(encoder, struct{ T toolByMap }{toolByMap(tool)})
				require.NoError(t, err)
				gotField, err := encodeIn(encoder, &struct{ T Tool }{tool})
				require.NoError(t, err)
				assert.Equal(t, string(wantField), string(gotField), "a field of a struct inside %s", encoder)

				wantList, err := encodeIn(encoder, []toolByMap{toolByMap(tool), toolByMap(tool)})
				require.NoError(t, err)
				gotList, err := encodeIn(encoder, &ListToolsResult{Tools: []Tool{tool, tool}})
				require.NoError(t, err)
				assert.Contains(t, string(gotList), `"tools":`+strings.TrimSuffix(string(wantList), "\n"), "a list inside %s", encoder)
			}
		})
	}
	for name, schema := range toolSchemaShapes() {
		t.Run("schema "+name, func(t *testing.T) {
			for _, encoder := range toolEncoders {
				want, err := encodeIn(encoder, schemaByMap(schema))
				require.NoError(t, err)
				got, err := encodeIn(encoder, &schema)
				require.NoError(t, err)
				assert.Equal(t, string(want), string(got), "inside %s", encoder)
			}
		})
	}
}

// A tool with both forms of a schema is refused as MarshalJSON refuses it,
// with nothing of it written; so is a value that cannot be encoded.
func TestToolMarshalJSONToRefusesWhatTheMapRefused(t *testing.T) {
	for name, tool := range map[string]Tool{
		"both input schemas":   {Name: "c", InputSchema: ToolInputSchema{Type: "object"}, RawInputSchema: json.RawMessage(`{}`)},
		"both output schemas":  {Name: "c", OutputSchema: ToolOutputSchema{Type: "object"}, RawOutputSchema: json.RawMessage(`{}`)},
		"invalid raw schema":   {Name: "c", RawInputSchema: json.RawMessage(`{`)},
		"empty raw schema":     {Name: "c", RawInputSchema: json.RawMessage{}},
		"an unencodable _meta": {Name: "c", Meta: &Meta{AdditionalFields: map[string]any{"f": func() {}}}},
		"an unencodable property": {Name: "c", InputSchema: ToolInputSchema{Type: "object",
			Properties: map[string]any{"x": func() {}}, PropertyOrder: []string{"x"}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, wantErr := tool.MarshalJSON()
			require.Error(t, wantErr)
			for _, encoder := range toolEncoders {
				out, err := encodeIn(encoder, &tool)
				require.Error(t, err, "inside %s", encoder)
				assert.Empty(t, out, "inside %s", encoder)
				if errors.Is(wantErr, errToolSchemaConflict) {
					assert.ErrorIs(t, err, errToolSchemaConflict, "inside %s", encoder)
				}
			}
		})
	}
}

// Keys and values of any spelling are written as the map wrote them, or
// refused by both.
func FuzzToolSchemaMarshalJSONTo(f *testing.F) {
	f.Add("a", "b", "<", "v", 1.5, true)
	f.Add("\U0001F600", "�", "type", " ", 1e21, false)
	f.Add("", "\xff", "$schema", draft04, 0.0, true)
	f.Fuzz(func(t *testing.T, k1, k2, k3, s string, n float64, ordered bool) {
		schema := ToolArgumentsSchema{
			Type:             s,
			Properties:       map[string]any{k1: s, k2: n},
			AdditionalFields: map[string]any{k3: s, k1: map[string]any{s: []any{k2, n}}},
		}
		if ordered {
			schema.PropertyOrder = []string{k2, k3, k2}
		}
		tool := Tool{Name: s, Description: k1, InputSchema: ToolInputSchema(schema), OutputSchema: ToolOutputSchema(schema)}
		for _, encoder := range toolEncoders {
			want, wantErr := encodeIn(encoder, toolByMap(tool))
			got, gotErr := encodeIn(encoder, &tool)
			if (wantErr != nil) != (gotErr != nil) {
				t.Fatalf("%s: errors differ: map %v, new %v", encoder, wantErr, gotErr)
			}
			if wantErr == nil && !bytes.Equal(want, got) {
				t.Fatalf("%s: bytes differ:\n map %q\n new %q", encoder, want, got)
			}
		}
	})
}

// listOfTools is a tools/list result of n tools, each with a small schema,
// raw or built.
func listOfTools(n int, built bool) *ListToolsResult {
	tools := make([]Tool, n)
	for i := range tools {
		name := "tool_" + strings.Repeat("x", i%3) + string(rune('a'+i%26))
		tools[i] = NewToolWithRawSchema(name, "a tool that does a thing with a region",
			json.RawMessage(`{"type":"object","properties":{"region":{"type":"string","description":"the region"},"n":{"type":"integer"}},"required":["region"]}`))
		if built {
			tools[i] = NewTool(name, WithDescription("a tool that does a thing with a region"),
				WithString("region", Required(), Description("the region")), WithNumber("n"))
		}
	}
	return &ListToolsResult{Tools: tools}
}

// A tools/list result is written straight into the encoder: no map per tool
// or per schema, and no copy of either's bytes. 1,011 allocations for 50 raw
// schemas and 1,761 for 50 built ones through MarshalJSON's maps; none and
// 350, what encoding a built schema's property maps costs, written straight.
func TestToolsListMarshalAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	for _, tc := range []struct {
		name  string
		built bool
		max   float64
	}{
		{"50 raw schemas", false, 0},
		{"50 built schemas", true, 350},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := listOfTools(50, tc.built)
			var out bytes.Buffer
			enc := json.NewEncoder(&out)
			allocs := testing.AllocsPerRun(100, func() {
				out.Reset()
				_ = enc.Encode(result)
			})
			assert.LessOrEqual(t, allocs, tc.max, "allocations per tools/list result encoded")
		})
	}
}

// filled sets v to a value with every exported field, element and entry set,
// down to depth: a field Tool or ToolArgumentsSchema gains is set too.
func filled(v reflect.Value, depth int) {
	if depth == 0 {
		return
	}
	if v.Type() == reflect.TypeFor[json.RawMessage]() {
		v.SetBytes([]byte(`{"type":"object","x":[1,"<y>"]}`))
		return
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString("s<&>")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(7)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1.5)
	case reflect.Interface:
		v.Set(reflect.ValueOf(any("v<&>")))
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		filled(v.Elem(), depth-1)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		filled(v.Index(0), depth-1)
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		key := reflect.New(v.Type().Key()).Elem()
		filled(key, depth-1)
		elem := reflect.New(v.Type().Elem()).Elem()
		filled(elem, depth-1)
		v.SetMapIndex(key, elem)
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				filled(v.Field(i), depth-1)
			}
		}
	}
}

// Every field of a tool and of a schema, set alone and all together, is
// written by MarshalJSONTo as MarshalJSON writes it: a field either gains is
// set here without a test to name it, so one MarshalJSON writes and
// MarshalJSONTo does not fails here. When it does, update MarshalJSONTo and
// the shapes of TestToolMarshalJSONToWritesWhatTheMapWrote.
func TestToolMarshalJSONToWritesEveryField(t *testing.T) {
	compare := func(t *testing.T, name string, want, got any) {
		t.Helper()
		for _, encoder := range toolEncoders {
			wantBytes, wantErr := encodeIn(encoder, want)
			gotBytes, gotErr := encodeIn(encoder, got)
			require.Equal(t, wantErr != nil, gotErr != nil, "%s inside %s: errors %v and %v", name, encoder, wantErr, gotErr)
			assert.Equal(t, string(wantBytes), string(gotBytes), "%s inside %s", name, encoder)
		}
	}

	toolType := reflect.TypeFor[Tool]()
	for i := range toolType.NumField() {
		field := toolType.Field(i)
		tool := Tool{Name: "t"}
		filled(reflect.ValueOf(&tool).Elem().Field(i), 6)
		compare(t, "Tool."+field.Name, toolByMap(tool), &tool)
	}
	var all Tool
	filled(reflect.ValueOf(&all).Elem(), 8)
	// Each schema in one of its two forms: both set is a conflict.
	structured, raw := all, all
	structured.RawInputSchema, structured.RawOutputSchema = nil, nil
	raw.InputSchema, raw.OutputSchema = ToolInputSchema{}, ToolOutputSchema{}
	compare(t, "every Tool field, structured schemas", toolByMap(structured), &structured)
	compare(t, "every Tool field, raw schemas", toolByMap(raw), &raw)
	compare(t, "every Tool field, both schemas", toolByMap(all), &all)

	schemaType := reflect.TypeFor[ToolArgumentsSchema]()
	for i := range schemaType.NumField() {
		field := schemaType.Field(i)
		var schema ToolArgumentsSchema
		filled(reflect.ValueOf(&schema).Elem().Field(i), 6)
		compare(t, "ToolArgumentsSchema."+field.Name, schemaByMap(schema), &schema)
		tool := Tool{Name: "t", InputSchema: ToolInputSchema(schema), OutputSchema: ToolOutputSchema(schema)}
		compare(t, "a Tool's schemas with "+field.Name, toolByMap(tool), &tool)
	}
	var schema ToolArgumentsSchema
	filled(reflect.ValueOf(&schema).Elem(), 8)
	compare(t, "every ToolArgumentsSchema field", schemaByMap(schema), &schema)
}
