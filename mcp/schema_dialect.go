package mcp

import "strings"

// schemaDialect is the JSON Schema dialect a schema declares.
type schemaDialect int

const (
	// dialect2020 is 2020-12, the dialect of a schema that declares none.
	dialect2020 schemaDialect = iota
	dialect2019
	dialectDraft07
	dialectDraft06
	dialectDraft04
)

// beforeDraft2019 reports whether d is draft-04, -06 or -07, where a $ref
// hides every keyword beside it.
func (d schemaDialect) beforeDraft2019() bool {
	return d == dialectDraft04 || d == dialectDraft06 || d == dialectDraft07
}

// schemaDialectOf returns the dialect a $schema value names, read as the
// validator reads it: the whole URI, http or https, with no fragment or an
// empty one. A schema without $schema is 2020-12, and so is one naming a
// dialect this package does not know, which validation refuses on its own.
func schemaDialectOf(schemaURI any) schemaDialect {
	uri, _ := schemaURI.(string)
	uri, fragment, _ := strings.Cut(uri, "#")
	if fragment != "" {
		return dialect2020
	}
	if rest, ok := strings.CutPrefix(uri, "http://"); ok {
		uri = rest
	} else {
		uri = strings.TrimPrefix(uri, "https://")
	}
	switch uri {
	case "json-schema.org/draft/2019-09/schema":
		return dialect2019
	case "json-schema.org/draft-07/schema":
		return dialectDraft07
	case "json-schema.org/draft-06/schema":
		return dialectDraft06
	case "json-schema.org/draft-04/schema":
		return dialectDraft04
	}
	return dialect2020
}

// embeddedResource reports whether schema, a subschema met while walking
// one of dialect, is the root of a schema resource of its own, and the
// dialect it is walked in, both as the validator decides them: it is a
// resource when its id names one (see hasResourceID), and only then does a
// $schema it carries name its dialect.
func embeddedResource(schema map[string]any, dialect schemaDialect) (bool, schemaDialect) {
	if uri, ok := schema["$schema"].(string); ok {
		if own := schemaDialectOf(uri); hasResourceID(schema, own) {
			return true, own
		}
	}
	return hasResourceID(schema, dialect), dialect
}

// hasResourceID reports whether schema, read in dialect, has an id ($id, or
// id in draft-04) that names a resource: one that is not empty without its
// fragment, which alone is only an anchor in the resource schema is in.
// Before 2019-09 a $ref hides the keywords beside it, the id among them.
func hasResourceID(schema map[string]any, dialect schemaDialect) bool {
	if _, ok := schema["$ref"]; ok && dialect.beforeDraft2019() {
		return false
	}
	idKeyword := "$id"
	if dialect == dialectDraft04 {
		idKeyword = "id"
	}
	id, _ := schema[idKeyword].(string)
	base, _, _ := strings.Cut(id, "#")
	return base != ""
}

// subschemaKind says what the value of a JSON Schema keyword holds.
type subschemaKind int

const (
	// noSubschema: the value is data, an annotation or a keyword this
	// package does not know, never searched for schemas.
	noSubschema subschemaKind = iota
	// subschemas: a schema, or an array of schemas.
	subschemas
	// namedSubschemas: a map from names to schemas (or, for draft-07
	// dependencies, to arrays of property names).
	namedSubschemas
)

// subschemaKeyword returns what the value of key holds in dialect, by the
// validator's list of the keywords that hold schemas in that dialect
// (santhosh-tekuri/jsonschema, Draft.subschemas). Each draft's list keeps
// every keyword of the one before: 2020-12 still reads definitions,
// dependencies and additionalItems. Any other keyword holds data.
func subschemaKeyword(key string, dialect schemaDialect) subschemaKind {
	since2019 := dialect == dialect2019 || dialect == dialect2020
	switch key {
	case "not", "allOf", "anyOf", "oneOf", "additionalProperties", "items", "additionalItems":
		return subschemas
	case "definitions", "properties", "patternProperties", "dependencies":
		return namedSubschemas
	case "contains", "propertyNames":
		if dialect != dialectDraft04 {
			return subschemas
		}
	case "if", "then", "else":
		if dialect != dialectDraft04 && dialect != dialectDraft06 {
			return subschemas
		}
	case "unevaluatedProperties", "unevaluatedItems", "contentSchema":
		if since2019 {
			return subschemas
		}
	case "$defs", "dependentSchemas":
		if since2019 {
			return namedSubschemas
		}
	case "prefixItems":
		if dialect == dialect2020 {
			return subschemas
		}
	}
	return noSubschema
}
