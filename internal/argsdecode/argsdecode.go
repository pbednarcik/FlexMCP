// Package argsdecode carries what the server needs of mcp's decoding of a
// tool call's arguments, so that neither package exports a name for it: the
// one-pass decoder input validation reads them with, and the way to decode a
// call whose tool reads its arguments on demand.
package argsdecode

// Numbers reads data, a tool call's arguments, in one pass into the value
// jsonschema.UnmarshalJSON builds of it, every number a json.Number of its
// literal as sent, and reports whether it could; when it could not, the
// caller decodes data itself. Package mcp sets it when it is initialised,
// before any package that imports mcp uses it.
var Numbers func(data []byte) (any, bool)

// SetOnDemand sets on params, a *mcp.CallToolParams about to be decoded, the
// function its decode asks whether the tool it names reads its arguments on
// demand; nil, after the decode, takes it away again. Package mcp sets it
// when it is initialised.
var SetOnDemand func(params any, onDemand func(name string) bool)

// AtOnce sets on params, a decoded *mcp.CallToolParams whose decode left its
// arguments to the getters, the Arguments a call decoded at once carries; it
// leaves any other params as they are. Package mcp sets it when it is
// initialised.
var AtOnce func(params any)
