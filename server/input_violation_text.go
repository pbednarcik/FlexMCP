package server

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"golang.org/x/text/message"
)

// The pieces of a violation the hook sees are written within fixed bounds:
// each writer stops at its cap, so no piece is built whole and then cut.

// maxBoundBits is the size, in bits, the numerator and the denominator of a
// bound may have for boundText to render it exactly.
const maxBoundBits = 256

// maxBoundBytes is the most an exact bound may take as text.
const maxBoundBytes = 256

// maxBoundFives is the most divisions by 5 boundText makes: 5^110 is the
// largest power of five that fits in maxBoundBits.
const maxBoundFives = 110

// cappedWriter collects text up to a byte limit. A write that does not fit
// is written as far as it does, cut on a UTF-8 boundary, and the writer takes
// nothing more.
type cappedWriter struct {
	buf   []byte
	limit int
	cut   bool
}

// newCappedWriter returns a writer that holds at most limit bytes.
func newCappedWriter(limit int) *cappedWriter {
	limit = max(limit, 0)
	return &cappedWriter{buf: make([]byte, 0, min(limit, 128)), limit: limit}
}

// WriteString appends s, or as much of it as fits, cut at a UTF-8 boundary.
// It reports whether all of s was written; once it reports false the writer
// is cut and every later write reports false too.
func (w *cappedWriter) WriteString(s string) bool {
	if w.cut {
		return false
	}
	room := w.limit - len(w.buf)
	if len(s) <= room {
		w.buf = append(w.buf, s...)
		return true
	}
	w.buf = append(w.buf, s[:cutBoundary(s, room)]...)
	w.cut = true
	return false
}

// String returns the text written so far.
func (w *cappedWriter) String() string { return string(w.buf) }

// Cut reports whether a write did not fit.
func (w *cappedWriter) Cut() bool { return w.cut }

// room is how many more bytes the writer takes.
func (w *cappedWriter) room() int { return w.limit - len(w.buf) }

// markCut stops the writer without a write that did not fit: a budget other
// than its bytes ran out.
func (w *cappedWriter) markCut() { w.cut = true }

// cutBoundary returns the greatest n' <= n at which s can be cut without
// splitting a character, n' = len(s) when s is no longer than n.
func cutBoundary[T ~string | ~[]byte](s T, n int) int {
	n = min(n, len(s))
	for n > 0 && n < len(s) && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

// pastLine reports whether w holds more than a line can show, or is cut:
// the pieces of a text that grows with a request stop here. Past a line it
// cuts w, so what the text goes on with is not written after the gap.
func pastLine(w *cappedWriter) bool {
	if len(w.buf) > maxValidationMessageBytes {
		w.markCut()
	}
	return w.cut
}

// cappedValidationLine renders one failure as validationLine does, without
// building its pointer or its text whole. text writes the failure's reason
// into the writer it is given, which holds a line's worth and a character.
// The pointer is escaped and capped as it is written, to what the text leaves
// it, and the bytes equal validationLine(at, wholeText) whenever the text
// fits the writer or is longer than the line.
func cappedValidationLine(at []string, text func(w *cappedWriter)) string {
	tw := newCappedWriter(maxValidationMessageBytes + utf8.UTFMax)
	text(tw)
	body := tw.buf
	room := max(maxValidationMessageBytes-len("...: ")-len(body), minShownLocation)
	var out []byte
	cutLocation := false
	if len(at) == 0 {
		out = append(out, "<root>"...)
	} else {
		// The pointer is whole when the line fits it, and otherwise cut to
		// room: so it is written up to the longer of the two.
		lw := newCappedWriter(max(maxValidationMessageBytes-len(": ")-len(body), room))
		writePointer(lw, at)
		location := lw.buf
		if lw.Cut() || (len(location)+len(": ")+len(body) > maxValidationMessageBytes && len(location) > room) {
			location = location[:cutBoundary(location, room)]
			cutLocation = true
		}
		out = append(out, location...)
	}
	if cutLocation {
		out = append(out, "...: "...)
	} else {
		out = append(out, ": "...)
	}
	out = append(out, body...)
	if len(out) > maxValidationMessageBytes {
		out = append(out[:cutBoundary(out, maxValidationMessageBytes)], "..."...)
	}
	return string(out)
}

// writePointer writes at as a JSON Pointer, as jsonPointer renders it, until
// w is full.
func writePointer(w *cappedWriter, at []string) {
	for _, segment := range at {
		if !w.WriteString("/") || !writeEscapedSegment(w, segment) {
			return
		}
	}
}

// writeEscapedSegment writes segment with its "~" and "/" escaped, as
// escapeJSONPointer does, and stops with w. It looks at no more of segment
// than w has room for.
func writeEscapedSegment(w *cappedWriter, segment string) bool {
	start := 0
	for i := 0; i < len(segment); i++ {
		if i-start > w.room() {
			w.WriteString(segment[start:i])
			return false
		}
		var escape string
		switch segment[i] {
		case '~':
			escape = "~0"
		case '/':
			escape = "~1"
		default:
			continue
		}
		if !w.WriteString(segment[start:i]) || !w.WriteString(escape) {
			return false
		}
		start = i + 1
	}
	return w.WriteString(segment[start:])
}

// kindText writes the English reason for k as k.LocalizedString(validationMessages)
// reads it. The kinds whose text grows with the request or the schema (Enum,
// Const, Required, Dependency, DependentRequired, AdditionalProperties,
// Pattern, Format, MinContains and MaxContains) are written piece by piece in
// kind.go's format, the small pieces through validationMessages, and stop at
// w's limit and past a line's worth; every other kind's text is small and
// comes from LocalizedString. Format's Err is written only when the value's
// display left room for it.
func kindText(k jsonschema.ErrorKind, w *cappedWriter) {
	switch k := k.(type) {
	case *kind.Enum:
		enumText(k, w)
	case *kind.Const:
		switch k.Want.(type) {
		case []any, map[string]any:
			w.WriteString("'const' failed")
		default:
			w.WriteString("value must be ")
			writeDisplay(w, k.Want)
		}
	case *kind.Required:
		if len(k.Missing) == 1 {
			w.WriteString("missing property ")
			writeQuoted(w, k.Missing[0])
		} else {
			w.WriteString("missing properties ")
			writeJoinQuoted(w, k.Missing)
		}
	case *kind.Dependency:
		requiredIfText(w, k.Missing, k.Prop)
	case *kind.DependentRequired:
		requiredIfText(w, k.Missing, k.Prop)
	case *kind.AdditionalProperties:
		w.WriteString("additional properties ")
		writeJoinQuoted(w, k.Properties)
		w.WriteString(" not allowed")
	case *kind.Pattern:
		writeQuoted(w, k.Got)
		w.WriteString(" does not match pattern ")
		writeQuoted(w, k.Want)
	case *kind.Format:
		writeDisplay(w, k.Got)
		w.WriteString(" is not valid ")
		w.WriteString(k.Want)
		w.WriteString(": ")
		if k.Err != nil && !w.Cut() {
			if w.room() > 0 {
				w.WriteString(localizedErrorText(k.Err))
			} else {
				// The value filled the writer to the byte: the err is dropped.
				w.markCut()
			}
		}
	case *kind.MinContains:
		if len(k.Got) == 0 {
			w.WriteString(validationMessages.Sprintf("min %d items required to match contains schema, but none matched", k.Want))
			return
		}
		w.WriteString(validationMessages.Sprintf("min %d items required to match contains schema, but matched %d items at ", k.Want, len(k.Got)))
		writeIndexes(w, k.Got)
	case *kind.MaxContains:
		w.WriteString(validationMessages.Sprintf("max %d items required to match contains schema, but matched %d items at ", k.Want, len(k.Got)))
		writeIndexes(w, k.Got)
	default:
		w.WriteString(k.LocalizedString(validationMessages))
	}
}

// enumText writes kind.Enum's text: the values when all are primitives, as
// display shows them, and "'enum' failed" when one is a container.
func enumText(k *kind.Enum, w *cappedWriter) {
	for _, item := range k.Want {
		switch item.(type) {
		case []any, map[string]any:
			w.WriteString("'enum' failed")
			return
		}
	}
	if len(k.Want) == 1 {
		w.WriteString("value must be ")
		writeDisplay(w, k.Want[0])
		return
	}
	w.WriteString("value must be one of ")
	for i, item := range k.Want {
		if pastLine(w) {
			return
		}
		if i > 0 {
			w.WriteString(", ")
		}
		writeDisplay(w, item)
	}
}

// requiredIfText writes the text of kind.Dependency and kind.DependentRequired.
func requiredIfText(w *cappedWriter, missing []string, prop string) {
	w.WriteString("properties ")
	writeJoinQuoted(w, missing)
	w.WriteString(" required, if ")
	writeQuoted(w, prop)
	w.WriteString(" exists")
}

// writeIndexes writes indexes as fmt writes an []int inside its brackets:
// separated by single spaces.
func writeIndexes(w *cappedWriter, indexes []int) {
	var scratch [24]byte
	for i, index := range indexes {
		if pastLine(w) {
			return
		}
		if i > 0 {
			w.WriteString(" ")
		}
		w.WriteString(string(strconv.AppendInt(scratch[:0], int64(index), 10)))
	}
}

// writeJoinQuoted writes names quoted and joined by ", ", as kind.go's
// joinQuoted does.
func writeJoinQuoted(w *cappedWriter, names []string) {
	for i, name := range names {
		if pastLine(w) {
			return
		}
		if i > 0 {
			w.WriteString(", ")
		}
		writeQuoted(w, name)
	}
}

// writeQuoted writes s as kind.go's quote does: in single quotes, with the
// escapes strconv uses for a double-quoted string, except that a double quote
// stands unescaped and a single quote is escaped. It looks at no more of s
// than w has room for.
func writeQuoted(w *cappedWriter, s string) bool {
	if !w.WriteString("'") {
		return false
	}
	const hex = "0123456789abcdef"
	var scratch [16]byte
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		var piece []byte
		if r == utf8.RuneError && size == 1 {
			piece = append(scratch[:0], '\\', 'x', hex[s[i]>>4], hex[s[i]&15])
		} else {
			piece = strconv.AppendQuoteRune(scratch[:0], r)
			piece = piece[1 : len(piece)-1]
		}
		if !w.WriteString(string(piece)) {
			return false
		}
		i += size
	}
	return w.WriteString("'")
}

// writeDisplay writes v as kind.go's display does: a string quoted, a
// container as "value", anything else as fmt prints it.
func writeDisplay(w *cappedWriter, v any) bool {
	switch v := v.(type) {
	case string:
		return writeQuoted(w, v)
	case []any, map[string]any:
		return w.WriteString("value")
	case json.Number:
		return w.WriteString(string(v))
	default:
		return w.WriteString(fmt.Sprintf("%v", v))
	}
}

// localizedErrorText is the text of err in the printer's words, as kind.go's
// localizedError reads it.
func localizedErrorText(err error) string {
	if localized, ok := err.(interface{ LocalizedError(*message.Printer) string }); ok {
		return localized.LocalizedError(validationMessages)
	}
	return err.Error()
}

// previewBudget is what is left of a list's previews: the values still to be
// written and the bytes they may still take.
type previewBudget struct {
	nodes, bytes int
}

// previewer writes one preview.
type previewer struct {
	w      *cappedWriter
	budget *previewBudget
}

// preview writes v as JSON text of at most limit bytes and at most what
// budget has left in bytes, spending one node of the budget for each value it
// writes: a scalar, an array, an object and each of their members. An
// object's members come in key order, the least keys chosen by a bounded
// selection. At the first budget it reaches it stops, and the text, cut, is a
// prefix of the JSON, not JSON. A nil budget is no budget but limit.
//
// Numbers read as they are: a json.Number as its literal, a float64 as its
// shortest form. A value of any other type than a request or a schema decodes
// to (nil, bool, string, json.Number, float64, []any and map[string]any) is
// not written: the preview is cut.
func preview(v any, limit int, budget *previewBudget) (text string, cut bool) {
	if budget == nil {
		budget = &previewBudget{nodes: math.MaxInt, bytes: math.MaxInt}
	}
	p := previewer{w: newCappedWriter(min(limit, budget.bytes)), budget: budget}
	p.value(v)
	budget.bytes -= len(p.w.buf)
	return p.w.String(), p.w.Cut()
}

// spend takes a node of the budget, or cuts the preview.
func (p *previewer) spend() bool {
	if p.budget.nodes <= 0 {
		p.w.markCut()
		return false
	}
	p.budget.nodes--
	return true
}

// value writes v, one node.
func (p *previewer) value(v any) bool {
	return p.spend() && p.body(v)
}

// body writes v, its node already spent.
func (p *previewer) body(v any) bool {
	w := p.w
	switch v := v.(type) {
	case nil:
		return w.WriteString("null")
	case bool:
		return w.WriteString(strconv.FormatBool(v))
	case string:
		return writeJSONString(w, v)
	case json.Number:
		return w.WriteString(string(v))
	case float64:
		return w.WriteString(string(appendJSONFloat(make([]byte, 0, 32), v)))
	case []any:
		return p.array(v)
	case map[string]any:
		return p.object(v)
	default:
		// No decode of a request or a schema makes another type.
		w.markCut()
		return false
	}
}

// array writes items, a node each, until a budget runs out.
func (p *previewer) array(items []any) bool {
	if !p.w.WriteString("[") {
		return false
	}
	for i, item := range items {
		if !p.spend() {
			return false
		}
		if i > 0 && !p.w.WriteString(",") {
			return false
		}
		if !p.body(item) {
			return false
		}
	}
	return p.w.WriteString("]")
}

// object writes the least keys of members that the budgets leave room for,
// in key order, each with its value. If keys are left out, the preview is
// cut after the last one written.
func (p *previewer) object(members map[string]any) bool {
	if !p.w.WriteString("{") {
		return false
	}
	// A member takes a node and four bytes at the least: "":0
	keys := leastKeys(members, min(len(members), p.budget.nodes, p.w.room()/4+1))
	for i, key := range keys {
		if i > 0 && !p.w.WriteString(",") {
			return false
		}
		if !writeJSONString(p.w, key) || !p.w.WriteString(":") || !p.value(members[key]) {
			return false
		}
	}
	if len(keys) < len(members) {
		p.w.markCut()
		return false
	}
	return p.w.WriteString("}")
}

// leastKeys returns the n least keys of members, in order, from one scan of
// them that keeps at most n.
func leastKeys(members map[string]any, n int) []string {
	if n <= 0 {
		return nil
	}
	keys := make([]string, 0, n)
	if n >= len(members) {
		for key := range members {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		return keys
	}
	for key := range members {
		if len(keys) == n {
			if key >= keys[n-1] {
				continue
			}
			keys = keys[:n-1]
		}
		at, _ := slices.BinarySearch(keys, key)
		keys = slices.Insert(keys, at, key)
	}
	return keys
}

// writeJSONString writes s as a JSON string, its closing quote only if all of
// it fit. It looks at no more of s than w has room for.
func writeJSONString(w *cappedWriter, s string) bool {
	if !w.WriteString(`"`) {
		return false
	}
	const hex = "0123456789abcdef"
	start := 0
	for i := 0; i < len(s); {
		if i-start > w.room() {
			w.WriteString(s[start:i])
			return false
		}
		c := s[i]
		var escape string
		switch {
		case c == '"':
			escape = `\"`
		case c == '\\':
			escape = `\\`
		case c == '\n':
			escape = `\n`
		case c == '\r':
			escape = `\r`
		case c == '\t':
			escape = `\t`
		case c < 0x20:
			escape = string([]byte{'\\', 'u', '0', '0', hex[c>>4], hex[c&15]})
		case c < utf8.RuneSelf:
			i++
			continue
		default:
			r, n := utf8.DecodeRuneInString(s[i:])
			if r != utf8.RuneError || n != 1 {
				i += n
				continue
			}
			escape = "\xef\xbf\xbd"
		}
		if !w.WriteString(s[start:i]) || !w.WriteString(escape) {
			return false
		}
		i++
		start = i
	}
	return w.WriteString(s[start:]) && w.WriteString(`"`)
}

// appendJSONFloat appends f in the shortest form that reads back as f, written
// as encoding/json writes it: an exponent only below 1e-6 and from 1e21.
func appendJSONFloat(b []byte, f float64) []byte {
	format := byte('f')
	if abs := math.Abs(f); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	b = strconv.AppendFloat(b, f, format, -1, 64)
	if format == 'e' {
		// Clean up e-09 to e-9.
		if n := len(b); n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
			b[n-2] = b[n-1]
			b = b[:n-1]
		}
	}
	return b
}

// boundText renders a numeric bound. When its numerator and denominator each
// fit in 256 bits and the denominator is 2^a*5^b (b found in at most 110
// divisions by 5), the bound has a finite decimal and the text is exact: the
// integer, or the decimal with max(a, b) places. If that is longer than 256
// bytes, or the bound is any other number, the text is the shortest form of
// its float64 and exact is false. The exact path does its arithmetic on numbers
// of at most 256 bits; the fallback reads r through Float64, whatever its size.
func boundText(r *big.Rat) (text string, exact bool) {
	if text, ok := exactBoundText(r); ok {
		return text, true
	}
	f, _ := r.Float64()
	return string(appendJSONFloat(nil, f)), false
}

// exactBoundText is the exact text of r, false when boundText's rule gives none.
func exactBoundText(r *big.Rat) (string, bool) {
	num, den := r.Num(), r.Denom()
	if num.BitLen() > maxBoundBits || den.BitLen() > maxBoundBits {
		return "", false
	}
	twos := den.TrailingZeroBits()
	rest := new(big.Int).Rsh(den, twos)
	five, quotient, remainder := big.NewInt(5), new(big.Int), new(big.Int)
	fives := 0
	for fives < maxBoundFives {
		quotient.QuoRem(rest, five, remainder)
		if remainder.Sign() != 0 {
			break
		}
		rest, quotient = quotient, rest
		fives++
	}
	if !rest.IsInt64() || rest.Int64() != 1 {
		return "", false
	}
	var text string
	if r.IsInt() {
		text = num.String()
	} else {
		text = r.FloatString(int(max(twos, uint(fives))))
	}
	if len(text) > maxBoundBytes {
		return "", false
	}
	return text, true
}
