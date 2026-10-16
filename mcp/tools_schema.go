package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// ToolArgumentsSchema represents a JSON Schema for tool arguments.
type ToolArgumentsSchema struct {
	Defs map[string]any `json:"$defs,omitempty"`
	Type string         `json:"type"`
	// Types holds an array-valued JSON Schema type, including a one-element
	// array. Type stays a string for compatibility and wins when it is not
	// empty. Decoding a string type clears Types; decoding an array clears Type.
	Types                []string       `json:"-"`
	Properties           map[string]any `json:"properties"`
	Required             []string       `json:"required,omitempty"`
	AdditionalProperties any            `json:"additionalProperties,omitempty"`
	// AdditionalFields holds the top-level JSON Schema keywords the other
	// fields do not model, such as $schema, title or oneOf. The modeled names
	// (type, properties, required, additionalProperties, $defs and its alias
	// definitions) are reserved: an entry under one of them is never written,
	// even when its field is empty. Decoding keeps numbers exact, as
	// json.Number.
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

type ToolOutputSchema ToolArgumentsSchema

// MarshalJSON implements the json.Marshaler interface for ToolInputSchema.
func (tis ToolInputSchema) MarshalJSON() ([]byte, error) {
	return ToolArgumentsSchema(tis).MarshalJSON()
}

// MarshalJSON implements the json.Marshaler interface for ToolOutputSchema.
func (tis ToolOutputSchema) MarshalJSON() ([]byte, error) {
	return ToolArgumentsSchema(tis).MarshalJSON()
}

// MarshalJSON implements the json.Marshaler interface for ToolArgumentsSchema.
func (tis ToolArgumentsSchema) MarshalJSON() ([]byte, error) {
	return toolArgumentsSchemaMarshalJSON(tis)
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
	case "type", "properties", "required", "additionalProperties", "$defs", "definitions":
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

// It handles both "$defs" (JSON Schema 2019-09+) and "definitions" (JSON Schema draft-07)
// by reading either field and storing it in the Defs field.
func toolArgumentsSchemaUnmarshalJSON(data []byte, tis *ToolArgumentsSchema) error {
	// Use a temporary type to avoid infinite recursion
	type Alias ToolArgumentsSchema
	aux := &struct {
		Definitions map[string]any `json:"definitions,omitempty"`
		// Type shadows Alias.Type so an array-valued type decodes into Types.
		Type json.RawMessage `json:"type"`
		// Properties shadows Alias.Properties so the raw bytes keep the key order.
		Properties json.RawMessage `json:"properties"`
		*Alias
	}{
		Alias: (*Alias)(tis),
	}

	if err := json.Unmarshal(data, aux); err != nil {
		return err
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

	if aux.Properties != nil {
		if err := json.Unmarshal(aux.Properties, &tis.Properties); err != nil {
			return err
		}
		tis.PropertyOrder = propertyOrder(aux.Properties)
	}

	// Numbers are kept as written: a const or enum past 2^53 is not rounded,
	// and a number no float64 holds is not refused.
	var additionalFields map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&additionalFields); err != nil {
		return err
	}
	for key := range additionalFields {
		if isModeledToolSchemaKeyword(key) {
			delete(additionalFields, key)
		}
	}
	if len(additionalFields) == 0 {
		additionalFields = nil
	}

	// If $defs wasn't provided but definitions was, use definitions.
	// Marshaling re-emits Defs as "$defs", so local "#/definitions/..." $ref
	// pointers must be rewritten to "#/$defs/..." or the round-tripped schema
	// carries dangling references that strict validators reject.
	if tis.Defs == nil && aux.Definitions != nil {
		tis.Defs = aux.Definitions
		dialect := schemaDialectOf(additionalFields["$schema"])
		rewriteDraft07LocalRefsIn(tis.Defs, dialect)
		rewriteDraft07LocalRefsIn(tis.Properties, dialect)
		rewriteDraft07LocalRefs(tis.AdditionalProperties, dialect)
		// The kept keywords are the root's own: its $id does not stop the
		// rewrite, a nested one does.
		rewriteDraft07LocalRefsAt(additionalFields, dialect, true)
	}

	tis.AdditionalFields = additionalFields
	return nil
}

// rewriteDraft07LocalRefs rewrites local draft-07 "#/definitions/..." $ref
// pointers to their 2019-09+ "#/$defs/..." equivalent in place, in schema,
// a schema or an array of schemas, and the schemas inside it. Non-local refs
// are left untouched, and so is everything that is not a schema in dialect:
// a keyword is followed only when subschemaKeyword names it. A subschema
// that is a resource of its own (see embeddedResource) has local refs that
// point into it, so the rewrite stops there.
func rewriteDraft07LocalRefs(schema any, dialect schemaDialect) {
	rewriteDraft07LocalRefsAt(schema, dialect, false)
}

// rewriteDraft07LocalRefsAt is rewriteDraft07LocalRefs; root says schema is
// the root resource, whose own $id does not stop the rewrite.
func rewriteDraft07LocalRefsAt(schema any, dialect schemaDialect, root bool) {
	const draft07Prefix = "#/definitions/"
	switch v := schema.(type) {
	case map[string]any:
		if resource, _ := embeddedResource(v, dialect); resource && !root {
			return
		}
		if ref, ok := v["$ref"].(string); ok && strings.HasPrefix(ref, draft07Prefix) {
			v["$ref"] = "#/$defs/" + ref[len(draft07Prefix):]
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
