package server

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
)

// headerBinding is an x-mcp-header binding as registration keeps it: with the
// name of its header, as an error names it, and the key [http.Header] files
// that header under, so a call builds neither.
type headerBinding struct {
	mcp.ParamHeaderBinding
	name string
	key  string
}

// keepHeaderBindings extracts a tool's x-mcp-header bindings as the server
// keeps them.
func keepHeaderBindings(tool *mcp.Tool) []headerBinding {
	bindings := mcp.ExtractParamHeaderBindings(tool)
	if len(bindings) == 0 {
		return nil
	}
	kept := make([]headerBinding, len(bindings))
	for i, binding := range bindings {
		name := binding.HeaderName()
		kept[i] = headerBinding{ParamHeaderBinding: binding, name: name, key: textproto.CanonicalMIMEHeaderKey(name)}
	}
	return kept
}

// checkParamHeaders checks the Mcp-Param-* headers of a tools/call against
// the arguments in its message, as [mcp.ValidateParamHeadersWithBindings]
// checks them against the params json.Unmarshal reads from the message. It
// reads the message once, keeping only where each bound value lies; a message
// it cannot settle exactly as encoding/json reads it takes that slow path.
func checkParamHeaders(headers http.Header, bindings []headerBinding, message []byte) error {
	if len(bindings) == 0 {
		return nil
	}
	w := paramHeaderWalkers.Get().(*paramHeaderWalker)
	settled, err := w.check(headers, bindings, message)
	w.finish()
	paramHeaderWalkers.Put(w)
	if settled {
		return err
	}
	return checkParamHeadersSlow(headers, bindings, message)
}

// checkParamHeadersSlow is [checkParamHeaders] by encoding/json: the params
// of the message decoded and handed to mcp's own check.
func checkParamHeadersSlow(headers http.Header, bindings []headerBinding, message []byte) error {
	var wrapper struct {
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(message, &wrapper); err != nil {
		return nil
	}
	plain := make([]mcp.ParamHeaderBinding, len(bindings))
	for i := range bindings {
		plain[i] = bindings[i].ParamHeaderBinding
	}
	return mcp.ValidateParamHeadersWithBindings(lookupHeader(headers), plain, wrapper.Params)
}

// paramHeaderWalker is the pooled state of one header check: the message as a
// bytes.Buffer, which jsontext reads in place, the decoder, a buffer for a
// string that holds escapes, and where each binding's value lies.
type paramHeaderWalker struct {
	in    bytes.Buffer
	dec   *jsontext.Decoder
	unq   []byte
	found []valueSpan
}

// valueSpan is where a value lies in the message; end is 0 for none.
type valueSpan struct{ start, end int }

// paramHeaderOptions are encoding/json's own but for invalid UTF-8, which
// json.Unmarshal replaces and the walk leaves to it.
var paramHeaderOptions = jsonv2.JoinOptions(json.DefaultOptionsV1(), jsontext.AllowInvalidUTF8(false))

var paramHeaderWalkers = sync.Pool{New: func() any {
	w := &paramHeaderWalker{}
	w.dec = jsontext.NewDecoder(&w.in, paramHeaderOptions)
	return w
}}

const (
	// maxWalkBindings is the most bindings the walk tracks, one bit each.
	maxWalkBindings = 64
	// maxKeptUnquote bounds the escape buffer a pooled walker keeps.
	maxKeptUnquote = 64 << 10
	// maxSafeInteger bounds the integers a header carries, as mcp's own
	// check does: those a float64 holds exactly.
	maxSafeInteger = 1<<53 - 1
)

// check walks message and, when it settles it, compares each binding's
// value with its header.
func (w *paramHeaderWalker) check(headers http.Header, bindings []headerBinding, message []byte) (settled bool, err error) {
	if len(bindings) > maxWalkBindings {
		return false, nil
	}
	w.in = *bytes.NewBuffer(message)
	w.dec.Reset(&w.in, paramHeaderOptions)
	if cap(w.found) < len(bindings) {
		w.found = make([]valueSpan, len(bindings))
	}
	w.found = w.found[:len(bindings)]
	clear(w.found)
	hasArgs, ok := w.message(bindings)
	if !ok {
		return false, nil
	}
	if !hasArgs {
		return true, nil
	}
	return true, w.compare(headers, bindings, message)
}

// finish lets go of the message and of an escape buffer grown past
// maxKeptUnquote.
func (w *paramHeaderWalker) finish() {
	w.in = bytes.Buffer{}
	w.dec.Reset(&w.in, paramHeaderOptions)
	if cap(w.unq) > maxKeptUnquote {
		w.unq = nil
	}
}

// message reads the whole message for its params member, reporting whether
// params holds an arguments object; ok is false where the slow path must
// decide.
func (w *paramHeaderWalker) message(bindings []headerBinding) (hasArgs, ok bool) {
	d := w.dec
	if d.PeekKind() != '{' {
		return false, false
	}
	if _, err := d.ReadToken(); err != nil {
		return false, false
	}
	seen := false
	for d.PeekKind() != '}' {
		params, ok := w.member("params")
		if !ok {
			return false, false
		}
		if params {
			if seen {
				// json.Unmarshal keeps the last of two params; the slow
				// path decides that.
				return false, false
			}
			seen = true
			if d.PeekKind() == '{' {
				if hasArgs, ok = w.params(bindings); !ok {
					return false, false
				}
				continue
			}
		}
		if d.SkipValue() != nil {
			return false, false
		}
	}
	if _, err := d.ReadToken(); err != nil {
		return false, false
	}
	_, err := d.ReadToken()
	return hasArgs, errors.Is(err, io.EOF)
}

// params reads a params object for its arguments member.
func (w *paramHeaderWalker) params(bindings []headerBinding) (hasArgs, ok bool) {
	d := w.dec
	if _, err := d.ReadToken(); err != nil {
		return false, false
	}
	seen := false
	for d.PeekKind() != '}' {
		arguments, ok := w.member("arguments")
		if !ok {
			return false, false
		}
		if arguments && seen {
			// json.Unmarshal merges a second arguments object into the
			// first; the slow path decides that.
			return false, false
		}
		seen = seen || arguments
		if !arguments || d.PeekKind() != '{' {
			if d.SkipValue() != nil {
				return false, false
			}
			continue
		}
		var set uint64
		for i := range bindings {
			if len(bindings[i].Path) > 0 {
				set |= 1 << i
			}
		}
		if !w.object(bindings, 0, set) {
			return false, false
		}
		hasArgs = true
	}
	_, err := d.ReadToken()
	return hasArgs, err == nil
}

// object reads an object at depth on the bound paths in set, recording where
// each bound value lies. A member named twice replaces what the first gave,
// as json.Unmarshal keeps the last.
func (w *paramHeaderWalker) object(bindings []headerBinding, depth int, set uint64) bool {
	d := w.dec
	if _, err := d.ReadToken(); err != nil {
		return false
	}
	for d.PeekKind() != '}' {
		name, ok := w.name()
		if !ok {
			return false
		}
		var leaves, inner uint64
		for rest := set; rest != 0; rest &= rest - 1 {
			i := bits.TrailingZeros64(rest)
			path := bindings[i].Path
			if path[depth] != string(name) {
				continue
			}
			if len(path) == depth+1 {
				leaves |= 1 << i
			} else {
				inner |= 1 << i
			}
		}
		if leaves|inner == 0 {
			if d.SkipValue() != nil {
				return false
			}
			continue
		}
		for rest := leaves | inner; rest != 0; rest &= rest - 1 {
			w.found[bits.TrailingZeros64(rest)] = valueSpan{}
		}
		switch {
		case leaves != 0 && inner != 0:
			// One binding's value is the parent of another's.
			return false
		case leaves != 0:
			v, err := d.ReadValue()
			if err != nil {
				return false
			}
			if v.Kind() == '"' {
				if _, ok := w.unquote(v); !ok {
					return false
				}
			}
			end := int(d.InputOffset())
			span := valueSpan{start: end - len(v), end: end}
			for rest := leaves; rest != 0; rest &= rest - 1 {
				w.found[bits.TrailingZeros64(rest)] = span
			}
		case d.PeekKind() == '{':
			if !w.object(bindings, depth+1, inner) {
				return false
			}
		default:
			if d.SkipValue() != nil {
				return false
			}
		}
	}
	_, err := d.ReadToken()
	return err == nil
}

// member reads an object's next name and reports whether it is target. ok is
// false for a name json.Unmarshal might match to target although it is not
// target, by its case-insensitive match.
func (w *paramHeaderWalker) member(target string) (is, ok bool) {
	name, ok := w.name()
	if !ok {
		return false, false
	}
	if string(name) == target {
		return true, true
	}
	return false, !mayFoldTo(name, target)
}

// mayFoldTo reports whether name could match the lower-case ASCII target
// case-insensitively. It errs on the side of yes: any name with a non-ASCII
// rune in a place that could match, or equal to target but for ASCII case,
// dashes and underscores.
func mayFoldTo(name []byte, target string) bool {
	i := 0
	for _, c := range name {
		if c >= utf8.RuneSelf {
			return true
		}
		if c == '-' || c == '_' {
			continue
		}
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if i >= len(target) || c != target[i] {
			return false
		}
		i++
	}
	return i == len(target)
}

// name reads an object's next name, unquoted.
func (w *paramHeaderWalker) name() ([]byte, bool) {
	v, err := w.dec.ReadValue()
	if err != nil {
		return nil, false
	}
	return w.unquote(v)
}

// unquote returns the text of the string literal v, in w.unq when v holds an
// escape.
func (w *paramHeaderWalker) unquote(v []byte) ([]byte, bool) {
	body := v[1 : len(v)-1]
	if bytes.IndexByte(body, '\\') < 0 {
		return body, true
	}
	var err error
	if w.unq, err = jsontext.AppendUnquote(w.unq[:0], v); err != nil {
		return nil, false
	}
	return w.unq, true
}

// compare checks each binding's value in message against its header, in
// order, with mcp's own rules and errors.
func (w *paramHeaderWalker) compare(headers http.Header, bindings []headerBinding, message []byte) error {
	for i := range bindings {
		binding := &bindings[i]
		values := headers[binding.key]
		var headerValue string
		if len(values) > 0 {
			headerValue = values[0]
		}
		span := w.found[i]
		raw := message[span.start:span.end]
		if span.end == 0 || string(raw) == "null" {
			if headerValue != "" {
				return mcp.HeaderMismatchError{
					Header: binding.name,
					Reason: fmt.Sprintf("unexpected header for absent or null parameter %q", strings.Join(binding.Path, ".")),
				}
			}
			continue
		}
		if len(values) == 0 {
			return mcp.HeaderMismatchError{
				Header: binding.name,
				Reason: fmt.Sprintf("missing header for parameter %q", strings.Join(binding.Path, ".")),
			}
		}
		decoded, ok := mcp.DecodeHeaderValue(headerValue)
		if !ok {
			return mcp.HeaderMismatchError{Header: binding.name, Reason: "invalid Base64 encoding"}
		}
		equal, primitive := w.equal(decoded, raw)
		if !primitive {
			return mcp.HeaderMismatchError{
				Header: binding.name,
				Reason: fmt.Sprintf("body parameter %q is not a primitive type", strings.Join(binding.Path, ".")),
			}
		}
		if !equal {
			return mcp.HeaderMismatchError{
				Header: binding.name,
				Reason: fmt.Sprintf("header value %q does not match body value", headerValue),
			}
		}
	}
	return nil
}

// equal reports what mcp's own check finds for a decoded header and the body
// value raw: whether raw is a string, a boolean or a safe integer, and
// whether the header carries it. An integer compares numerically.
func (w *paramHeaderWalker) equal(header string, raw []byte) (equal, primitive bool) {
	switch raw[0] {
	case '"':
		body, ok := w.unquote(raw)
		return ok && header == string(body), ok
	case 't', 'f':
		return header == string(raw), true
	case '{', '[':
		return false, false
	}
	body, err := strconv.ParseFloat(string(raw), 64)
	if err != nil || !safeInteger(body) {
		return false, false
	}
	value, err := strconv.ParseFloat(header, 64)
	if err != nil || !safeInteger(value) {
		return false, true
	}
	return int64(value) == int64(body), true
}

// safeInteger reports whether f is an integer a float64 holds exactly.
func safeInteger(f float64) bool {
	return f == math.Trunc(f) && f >= -maxSafeInteger && f <= maxSafeInteger
}
