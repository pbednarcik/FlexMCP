package mcp

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
	"slices"
	"strings"
)

// ToolArgumentsSchema represents a JSON Schema for tool arguments.
type ToolArgumentsSchema struct {
	// Defs holds the $defs keyword. Decoding keeps numbers exact, as
	// json.Number.
	Defs map[string]any `json:"$defs,omitempty"`
	Type string         `json:"type"`
	// Types holds an array-valued JSON Schema type, including a one-element
	// array. Type stays a string for compatibility and wins when it is not
	// empty. Decoding a string type clears Types; decoding an array clears Type.
	Types []string `json:"-"`
	// Properties holds the properties keyword. Decoding keeps numbers exact,
	// as json.Number.
	Properties map[string]any `json:"properties"`
	Required   []string       `json:"required,omitempty"`
	// AdditionalProperties holds the additionalProperties keyword. Decoding
	// keeps numbers exact, as json.Number.
	AdditionalProperties any `json:"additionalProperties,omitempty"`
	// AdditionalFields holds the top-level JSON Schema keywords the other
	// fields do not model, such as $schema, title or oneOf. The modeled names
	// (type, properties, required, additionalProperties and $defs) are
	// reserved: an entry under one of them is never written, even when its
	// field is empty. definitions is kept here, as written, unless decoding
	// moved it to Defs. Decoding keeps numbers exact, as json.Number.
	AdditionalFields map[string]any `json:"-"`
	// PropertyOrder is the marshal order of the top-level Properties keys.
	// UnmarshalJSON sets it from the "properties" object. Names that are not
	// in Properties are skipped, and keys that are not listed follow in
	// sorted order. A nil PropertyOrder marshals all keys in sorted order.
	PropertyOrder []string `json:"-"`
}

// ToolInputSchema remains a named type for retro-compatibility, so its JSON
// methods explicitly forward to ToolArgumentsSchema.
type ToolInputSchema ToolArgumentsSchema

// ToolOutputSchema is a tool's outputSchema. In a Tool, one with any keyword
// set and no type is an object schema, the type the specification requires of
// an outputSchema: tools/list writes it with "type":"object", and output
// validation checks against that. The zero ToolOutputSchema is no schema: see
// IsZero. Encoded on its own, it is written as ToolArgumentsSchema is.
type ToolOutputSchema ToolArgumentsSchema

// MarshalJSON implements the json.Marshaler interface for ToolInputSchema.
func (tis ToolInputSchema) MarshalJSON() ([]byte, error) {
	return ToolArgumentsSchema(tis).MarshalJSON()
}

// MarshalJSON implements the json.Marshaler interface for ToolOutputSchema.
func (tis ToolOutputSchema) MarshalJSON() ([]byte, error) {
	return ToolArgumentsSchema(tis).MarshalJSON()
}

// AsObjectSchema returns the schema as a Tool lists it: with "type":"object"
// when it has no type, an empty type array included, as is otherwise.
func (tis ToolOutputSchema) AsObjectSchema() ToolOutputSchema {
	if tis.Type == "" && len(tis.Types) == 0 {
		tis.Type = "object"
	}
	return tis
}

// IsZero reports whether the schema sets no keyword: no type, $defs,
// properties, required, additionalProperties or kept keyword. A tool whose
// OutputSchema is zero has no structured output schema: tools/list leaves it
// out and output validation does not run, and RawOutputSchema may be set
// beside it. PropertyOrder alone sets no keyword.
func (tis ToolOutputSchema) IsZero() bool {
	return tis.Type == "" && tis.Types == nil && tis.Defs == nil && tis.Properties == nil &&
		tis.Required == nil && tis.AdditionalProperties == nil && len(tis.AdditionalFields) == 0
}

// MarshalJSON implements the json.Marshaler interface for ToolArgumentsSchema.
func (tis ToolArgumentsSchema) MarshalJSON() ([]byte, error) {
	return toolArgumentsSchemaMarshalJSON(tis)
}

// MarshalJSONTo writes the schema into enc as MarshalJSON writes it. When enc
// encodes as json.Marshal does, each keyword is written straight in, without
// the map MarshalJSON builds, its boxed values and the copy of the encoded
// schema; otherwise MarshalJSON's bytes are written.
func (tis *ToolArgumentsSchema) MarshalJSONTo(enc *jsontext.Encoder) error {
	if !encodesAsMarshal(enc) {
		encoded, err := tis.MarshalJSON()
		if err != nil {
			return err
		}
		return enc.WriteValue(encoded)
	}
	return writeToolSchema(enc, tis)
}

// UnmarshalJSON implements the json.Unmarshaler interface for ToolInputSchema.
func (tis *ToolInputSchema) UnmarshalJSON(data []byte) error {
	return (*ToolArgumentsSchema)(tis).UnmarshalJSON(data)
}

// UnmarshalJSON implements the json.Unmarshaler interface for ToolOutputSchema.
func (tis *ToolOutputSchema) UnmarshalJSON(data []byte) error {
	return (*ToolArgumentsSchema)(tis).UnmarshalJSON(data)
}

// UnmarshalJSON implements the json.Unmarshaler interface for ToolArgumentsSchema.
func (tis *ToolArgumentsSchema) UnmarshalJSON(data []byte) error {
	return toolArgumentsSchemaUnmarshalJSON(data, tis)
}

// isModeledToolSchemaKeyword reports whether key is reserved for a field of
// ToolArgumentsSchema.
func isModeledToolSchemaKeyword(key string) bool {
	switch key {
	case "type", "properties", "required", "additionalProperties", "$defs":
		return true
	}
	return false
}

// toolArgumentsSchemaMarshalJSON handles the fields stored in ToolArgumentsSchema when json.Marshaler is called
func toolArgumentsSchemaMarshalJSON(tis ToolArgumentsSchema) ([]byte, error) {
	m := make(map[string]any, len(tis.AdditionalFields)+6)
	for key, value := range tis.AdditionalFields {
		if !isModeledToolSchemaKeyword(key) {
			m[key] = value
		}
	}
	m["type"] = tis.Type
	if tis.Type == "" && tis.Types != nil {
		m["type"] = tis.Types
	}

	if tis.Defs != nil {
		m["$defs"] = tis.Defs
	}

	// Marshal Properties to '{}' rather than `nil` when its length equals zero
	switch {
	case tis.PropertyOrder != nil:
		properties, err := marshalOrderedProperties(tis.Properties, tis.PropertyOrder)
		if err != nil {
			return nil, err
		}
		m["properties"] = properties
	case tis.Properties != nil:
		m["properties"] = tis.Properties
	default:
		m["properties"] = map[string]any{}
	}

	// Marshal Required to '[]' rather than `nil` when its length equals zero,
	// except in draft-04, where required is never empty.
	switch {
	case len(tis.Required) > 0:
		m["required"] = tis.Required
	case schemaDialectOf(tis.AdditionalFields["$schema"]) != dialectDraft04:
		m["required"] = []string{}
	}

	if tis.AdditionalProperties != nil {
		m["additionalProperties"] = tis.AdditionalProperties
	}

	return json.Marshal(m)
}

// writeToolSchema writes tis into enc, which encodes as json.Marshal does, as
// toolArgumentsSchemaMarshalJSON writes it: the kept keywords and the modeled
// ones together, in the order json.Marshal gives a map's keys.
func writeToolSchema(enc *jsontext.Encoder, tis *ToolArgumentsSchema) error {
	// Most schemas keep no keyword of their own: the modeled ones fit on the
	// stack.
	var fixed [8]string
	keys := fixed[:0]
	for key := range tis.AdditionalFields {
		if !isModeledToolSchemaKeyword(key) {
			keys = append(keys, key)
		}
	}
	keys = append(keys, "type", "properties")
	if tis.Defs != nil {
		keys = append(keys, "$defs")
	}
	if len(tis.Required) > 0 || schemaDialectOf(tis.AdditionalFields["$schema"]) != dialectDraft04 {
		keys = append(keys, "required")
	}
	if tis.AdditionalProperties != nil {
		keys = append(keys, "additionalProperties")
	}
	slices.Sort(keys)

	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	for _, key := range keys {
		if err := writeToolSchemaKeyword(enc, tis, key); err != nil {
			return err
		}
	}
	return enc.WriteToken(jsontext.EndObject)
}

// writeToolSchemaKeyword writes the member key of the schema tis, as
// toolArgumentsSchemaMarshalJSON's map holds it.
func writeToolSchemaKeyword(enc *jsontext.Encoder, tis *ToolArgumentsSchema, key string) error {
	if !isModeledToolSchemaKeyword(key) {
		if err := writeMetaName(enc, key); err != nil {
			return err
		}
		return jsonv2.MarshalEncode(enc, tis.AdditionalFields[key])
	}
	if err := enc.WriteToken(jsontext.String(key)); err != nil {
		return err
	}
	switch key {
	case "type":
		if tis.Type == "" && tis.Types != nil {
			return jsonv2.MarshalEncode(enc, &tis.Types)
		}
		return enc.WriteToken(jsontext.String(tis.Type))
	case "$defs":
		return jsonv2.MarshalEncode(enc, tis.Defs)
	case "properties":
		// Written as '{}' rather than null when there are none.
		switch {
		case tis.PropertyOrder != nil:
			return writeOrderedProperties(enc, tis.Properties, tis.PropertyOrder)
		case tis.Properties != nil:
			return jsonv2.MarshalEncode(enc, tis.Properties)
		}
		if err := enc.WriteToken(jsontext.BeginObject); err != nil {
			return err
		}
		return enc.WriteToken(jsontext.EndObject)
	case "required":
		// Written as '[]' rather than null when there are none.
		if len(tis.Required) > 0 {
			return jsonv2.MarshalEncode(enc, &tis.Required)
		}
		if err := enc.WriteToken(jsontext.BeginArray); err != nil {
			return err
		}
		return enc.WriteToken(jsontext.EndArray)
	}
	return jsonv2.MarshalEncode(enc, tis.AdditionalProperties)
}

// orderedPropertiesSeen is the most names writeOrderedProperties looks
// through one by one; a longer order is looked up in a set.
const orderedPropertiesSeen = 32

// writeOrderedProperties writes properties into enc as
// marshalOrderedProperties encodes them: the names order lists first, those
// not in properties and the repeats skipped, then the rest in sorted order.
func writeOrderedProperties(enc *jsontext.Encoder, properties map[string]any, order []string) error {
	// The names written are looked through one by one while order is
	// short, on the stack; a longer order keeps them in a set.
	var fixed [orderedPropertiesSeen]string
	written := fixed[:0]
	var writtenSet map[string]bool
	if len(order) > orderedPropertiesSeen {
		writtenSet = make(map[string]bool, len(order))
	}

	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	count := 0
	for _, name := range order {
		value, ok := properties[name]
		if !ok || writtenSet[name] || (writtenSet == nil && slices.Contains(written, name)) {
			continue
		}
		if err := writeProperty(enc, name, value); err != nil {
			return err
		}
		if writtenSet != nil {
			writtenSet[name] = true
		} else {
			written = append(written, name)
		}
		count++
	}
	if count < len(properties) {
		rest := make([]string, 0, len(properties)-count)
		for name := range properties {
			if !writtenSet[name] && (writtenSet != nil || !slices.Contains(written, name)) {
				rest = append(rest, name)
			}
		}
		slices.Sort(rest)
		for _, name := range rest {
			if err := writeProperty(enc, name, properties[name]); err != nil {
				return err
			}
		}
	}
	return enc.WriteToken(jsontext.EndObject)
}

// writeProperty writes a member named name whose value is value, the name as
// json.Marshal writes a map's key.
func writeProperty(enc *jsontext.Encoder, name string, value any) error {
	if err := writeMetaName(enc, name); err != nil {
		return err
	}
	return jsonv2.MarshalEncode(enc, value)
}

// marshalOrderedProperties encodes properties as a JSON object. Keys listed in
// order come first, names not in properties are skipped, and the remaining
// keys follow in sorted order. Keys and values go through json.Marshal, so
// escaping matches the encoding of a map.
func marshalOrderedProperties(properties map[string]any, order []string) (json.RawMessage, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	written := make(map[string]bool, len(properties))
	writeProperty := func(name string) error {
		key, err := json.Marshal(name)
		if err != nil {
			return err
		}
		value, err := json.Marshal(properties[name])
		if err != nil {
			return err
		}
		if len(written) > 0 {
			buf.WriteByte(',')
		}
		written[name] = true
		buf.Write(key)
		buf.WriteByte(':')
		buf.Write(value)
		return nil
	}

	for _, name := range order {
		if _, ok := properties[name]; !ok || written[name] {
			continue
		}
		if err := writeProperty(name); err != nil {
			return nil, err
		}
	}

	if len(written) < len(properties) {
		rest := make([]string, 0, len(properties)-len(written))
		for name := range properties {
			if !written[name] {
				rest = append(rest, name)
			}
		}
		slices.Sort(rest)
		for _, name := range rest {
			if err := writeProperty(name); err != nil {
				return nil, err
			}
		}
	}

	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// propertyOrder returns the keys of the JSON object raw in the order they
// appear, keeping the first position of a duplicate key. It returns nil when
// raw is not an object or has no keys.
func propertyOrder(raw json.RawMessage) []string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil
	}

	var order []string
	seen := make(map[string]bool)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil
		}
		name, ok := tok.(string)
		if !ok {
			return nil
		}
		if !seen[name] {
			seen[name] = true
			order = append(order, name)
		}

		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil
		}
	}
	return order
}

// toolArgumentsSchemaUnmarshalJSON decodes data into tis, keeping numbers
// exact, as json.Number. A definitions keyword moves to Defs, its local
// refs to "#/definitions" or into it rewritten to "#/$defs", only when Defs is
// still nil once $defs is decoded and $schema names no draft before 2019-09;
// otherwise it stays in AdditionalFields as written, refs untouched.
func toolArgumentsSchemaUnmarshalJSON(data []byte, tis *ToolArgumentsSchema) error {
	// The modelled keywords are looked up by their exact names, as JSON keys
	// and JSON Schema keywords are case-sensitive: a decode into struct
	// fields matches a name in any case, and would take "Properties" for
	// properties while AdditionalFields kept it too, so the schema encoded
	// again with a properties it never had. A key in another case is an
	// unknown keyword, kept in AdditionalFields as written.
	var keywords map[string]json.RawMessage
	if err := json.Unmarshal(data, &keywords); err != nil {
		return err
	}
	aux := struct {
		// Defs, Definitions and AdditionalProperties are kept raw so their
		// numbers are decoded exactly.
		Defs, Definitions, AdditionalProperties json.RawMessage
		// Type is kept raw so an array-valued type decodes into Types.
		Type json.RawMessage
		// Properties is kept raw so the raw bytes keep the key order and its
		// numbers are decoded exactly.
		Properties json.RawMessage
	}{
		Defs:                 keywords["$defs"],
		Definitions:          keywords["definitions"],
		AdditionalProperties: keywords["additionalProperties"],
		Type:                 keywords["type"],
		Properties:           keywords["properties"],
	}
	if required, ok := keywords["required"]; ok {
		if err := json.Unmarshal(required, &tis.Required); err != nil {
			return err
		}
	}

	if len(aux.Type) > 0 {
		if bytes.TrimSpace(aux.Type)[0] == '[' {
			var types []string
			if err := json.Unmarshal(aux.Type, &types); err != nil {
				return fmt.Errorf("decode schema type array: %w", err)
			}
			tis.Type = ""
			tis.Types = types
		} else {
			if err := json.Unmarshal(aux.Type, &tis.Type); err != nil {
				return fmt.Errorf("decode schema type: %w", err)
			}
			tis.Types = nil
		}
	}

	// Numbers are kept as written: a const or enum past 2^53 is not rounded,
	// and a number no float64 holds is not refused.
	if aux.Defs != nil {
		if err := decodeExact(aux.Defs, &tis.Defs); err != nil {
			return err
		}
	}
	var definitions map[string]any
	if aux.Definitions != nil {
		if err := decodeExact(aux.Definitions, &definitions); err != nil {
			return err
		}
	}
	if aux.Properties != nil {
		if err := decodeExact(aux.Properties, &tis.Properties); err != nil {
			return err
		}
		tis.PropertyOrder = propertyOrder(aux.Properties)
	}
	if aux.AdditionalProperties != nil {
		if err := decodeExact(aux.AdditionalProperties, &tis.AdditionalProperties); err != nil {
			return err
		}
	}

	var additionalFields map[string]any
	if err := decodeExact(data, &additionalFields); err != nil {
		return err
	}
	for key := range additionalFields {
		if isModeledToolSchemaKeyword(key) {
			delete(additionalFields, key)
		}
	}
	// A null definitions is no container, and is dropped.
	if additionalFields["definitions"] == nil {
		delete(additionalFields, "definitions")
	}

	// definitions moves to Defs when Defs is still nil and the dialect reads
	// $defs: 2019-09 on, or none declared. Defs is marshalled as "$defs", so
	// local refs to "#/definitions" or into it are rewritten to "#/$defs", or
	// they would dangle. Otherwise definitions stays in additionalFields as
	// written: an older draft reads no $defs, and a schema with both
	// containers needs both. The moved map is the local decode and the kept
	// one additionalFields' own, so the two never share a value.
	dialect := schemaDialectOf(additionalFields["$schema"])
	if definitions != nil && tis.Defs == nil && !dialect.beforeDraft2019() {
		delete(additionalFields, "definitions")
		tis.Defs = definitions
		rewriteDraft07LocalRefsIn(tis.Defs, dialect)
		rewriteDraft07LocalRefsIn(tis.Properties, dialect)
		rewriteDraft07LocalRefs(tis.AdditionalProperties, dialect)
		// The kept keywords are the root's own: its $id does not stop the
		// rewrite, a nested one does.
		rewriteDraft07LocalRefsAt(additionalFields, dialect, true)
	}
	if len(additionalFields) == 0 {
		additionalFields = nil
	}

	tis.AdditionalFields = additionalFields
	return nil
}

// decodeExact decodes the JSON value raw into v, keeping numbers as
// json.Number.
func decodeExact(raw []byte, v any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	return decoder.Decode(v)
}

// rewriteDraft07LocalRefs rewrites local draft-07 pointers to definitions
// to their 2019-09+ equivalent in place, in schema, a schema or an array of
// schemas, and the schemas inside it: a $ref (or a 2020-12 $dynamicRef) of
// exactly "#/definitions", or starting with "#/definitions/", follows it to
// "#/$defs". Any other ref, such as "#/definitionsX/...", a non-local or a
// percent-encoded one, is left untouched, and so is everything that is not
// a schema in dialect: a keyword is followed only when subschemaKeyword
// names it. A subschema that is a resource of its own (see
// embeddedResource) has local refs that point into it, so the rewrite stops
// there.
func rewriteDraft07LocalRefs(schema any, dialect schemaDialect) {
	rewriteDraft07LocalRefsAt(schema, dialect, false)
}

// rewriteDraft07LocalRefsAt is rewriteDraft07LocalRefs; root says schema is
// the root resource, whose own $id does not stop the rewrite.
func rewriteDraft07LocalRefsAt(schema any, dialect schemaDialect, root bool) {
	switch v := schema.(type) {
	case map[string]any:
		if resource, _ := embeddedResource(v, dialect); resource && !root {
			return
		}
		rewriteDraft07LocalRef(v, "$ref")
		// A $dynamicRef to a pointer resolves as a $ref does; before
		// 2020-12 it is no keyword.
		if dialect == dialect2020 {
			rewriteDraft07LocalRef(v, "$dynamicRef")
		}
		for key, child := range v {
			switch subschemaKeyword(key, dialect) {
			case subschemas:
				rewriteDraft07LocalRefs(child, dialect)
			case namedSubschemas:
				rewriteDraft07LocalRefsIn(child, dialect)
			}
		}
	case []any:
		for _, child := range v {
			rewriteDraft07LocalRefs(child, dialect)
		}
	}
}

// rewriteDraft07LocalRef rewrites the ref schema[keyword] when it is
// "#/definitions" or starts with "#/definitions/": "#/definitions" becomes
// "#/$defs", and the rest of the pointer is kept.
func rewriteDraft07LocalRef(schema map[string]any, keyword string) {
	ref, _ := schema[keyword].(string)
	if rest, ok := strings.CutPrefix(ref, "#/definitions"); ok && (rest == "" || rest[0] == '/') {
		schema[keyword] = "#/$defs" + rest
	}
}

// rewriteDraft07LocalRefsIn is rewriteDraft07LocalRefs for each schema of a
// map from names to schemas, such as properties or $defs, whose keys are
// names and not keywords.
func rewriteDraft07LocalRefsIn(named any, dialect schemaDialect) {
	if schemas, ok := named.(map[string]any); ok {
		for _, schema := range schemas {
			rewriteDraft07LocalRefs(schema, dialect)
		}
	}
}
