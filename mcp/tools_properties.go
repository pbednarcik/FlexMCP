package mcp

// PropertyOption is a function that configures a property in a Tool's input schema.
// It allows for flexible configuration of JSON Schema properties using the functional options pattern.
type PropertyOption func(map[string]any)

//
// Common Property Options
//

// Description adds a description to a property in the JSON Schema.
// The description should explain the purpose and expected values of the property.
func Description(desc string) PropertyOption {
	return func(schema map[string]any) {
		schema["description"] = desc
	}
}

// Required marks a property as required in the tool's input schema.
// Required properties must be provided when using the tool.
func Required() PropertyOption {
	return func(schema map[string]any) {
		schema["required"] = true
	}
}

// Title adds a display-friendly title to a property in the JSON Schema.
// This title can be used by UI components to show a more readable property name.
func Title(title string) PropertyOption {
	return func(schema map[string]any) {
		schema["title"] = title
	}
}

//
// String Property Options
//

// DefaultString sets the default value for a string property.
// This value will be used if the property is not explicitly provided.
func DefaultString(value string) PropertyOption {
	return func(schema map[string]any) {
		schema["default"] = value
	}
}

// Enum specifies a list of allowed values for a string property.
// The property value must be one of the specified enum values.
func Enum(values ...string) PropertyOption {
	return func(schema map[string]any) {
		schema["enum"] = values
	}
}

// MaxLength sets the maximum length for a string property.
// The string value must not exceed this length.
func MaxLength(max int) PropertyOption {
	return func(schema map[string]any) {
		schema["maxLength"] = max
	}
}

// MinLength sets the minimum length for a string property.
// The string value must be at least this length.
func MinLength(min int) PropertyOption {
	return func(schema map[string]any) {
		schema["minLength"] = min
	}
}

// Pattern sets a regex pattern that a string property must match.
// The string value must conform to the specified regular expression.
func Pattern(pattern string) PropertyOption {
	return func(schema map[string]any) {
		schema["pattern"] = pattern
	}
}

//
// Number Property Options
//

// DefaultNumber sets the default value for a number or integer property.
// This value will be used if the property is not explicitly provided.
func DefaultNumber[T int | int64 | float64](value T) PropertyOption {
	return func(schema map[string]any) {
		schema["default"] = value
	}
}

// Max sets the maximum value for a number or integer property.
// The number value must not exceed this maximum.
func Max[T int | int64 | float64](max T) PropertyOption {
	return func(schema map[string]any) {
		schema["maximum"] = max
	}
}

// Min sets the minimum value for a number or integer property.
// The number value must not be less than this minimum.
func Min[T int | int64 | float64](min T) PropertyOption {
	return func(schema map[string]any) {
		schema["minimum"] = min
	}
}

// MultipleOf specifies that a number or integer must be a multiple of the given value.
// The number value must be divisible by this value.
func MultipleOf[T int | int64 | float64](value T) PropertyOption {
	return func(schema map[string]any) {
		schema["multipleOf"] = value
	}
}

//
// Boolean Property Options
//

// DefaultBool sets the default value for a boolean property.
// This value will be used if the property is not explicitly provided.
func DefaultBool(value bool) PropertyOption {
	return func(schema map[string]any) {
		schema["default"] = value
	}
}

//
// Array Property Options
//

// DefaultArray sets the default value for an array property.
// This value will be used if the property is not explicitly provided.
func DefaultArray[T any](value []T) PropertyOption {
	return func(schema map[string]any) {
		schema["default"] = value
	}
}

//
// Property Type Helpers
//

// WithInteger adds an integer property to the tool schema.
// It accepts property options to configure the integer property's behavior and constraints.
func WithInteger(name string, opts ...PropertyOption) ToolOption {
	return func(t *Tool) {
		schema := map[string]any{
			"type": "integer",
		}

		for _, opt := range opts {
			opt(schema)
		}

		// Remove required from property schema and add to InputSchema.required
		if required, ok := schema["required"].(bool); ok && required {
			delete(schema, "required")
			t.InputSchema.Required = append(t.InputSchema.Required, name)
		}

		t.InputSchema.Properties[name] = schema
	}
}

// WithBoolean adds a boolean property to the tool schema.
// It accepts property options to configure the boolean property's behavior and constraints.
func WithBoolean(name string, opts ...PropertyOption) ToolOption {
	return func(t *Tool) {
		schema := map[string]any{
			"type": "boolean",
		}

		for _, opt := range opts {
			opt(schema)
		}

		// Remove required from property schema and add to InputSchema.required
		if required, ok := schema["required"].(bool); ok && required {
			delete(schema, "required")
			t.InputSchema.Required = append(t.InputSchema.Required, name)
		}

		t.InputSchema.Properties[name] = schema
	}
}

// WithNumber adds a number property to the tool schema.
// It accepts property options to configure the number property's behavior and constraints.
func WithNumber(name string, opts ...PropertyOption) ToolOption {
	return func(t *Tool) {
		schema := map[string]any{
			"type": "number",
		}

		for _, opt := range opts {
			opt(schema)
		}

		// Remove required from property schema and add to InputSchema.required
		if required, ok := schema["required"].(bool); ok && required {
			delete(schema, "required")
			t.InputSchema.Required = append(t.InputSchema.Required, name)
		}

		t.InputSchema.Properties[name] = schema
	}
}

// WithString adds a string property to the tool schema.
// It accepts property options to configure the string property's behavior and constraints.
func WithString(name string, opts ...PropertyOption) ToolOption {
	return func(t *Tool) {
		schema := map[string]any{
			"type": "string",
		}

		for _, opt := range opts {
			opt(schema)
		}

		// Remove required from property schema and add to InputSchema.required
		if required, ok := schema["required"].(bool); ok && required {
			delete(schema, "required")
			t.InputSchema.Required = append(t.InputSchema.Required, name)
		}

		t.InputSchema.Properties[name] = schema
	}
}

// WithObject adds an object property to the tool schema.
// It accepts property options to configure the object property's behavior and constraints.
func WithObject(name string, opts ...PropertyOption) ToolOption {
	return func(t *Tool) {
		schema := map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}

		for _, opt := range opts {
			opt(schema)
		}

		// Remove required from property schema and add to InputSchema.required
		if required, ok := schema["required"].(bool); ok && required {
			delete(schema, "required")
			t.InputSchema.Required = append(t.InputSchema.Required, name)
		}

		t.InputSchema.Properties[name] = schema
	}
}

// WithArray returns a ToolOption that adds an array-typed property with the given name to a Tool's input schema.
// It applies provided PropertyOption functions to configure the property's schema, moves a `required` flag
// from the property schema into the Tool's InputSchema.Required slice when present, and registers the resulting
// schema under InputSchema.Properties[name].
func WithArray(name string, opts ...PropertyOption) ToolOption {
	return func(t *Tool) {
		schema := map[string]any{
			"type": "array",
		}

		for _, opt := range opts {
			opt(schema)
		}

		// Remove required from property schema and add to InputSchema.required
		if required, ok := schema["required"].(bool); ok && required {
			delete(schema, "required")
			t.InputSchema.Required = append(t.InputSchema.Required, name)
		}

		t.InputSchema.Properties[name] = schema
	}
}

// WithAny adds an input property named name with no predefined JSON Schema type to the Tool's input schema.
// The returned ToolOption applies the provided PropertyOption functions to the property's schema, moves a property-level
// `required` flag into the Tool's InputSchema.Required list if present, and stores the resulting schema under InputSchema.Properties[name].
func WithAny(name string, opts ...PropertyOption) ToolOption {
	return func(t *Tool) {
		schema := map[string]any{}

		for _, opt := range opts {
			opt(schema)
		}

		// Remove required from property schema and add to InputSchema.required
		if required, ok := schema["required"].(bool); ok && required {
			delete(schema, "required")
			t.InputSchema.Required = append(t.InputSchema.Required, name)
		}

		t.InputSchema.Properties[name] = schema
	}
}

// Properties sets the "properties" map for an object schema.
// The returned PropertyOption stores the provided map under the schema's "properties" key.
func Properties(props map[string]any) PropertyOption {
	return func(schema map[string]any) {
		schema["properties"] = props
	}
}

// AdditionalProperties specifies whether additional properties are allowed in the object
// or defines a schema for additional properties
func AdditionalProperties(schema any) PropertyOption {
	return func(schemaMap map[string]any) {
		schemaMap["additionalProperties"] = schema
	}
}

// MinProperties sets the minimum number of properties for an object
func MinProperties(min int) PropertyOption {
	return func(schema map[string]any) {
		schema["minProperties"] = min
	}
}

// MaxProperties sets the maximum number of properties for an object
func MaxProperties(max int) PropertyOption {
	return func(schema map[string]any) {
		schema["maxProperties"] = max
	}
}

// PropertyNames defines a schema for property names in an object
func PropertyNames(schema map[string]any) PropertyOption {
	return func(schemaMap map[string]any) {
		schemaMap["propertyNames"] = schema
	}
}

// Items defines the schema for array items.
// Accepts any schema definition for maximum flexibility.
//
// Example:
//
//	Items(map[string]any{
//	    "type": "object",
//	    "properties": map[string]any{
//	        "name": map[string]any{"type": "string"},
//	        "age": map[string]any{"type": "number"},
//	    },
//	})
//
// For simple types, use ItemsString(), ItemsNumber(), ItemsBoolean() instead.
func Items(schema any) PropertyOption {
	return func(schemaMap map[string]any) {
		schemaMap["items"] = schema
	}
}

// MinItems sets the minimum number of items for an array
func MinItems(min int) PropertyOption {
	return func(schema map[string]any) {
		schema["minItems"] = min
	}
}

// MaxItems sets the maximum number of items for an array
func MaxItems(max int) PropertyOption {
	return func(schema map[string]any) {
		schema["maxItems"] = max
	}
}

// UniqueItems specifies whether array items must be unique
func UniqueItems(unique bool) PropertyOption {
	return func(schema map[string]any) {
		schema["uniqueItems"] = unique
	}
}

// WithStringItems configures an array's items to be of type string.
//
// Supported options: Description(), DefaultString(), Enum(), MaxLength(), MinLength(), Pattern()
// Note: Options like Required() are not valid for item schemas and will be ignored.
//
// Examples:
//
//	mcp.WithArray("tags", mcp.WithStringItems())
//	mcp.WithArray("colors", mcp.WithStringItems(mcp.Enum("red", "green", "blue")))
//	mcp.WithArray("names", mcp.WithStringItems(mcp.MinLength(1), mcp.MaxLength(50)))
//
// Limitations: Only supports simple string arrays. Use Items() for complex objects.
func WithStringItems(opts ...PropertyOption) PropertyOption {
	return func(schema map[string]any) {
		itemSchema := map[string]any{
			"type": "string",
		}

		for _, opt := range opts {
			opt(itemSchema)
		}

		schema["items"] = itemSchema
	}
}

// WithStringEnumItems configures an array's items to be of type string with a specified enum.
// Example:
//
//	mcp.WithArray("priority", mcp.WithStringEnumItems([]string{"low", "medium", "high"}))
//
// Limitations: Only supports string enums. Use WithStringItems(Enum(...)) for more flexibility.
func WithStringEnumItems(values []string) PropertyOption {
	return func(schema map[string]any) {
		schema["items"] = map[string]any{
			"type": "string",
			"enum": values,
		}
	}
}

// WithNumberItems configures an array's items to be of type number.
//
// Supported options: Description(), DefaultNumber(), Min(), Max(), MultipleOf()
// Note: Options like Required() are not valid for item schemas and will be ignored.
//
// Examples:
//
//	mcp.WithArray("scores", mcp.WithNumberItems(mcp.Min(0), mcp.Max(100)))
//	mcp.WithArray("prices", mcp.WithNumberItems(mcp.Min(0)))
//
// Limitations: Only supports simple number arrays. Use Items() for complex objects.
func WithNumberItems(opts ...PropertyOption) PropertyOption {
	return func(schema map[string]any) {
		itemSchema := map[string]any{
			"type": "number",
		}

		for _, opt := range opts {
			opt(itemSchema)
		}

		schema["items"] = itemSchema
	}
}

// WithIntegerItems configures an array's items to be of type integer.
//
// Supported options: Description(), DefaultNumber(), Min(), Max(), MultipleOf()
// Note: Options like Required() are not valid for item schemas and will be ignored.
//
// Examples:
//
//	mcp.WithArray("ids", mcp.WithIntegerItems())
//	mcp.WithArray("scores", mcp.WithIntegerItems(mcp.Min(0), mcp.Max(100)))
//
// Limitations: Only supports simple integer arrays. Use Items() for complex objects.
func WithIntegerItems(opts ...PropertyOption) PropertyOption {
	return func(schema map[string]any) {
		itemSchema := map[string]any{
			"type": "integer",
		}

		for _, opt := range opts {
			opt(itemSchema)
		}

		if required, ok := itemSchema["required"].(bool); ok && required {
			delete(itemSchema, "required")
		}

		schema["items"] = itemSchema
	}
}

// WithBooleanItems configures an array's items to be of type boolean.
//
// Supported options: Description(), DefaultBool()
// Note: Options like Required() are not valid for item schemas and will be ignored.
//
// Examples:
//
//	mcp.WithArray("flags", mcp.WithBooleanItems())
//	mcp.WithArray("permissions", mcp.WithBooleanItems(mcp.Description("User permissions")))
//
// Limitations: Only supports simple boolean arrays. Use Items() for complex objects.
func WithBooleanItems(opts ...PropertyOption) PropertyOption {
	return func(schema map[string]any) {
		itemSchema := map[string]any{
			"type": "boolean",
		}

		for _, opt := range opts {
			opt(itemSchema)
		}

		schema["items"] = itemSchema
	}
}
