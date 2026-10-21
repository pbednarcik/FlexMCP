package mcp

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// valueDecoder is the pooled state of one fast decode of a _meta or of a tool
// call's arguments: a reader over the input, a decoder with encoding/json's
// own options on it, a buffer for strings that hold escapes, and the strings
// decoded before.
type valueDecoder struct {
	r   bytes.Reader
	dec *jsontext.Decoder
	unq []byte
	// strs holds strings this decoder read before, by their bytes: a client
	// sends the same ones on every request. It holds at most
	// maxKeptStrings, each at most maxKeptString bytes long, and
	// is cleared when full.
	strs map[string]keptString
}

// keptString is a string a decoder kept, and its boxed form once it was read
// as a value: a key is never boxed.
type keptString struct {
	s   string
	box any
}

const (
	// maxKeptString is the longest string, in bytes, a decoder keeps.
	maxKeptString = 64
	// maxKeptStrings is how many strings a decoder keeps before it
	// forgets them all.
	maxKeptStrings = 256
)

// maxFastInput bounds the input the single pass reads; a larger one goes
// to the slow decoder. A decoder's buffers grow with its input and go back to
// the pool as grown, so this also bounds what a pooled decoder keeps: none is
// kept that an input past it grew. A large input that fails at its end is not
// first built value by value.
const maxFastInput = 64 << 10

// metaDecoders keeps the decoders between requests, so a decode makes none.
var metaDecoders = sync.Pool{New: newValueDecoder}

// newValueDecoder makes a decoder for a pool, its string cache empty.
func newValueDecoder() any {
	d := &valueDecoder{strs: make(map[string]keptString, maxKeptStrings)}
	d.dec = jsontext.NewDecoder(&d.r, metaJSONOptions)
	return d
}

// metaConstStrings are the keys and values clients send on every request,
// boxed once: the well-known keys and the common keys inside them, the
// protocol versions and the log levels. A string read as one of them costs
// nothing.
var metaConstStrings = func() map[string]any {
	m := make(map[string]any)
	for _, s := range []string{
		MetaKeyProtocolVersion, MetaKeyClientInfo, MetaKeyClientCapabilities, MetaKeyServerInfo,
		MetaKeyLogLevel, MetaKeySubscriptionID, "progressToken",
		"name", "version", "title", "description", "websiteUrl", "icons", "src", "mimeType", "sizes", "theme",
		"roots", "listChanged", "sampling", "context", "tools", "elicitation", "form", "url", "tasks",
		"extensions", "experimental", "list", "cancel", "requests",
		ProtocolVersion20260728, ProtocolVersion20251125, ProtocolVersion20250618, ProtocolVersion20250326, ProtocolVersion20241105,
		string(LoggingLevelDebug), string(LoggingLevelInfo), string(LoggingLevelNotice), string(LoggingLevelWarning),
		string(LoggingLevelError), string(LoggingLevelCritical), string(LoggingLevelAlert), string(LoggingLevelEmergency),
	} {
		m[s] = s
	}
	return m
}()

// unmarshalFast reads data into m in one pass, as [Meta.unmarshalJSONSlow]
// would, and reports whether it could. It gives up, leaving m as it was, on
// any input it cannot settle exactly as encoding/json does: not an object,
// trailing data, a syntax error, invalid UTF-8 or an escape it cannot read, a
// number out of range, a key other than progressToken that folds to it, or
// an input longer than maxFastInput.
func (m *Meta) unmarshalFast(data []byte) bool {
	if len(data) > maxFastInput {
		return false
	}
	d := metaDecoders.Get().(*valueDecoder)
	d.r.Reset(data)
	d.dec.Reset(&d.r, metaJSONOptions)
	ok := d.meta(m)
	d.r.Reset(nil)
	metaDecoders.Put(d)
	return ok
}

// meta reads the top-level object into m, or reports false with m unchanged.
func (d *valueDecoder) meta(m *Meta) bool {
	if d.dec.PeekKind() != '{' {
		return false
	}
	if _, err := d.dec.ReadToken(); err != nil {
		return false
	}
	fields := make(map[string]any)
	var token ProgressToken
	for d.dec.PeekKind() != '}' {
		key, ok := d.name()
		if !ok {
			return false
		}
		if key == "progressToken" {
			if token, ok = d.progressToken(); !ok {
				return false
			}
			continue
		}
		// The slow path re-reads a numeric token by a case-insensitive
		// match, which such a key would take part in.
		if strings.EqualFold(key, "progressToken") {
			return false
		}
		value, ok := d.value()
		if !ok {
			return false
		}
		fields[key] = value
	}
	if _, err := d.dec.ReadToken(); err != nil {
		return false
	}
	if !d.end() {
		return false
	}
	m.ProgressToken = token
	m.AdditionalFields = fields
	return true
}

// end reports whether the input ends after the value read.
func (d *valueDecoder) end() bool {
	_, err := d.dec.ReadToken()
	return errors.Is(err, io.EOF)
}

// progressToken reads the progress token as the slow path keeps it: a number
// as [decodeProgressToken] reads it, anything else as any other value.
func (d *valueDecoder) progressToken() (ProgressToken, bool) {
	if d.dec.PeekKind() != '0' {
		return d.value()
	}
	v, err := d.dec.ReadValue()
	if err != nil {
		return nil, false
	}
	// A number out of a float64's range fails the slow path's decode.
	if _, err := strconv.ParseFloat(string(v), 64); err != nil {
		return nil, false
	}
	token, err := decodeProgressToken(v)
	if err != nil {
		return nil, false
	}
	return token, true
}

// value reads the next value as json.Unmarshal into an any reads it.
func (d *valueDecoder) value() (any, bool) {
	switch d.dec.PeekKind() {
	case '"':
		v, err := d.dec.ReadValue()
		if err != nil {
			return nil, false
		}
		s, boxed, ok := d.text(v, true)
		if !ok {
			return nil, false
		}
		if boxed != nil {
			return boxed, true
		}
		return s, true
	case '0':
		v, err := d.dec.ReadValue()
		if err != nil {
			return nil, false
		}
		f, err := strconv.ParseFloat(string(v), 64)
		if err != nil {
			return nil, false
		}
		return f, true
	case 't', 'f':
		tok, err := d.dec.ReadToken()
		if err != nil {
			return nil, false
		}
		return tok.Bool(), true
	case 'n':
		if _, err := d.dec.ReadToken(); err != nil {
			return nil, false
		}
		return nil, true
	case '{':
		return d.object()
	case '[':
		return d.array()
	}
	return nil, false
}

// object reads a nested object into a map of its own.
func (d *valueDecoder) object() (any, bool) {
	if _, err := d.dec.ReadToken(); err != nil {
		return nil, false
	}
	m := make(map[string]any)
	for d.dec.PeekKind() != '}' {
		key, ok := d.name()
		if !ok {
			return nil, false
		}
		value, ok := d.value()
		if !ok {
			return nil, false
		}
		m[key] = value
	}
	if _, err := d.dec.ReadToken(); err != nil {
		return nil, false
	}
	return m, true
}

// array reads an array into a slice of its own, an empty one not nil.
func (d *valueDecoder) array() (any, bool) {
	if _, err := d.dec.ReadToken(); err != nil {
		return nil, false
	}
	a := []any{}
	for d.dec.PeekKind() != ']' {
		value, ok := d.value()
		if !ok {
			return nil, false
		}
		a = append(a, value)
	}
	if _, err := d.dec.ReadToken(); err != nil {
		return nil, false
	}
	return a, true
}

// name reads an object's next key. A key is kept as a value is, unboxed: a
// client repeats its keys too, and one kept costs nothing on the next
// request.
func (d *valueDecoder) name() (string, bool) {
	v, err := d.dec.ReadValue()
	if err != nil {
		return "", false
	}
	s, _, ok := d.text(v, false)
	return s, ok
}

// text returns the string the JSON string literal v holds and, when it is one
// of [metaConstStrings] or a string kept and read as a value, its boxed form;
// false where the slow path should decide. A string kept is looked up by its
// bytes, so it is always the string read; it is boxed the first time it is
// read as a value.
func (d *valueDecoder) text(v jsontext.Value, value bool) (s string, boxed any, ok bool) {
	body := []byte(v[1 : len(v)-1])
	if bytes.IndexByte(body, '\\') >= 0 {
		var err error
		if d.unq, err = jsontext.AppendUnquote(d.unq[:0], v); err != nil {
			return "", nil, false
		}
		body = d.unq
	} else if !utf8.Valid(body) {
		return "", nil, false
	}
	if c, ok := metaConstStrings[string(body)]; ok {
		return c.(string), c, true
	}
	if len(body) > maxKeptString {
		return string(body), nil, true
	}
	kept, ok := d.strs[string(body)]
	if ok && (kept.box != nil || !value) {
		return kept.s, kept.box, true
	}
	if !ok {
		if len(d.strs) >= maxKeptStrings {
			clear(d.strs)
		}
		kept.s = string(body)
	}
	if value {
		kept.box = kept.s
	}
	d.strs[kept.s] = kept
	return kept.s, kept.box, true
}
