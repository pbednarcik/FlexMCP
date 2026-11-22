package mcp

import (
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// schemaFor generates a JSON schema for T, applying mcp-go struct field tag
// conventions on top of github.com/google/jsonschema-go/jsonschema.
//
// Supported field tags:
//   - jsonschema_description: property description
//   - jsonschema: plain text description, or comma-separated options such as enum=value
func schemaFor[T any]() (*jsonschema.Schema, error) {
	opts := &jsonschema.ForOptions{IgnoreInvalidTypes: true}
	state := schemaFallbackState{active: make(map[reflect.Type]bool)}

	schema, err := jsonschema.For[T](opts)
	if err != nil {
		if !isJSONSchemaTagOptionError(err) || !supportsSchemaTagFallback(reflect.TypeFor[T]()) {
			return nil, err
		}
		schema, err = state.schemaForStructFields(reflect.TypeFor[T](), opts, false)
	} else {
		schema, err = state.reconcileNativeSchema(reflect.TypeFor[T](), schema, opts, false)
	}
	if err != nil {
		return nil, err
	}

	applyStructFieldTags(reflect.TypeFor[T](), schema)
	return schema, nil
}

func isJSONSchemaTagOptionError(err error) bool {
	return strings.Contains(err.Error(), "tag must not begin with 'WORD='")
}

func supportsSchemaTagFallback(t reflect.Type) bool {
	return supportsSchemaTagFallbackType(t, make(map[reflect.Type]bool))
}

func supportsSchemaTagFallbackType(t reflect.Type, seen map[reflect.Type]bool) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Array, reflect.Slice:
		return supportsSchemaTagFallbackType(t.Elem(), seen)
	case reflect.Map:
		return supportsSchemaTagFallbackType(t.Elem(), seen)
	case reflect.Struct:
		if seen[t] {
			return true
		}
		seen[t] = true
		for field := range t.Fields() {
			// encoding/json promotes the fields of an unexported embedded
			// struct, so their tags are checked too.
			fieldType := field.Type
			if fieldType.Kind() == reflect.Pointer {
				fieldType = fieldType.Elem()
			}
			if !field.IsExported() && (!field.Anonymous || fieldType.Kind() != reflect.Struct) {
				continue
			}
			if fieldJSONTagInfo(field).omit {
				continue
			}
			if tag, ok := field.Tag.Lookup("jsonschema"); ok {
				for option := range strings.SplitSeq(tag, ",") {
					option = strings.TrimSpace(option)
					if strings.Contains(option, "=") && !strings.HasPrefix(option, "enum=") {
						return false
					}
				}
			}
			if !supportsSchemaTagFallbackType(field.Type, seen) {
				return false
			}
		}
	}
	return true
}

var errRecursiveSchemaFallback = errors.New("recursive type cannot use schema tag fallback")

type schemaFallbackState struct {
	active map[reflect.Type]bool
}

// schemaForStructFields builds the schema of struct t (or a pointer to one)
// field by field. addressable says whether encoding/json can take the
// value's address there, which decides whether a pointer-receiver
// MarshalJSON or MarshalText applies.
func (s *schemaFallbackState) schemaForStructFields(
	t reflect.Type,
	opts *jsonschema.ForOptions,
	addressable bool,
) (*jsonschema.Schema, error) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
		addressable = true
	}
	if t.Kind() != reflect.Struct {
		return jsonschema.ForType(t, opts)
	}
	fields, err := selectedJSONFields(t)
	if err != nil {
		return nil, err
	}
	return s.schemaForSelectedFields(t, fields, nil, opts, addressable)
}

// schemaForSelectedFields builds the object schema of struct t from the
// fields selectedJSONFields chose, in their order. A field with an entry in
// nativeProperties keeps that schema, reconciled; the rest are built anew.
func (s *schemaFallbackState) schemaForSelectedFields(
	t reflect.Type,
	fields []jsonSchemaField,
	nativeProperties map[string]*jsonschema.Schema,
	opts *jsonschema.ForOptions,
	addressable bool,
) (*jsonschema.Schema, error) {
	if s.active[t] {
		return nil, fmt.Errorf("%w: %s", errRecursiveSchemaFallback, t)
	}
	s.active[t] = true
	defer delete(s.active, t)

	schema := &jsonschema.Schema{
		Type:                 "object",
		Properties:           map[string]*jsonschema.Schema{},
		AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
	}

	for _, field := range fields {
		var fieldSchema *jsonschema.Schema
		var err error
		fieldAddressable := addressable || field.throughPointer
		if native := nativeProperties[field.info.name]; native != nil {
			fieldSchema, err = s.reconcileNativeSchema(field.field.Type, native, opts, fieldAddressable)
		} else {
			fieldSchema, err = s.schemaForFieldType(field.field.Type, opts, fieldAddressable)
		}
		if err != nil {
			if errors.Is(err, errRecursiveSchemaFallback) {
				return nil, err
			}
			if opts.IgnoreInvalidTypes {
				continue
			}
			return nil, err
		}
		if fieldSchema == nil {
			continue
		}

		schema.Properties[field.info.name] = fieldSchema
		schema.PropertyOrder = append(schema.PropertyOrder, field.info.name)
		if !field.info.settings["omitempty"] && !field.info.settings["omitzero"] {
			schema.Required = append(schema.Required, field.info.name)
		}
	}

	return schema, nil
}

func (s *schemaFallbackState) schemaForFieldType(
	t reflect.Type,
	opts *jsonschema.ForOptions,
	addressable bool,
) (*jsonschema.Schema, error) {
	schema, err := jsonschema.ForType(t, opts)
	if err == nil {
		return s.reconcileNativeSchema(t, schema, opts, addressable)
	}
	if !isJSONSchemaTagOptionError(err) || !supportsSchemaTagFallback(t) {
		return schema, err
	}

	switch t.Kind() {
	case reflect.Pointer:
		return s.schemaForFieldType(t.Elem(), opts, true)
	case reflect.Struct:
		return s.schemaForStructFields(t, opts, addressable)
	case reflect.Array, reflect.Slice:
		items, itemErr := s.schemaForFieldType(t.Elem(), opts, addressable || t.Kind() == reflect.Slice)
		if itemErr != nil {
			return nil, itemErr
		}
		schema := &jsonschema.Schema{Type: "array", Items: items}
		if t.Kind() == reflect.Array {
			schema.MinItems = new(t.Len())
			schema.MaxItems = new(t.Len())
		}
		return schema, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, err
		}
		values, valueErr := s.schemaForFieldType(t.Elem(), opts, false)
		if valueErr != nil {
			return nil, valueErr
		}
		return &jsonschema.Schema{Type: "object", AdditionalProperties: values}, nil
	default:
		return nil, err
	}
}

// reconcileNativeSchema keeps a schema jsonschema-go inferred, and rebuilds a
// struct's properties only where its field choice, which follows Go's
// selector rules and lets a later field of one JSON name overwrite an
// earlier one, disagrees with the field encoding/json selects. A type that
// encodes itself is left as it is.
func (s *schemaFallbackState) reconcileNativeSchema(
	t reflect.Type,
	schema *jsonschema.Schema,
	opts *jsonschema.ForOptions,
	addressable bool,
) (*jsonschema.Schema, error) {
	if schema == nil || hasCustomJSONEncoding(t, addressable) {
		return schema, nil
	}
	var err error
	switch t.Kind() {
	case reflect.Pointer:
		return s.reconcileNativeSchema(t.Elem(), schema, opts, true)
	case reflect.Struct:
		if schema.Properties == nil {
			return schema, nil
		}
		// jsonschema-go accepted this type; a recursive embedding adds no
		// field the walk has not seen, so its error does not apply here.
		fields, _ := selectedJSONFields(t)
		native, matches, selectionErr := nativeJSONFieldSelection(t, fields, schema.Properties, opts)
		if selectionErr != nil {
			return nil, selectionErr
		}
		if !matches {
			preserved := make(map[string]*jsonschema.Schema, len(fields))
			for _, field := range fields {
				if source, ok := native[field.info.name]; ok && sameJSONField(source, field.field) {
					preserved[field.info.name] = schema.Properties[field.info.name]
				}
			}
			rebuilt, buildErr := s.schemaForSelectedFields(t, fields, preserved, opts, addressable)
			if buildErr != nil {
				return nil, buildErr
			}
			rebuilt.Type, rebuilt.Types = schema.Type, schema.Types
			rebuilt.Description = schema.Description
			return rebuilt, nil
		}
		for _, field := range fields {
			prop, propErr := s.reconcileNativeSchema(field.field.Type, schema.Properties[field.info.name], opts, addressable || field.throughPointer)
			if propErr != nil {
				return nil, propErr
			}
			if prop != nil {
				schema.Properties[field.info.name] = prop
			}
		}
	case reflect.Array, reflect.Slice:
		schema.Items, err = s.reconcileNativeSchema(t.Elem(), schema.Items, opts, addressable || t.Kind() == reflect.Slice)
	case reflect.Map:
		if t.Key().Kind() == reflect.String {
			schema.AdditionalProperties, err = s.reconcileNativeSchema(t.Elem(), schema.AdditionalProperties, opts, false)
		}
	}
	return schema, err
}

// nativeJSONFieldSelection says, per JSON name, which field jsonschema-go
// took its property from, and whether that is exactly the selection
// encoding/json makes.
func nativeJSONFieldSelection(
	t reflect.Type,
	fields []jsonSchemaField,
	properties map[string]*jsonschema.Schema,
	opts *jsonschema.ForOptions,
) (map[string]reflect.StructField, bool, error) {
	native := make(map[string]reflect.StructField, len(fields))
	matches := true
	for _, field := range reflect.VisibleFields(t) {
		info := fieldJSONInfo(field)
		if field.Anonymous || info.omit {
			continue
		}
		if _, duplicate := native[info.name]; duplicate {
			matches = false
			if opts.IgnoreInvalidTypes && properties[info.name] != nil {
				// A field jsonschema-go ignores does not overwrite the
				// property an earlier field wrote.
				candidate, err := jsonschema.ForType(field.Type, opts)
				if err != nil {
					return nil, false, err
				}
				if candidate == nil {
					continue
				}
			}
		}
		native[info.name] = field
	}
	if len(native) != len(fields) {
		matches = false
	}
	for _, field := range fields {
		if got, ok := native[field.info.name]; !ok || !sameJSONField(got, field.field) {
			matches = false
		}
	}
	return native, matches, nil
}

func sameJSONField(a, b reflect.StructField) bool {
	return a.Name == b.Name && a.Type == b.Type && a.Tag == b.Tag
}

// hasCustomJSONEncoding reports whether encoding/json encodes a value of t
// through its own MarshalJSON or MarshalText; a pointer-receiver method
// counts only where the value is addressable.
func hasCustomJSONEncoding(t reflect.Type, addressable bool) bool {
	for _, iface := range []reflect.Type{reflect.TypeFor[json.Marshaler](), reflect.TypeFor[encoding.TextMarshaler]()} {
		if t.Implements(iface) || (addressable && reflect.PointerTo(t).Implements(iface)) {
			return true
		}
	}
	return false
}

func applyStructFieldTags(t reflect.Type, schema *jsonschema.Schema) {
	if schema == nil || schema.Properties == nil {
		return
	}
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}

	// A recursive embedding (type T struct{ *T }) adds no new fields; the
	// guard stops the walk from following it forever.
	active := make(map[reflect.Type]bool)
	var walk func(reflect.Type)
	walk = func(structType reflect.Type) {
		if active[structType] {
			return
		}
		active[structType] = true
		defer delete(active, structType)
		for field := range structType.Fields() {
			fieldType := field.Type
			info := fieldJSONInfo(field)
			if info.omit {
				continue
			}
			if field.Anonymous && !info.explicitName {
				anonType := fieldType
				if anonType.Kind() == reflect.Pointer {
					anonType = anonType.Elem()
				}
				if anonType.Kind() == reflect.Struct {
					walk(anonType)
					continue
				}
			}

			prop, ok := schema.Properties[info.name]
			if !ok {
				continue
			}

			description, enums := fieldSchemaAnnotations(field)
			if description != "" {
				prop.Description = description
			}
			if len(enums) > 0 {
				prop.Enum = enums
			}
			applyStructFieldTagsToType(fieldType, prop)
		}
	}

	walk(t)
}

func applyStructFieldTagsToType(t reflect.Type, schema *jsonschema.Schema) {
	if schema == nil {
		return
	}

	switch t.Kind() {
	case reflect.Pointer:
		applyStructFieldTagsToType(t.Elem(), schema)
	case reflect.Struct:
		applyStructFieldTags(t, schema)
	case reflect.Array, reflect.Slice:
		applyStructFieldTagsToType(t.Elem(), schema.Items)
	case reflect.Map:
		if t.Key().Kind() == reflect.String {
			applyStructFieldTagsToType(t.Elem(), schema.AdditionalProperties)
		}
	}
}

type jsonSchemaField struct {
	field     reflect.StructField
	info      jsonFieldInfo
	depth     int
	ambiguous bool
	// throughPointer: the field is promoted through an embedded pointer,
	// so encoding/json reaches it addressable.
	throughPointer bool
}

// selectedJSONFields resolves which fields of struct t encoding/json reads
// and writes, in first-seen order: among fields sharing a JSON name the
// shallowest wins, at one depth a tagged field beats an untagged one, and two
// of a kind at one depth are both dropped. An unexported embedded struct
// still promotes its fields. A recursive embedding returns the fields found
// with errRecursiveSchemaFallback.
func selectedJSONFields(t reflect.Type) ([]jsonSchemaField, error) {
	fields := make(map[string]jsonSchemaField)
	var names []string
	var cycleErr error
	active := make(map[reflect.Type]bool)
	var walk func(reflect.Type, int, bool)
	walk = func(t reflect.Type, depth int, throughPointer bool) {
		if active[t] {
			cycleErr = fmt.Errorf("%w: %s", errRecursiveSchemaFallback, t)
			return
		}
		active[t] = true
		defer delete(active, t)
		for field := range t.Fields() {
			info := fieldJSONTagInfo(field)
			if info.omit {
				continue
			}
			fieldType := field.Type
			if fieldType.Kind() == reflect.Pointer {
				fieldType = fieldType.Elem()
			}
			if field.Anonymous && !info.explicitName && fieldType.Kind() == reflect.Struct {
				walk(fieldType, depth+1, throughPointer || field.Type.Kind() == reflect.Pointer)
				continue
			}
			if !field.IsExported() && (!field.Anonymous || fieldType.Kind() != reflect.Struct) {
				continue
			}
			previous, exists := fields[info.name]
			if !exists {
				names = append(names, info.name)
			} else {
				if previous.depth < depth {
					continue
				}
				if previous.depth == depth {
					if previous.info.explicitName && !info.explicitName {
						continue
					}
					if previous.info.explicitName == info.explicitName {
						previous.ambiguous = true
						fields[info.name] = previous
						continue
					}
				}
			}
			fields[info.name] = jsonSchemaField{field: field, info: info, depth: depth, throughPointer: throughPointer}
		}
	}
	walk(t, 0, false)

	var selected []jsonSchemaField
	for _, name := range names {
		if field := fields[name]; !field.ambiguous {
			selected = append(selected, field)
		}
	}
	return selected, cycleErr
}

func fieldSchemaAnnotations(field reflect.StructField) (string, []any) {
	description := ""
	var enums []any

	if tag, ok := field.Tag.Lookup("jsonschema_description"); ok {
		description = strings.TrimSpace(tag)
	}

	if tag, ok := field.Tag.Lookup("jsonschema"); ok {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			return description, enums
		}

		parsedDescription, parsedEnums := parseJSONSchemaTag(tag)
		if len(parsedEnums) > 0 {
			enums = parsedEnums
		}
		if description == "" && parsedDescription != "" {
			description = parsedDescription
		}
	}

	return description, enums
}

func parseJSONSchemaTag(tag string) (string, []any) {
	parts := strings.Split(tag, ",")
	var textParts []string
	var enums []any

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		if enumValue, ok := strings.CutPrefix(part, "enum="); ok {
			enums = append(enums, enumValue)
			continue
		}

		textParts = append(textParts, part)
	}

	return strings.Join(textParts, ", "), enums
}

type jsonFieldInfo struct {
	omit         bool
	explicitName bool
	name         string
	settings     map[string]bool
}

func fieldJSONInfo(field reflect.StructField) jsonFieldInfo {
	if !field.IsExported() {
		return jsonFieldInfo{omit: true}
	}
	return fieldJSONTagInfo(field)
}

// fieldJSONTagInfo reads the json tag alone, exported or not: an unexported
// embedded struct still promotes its fields.
func fieldJSONTagInfo(field reflect.StructField) jsonFieldInfo {
	info := jsonFieldInfo{name: field.Name}
	tag, ok := field.Tag.Lookup("json")
	if !ok {
		return info
	}

	name, rest, found := strings.Cut(tag, ",")
	if name == "-" && !found {
		return jsonFieldInfo{omit: true}
	}
	if name != "" {
		info.name = name
		info.explicitName = true
	}
	if rest != "" {
		info.settings = map[string]bool{}
		for setting := range strings.SplitSeq(rest, ",") {
			info.settings[setting] = true
		}
	}
	return info
}

func schemaForRawMessage[T any]() ([]byte, error) {
	schema, err := schemaFor[T]()
	if err != nil {
		return nil, fmt.Errorf("generate schema: %w", err)
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("marshal schema: %w", err)
	}
	return raw, nil
}
