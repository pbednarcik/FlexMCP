package mcp

import "sync"

// argumentDecoders keeps the decoders that read a tool call's arguments
// between requests. They are not [metaDecoders]: argument values are new on
// each call more often than a _meta's, and in a decoder of their own they do
// not push out the strings a client repeats in every _meta.
var argumentDecoders = sync.Pool{New: newValueDecoder}

// decodeArgumentsFast reads data, a tool call's arguments, in one pass into
// the value json.Unmarshal into an any builds, and reports whether it could.
// It gives up on any input it cannot settle exactly as encoding/json does:
// trailing data, a syntax error, nesting past the decoder's limit, invalid
// UTF-8 or an escape it cannot read, a number out of range, or an input
// longer than maxFastInput.
func decodeArgumentsFast(data []byte) (any, bool) {
	if len(data) > maxFastInput {
		return nil, false
	}
	d := argumentDecoders.Get().(*valueDecoder)
	value, ok := d.decodeArguments(data)
	argumentDecoders.Put(d)
	return value, ok
}

// decodeArguments reads data as decodeArgumentsFast does, with d, and lets go
// of data whether or not it could.
func (d *valueDecoder) decodeArguments(data []byte) (any, bool) {
	d.start(data)
	value, ok := d.value()
	ok = ok && d.end()
	d.finish()
	if !ok {
		return nil, false
	}
	return value, true
}
