// Package argsdecode carries mcp's one-pass decoder of a tool call's
// arguments to the server's input validation, so that neither package
// exports a name for it.
package argsdecode

// Numbers reads data, a tool call's arguments, in one pass into the value
// jsonschema.UnmarshalJSON builds of it, every number a json.Number of its
// literal as sent, and reports whether it could; when it could not, the
// caller decodes data itself. Package mcp sets it when it is initialised,
// before any package that imports mcp uses it.
var Numbers func(data []byte) (any, bool)
