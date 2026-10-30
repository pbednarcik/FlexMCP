package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
	"unsafe"
)

// The reasons RequireWholeInt and RequireWholeIntText give for refusing an
// argument. They are carried by an *ArgumentError; test them with errors.Is.
var (
	// ErrArgumentMissing reports that the key is absent or null.
	ErrArgumentMissing = errors.New("argument is missing")
	// ErrArgumentNotANumber reports a value that is not a number: a type
	// other than a number, NaN, or a json.Number that is not a JSON number
	// literal.
	ErrArgumentNotANumber = errors.New("argument is not a number")
	// ErrArgumentFraction reports a number with a fractional part.
	ErrArgumentFraction = errors.New("argument has a fractional part")
	// ErrArgumentOutOfRange reports a number outside the range of int.
	ErrArgumentOutOfRange = errors.New("argument is out of range")
	// ErrArgumentInexact reports a float64 that stands for more than one
	// whole number, so its exact value cannot be known.
	ErrArgumentInexact = errors.New("argument is not exactly representable")
	// ErrArgumentBadText reports a string that is not a decimal integer
	// strconv.Atoi accepts, when text is on.
	ErrArgumentBadText = errors.New("argument is not a decimal integer")
)

// argumentValueCut is the most bytes of an offending value an ArgumentError
// keeps.
const argumentValueCut = 64

// ArgumentError is the error RequireWholeInt and RequireWholeIntText return.
// Reason is one of the ErrArgument sentinels, and Unwrap returns it, so
// callers test the reason with errors.Is.
type ArgumentError struct {
	// Key is the argument name.
	Key string
	// Value is the offending value for a scalar, the client's literal when it
	// was judged by that, cut at 64 bytes, and empty for anything else.
	Value string
	// Reason is the sentinel that says why the argument was refused.
	Reason error
}

// Error names the key, the reason and, for a scalar, the cut value.
func (e *ArgumentError) Error() string {
	if e.Value == "" {
		return fmt.Sprintf("argument %q: %v", e.Key, e.Reason)
	}
	return fmt.Sprintf("argument %q: %v: %q", e.Key, e.Reason, e.Value)
}

// Unwrap returns the Reason.
func (e *ArgumentError) Unwrap() error { return e.Reason }

// RequireWholeInt returns the int argument for key, and refuses what GetInt
// and RequireInt silently truncate or convert: a fraction, a value outside
// the range of int, a NaN, and a string. It reads the same map as GetInt. A
// missing key or a null is ErrArgumentMissing. A whole number past 2^53 is
// read exactly from the client's literal when the request carries one. Under
// a before-call or before-any hook with input validation on, a float64 of 2^53
// or more in size answers ErrArgumentInexact instead. The error is an
// *ArgumentError whose Reason is one of the ErrArgument sentinels.
func (r CallToolRequest) RequireWholeInt(key string) (int, error) {
	return r.requireWholeInt(key, false)
}

// RequireWholeIntText is RequireWholeInt with text on: a string is accepted
// when strconv.Atoi accepts it, so "+5" is 5 and "007" is 7, and " 5", "1e2" and
// "" are ErrArgumentBadText.
func (r CallToolRequest) RequireWholeIntText(key string) (int, error) {
	return r.requireWholeInt(key, true)
}

func (r CallToolRequest) requireWholeInt(key string, text bool) (int, error) {
	val, ok := r.GetArguments()[key]
	if !ok || val == nil {
		return 0, &ArgumentError{Key: key, Reason: ErrArgumentMissing}
	}
	n, literal, err := r.wholeArgument(key, val, strconv.IntSize, text)
	if err != nil {
		value := scalarText(val)
		if literal != "" {
			value = strings.Clone(cutText(literal))
		}
		return 0, &ArgumentError{Key: key, Value: value, Reason: err}
	}
	return int(n), nil
}

// roundTripped reports whether the server marked p: its input validation
// judged a round trip of a before-hook's map, so a float64 in Arguments may
// stand for several of the literals the client sent.
func roundTripped(p CallToolParams) bool { return p.roundTrip }

// maxExactFloat is 2^53, where a float64 stops standing for one whole number.
const maxExactFloat = float64(1 << 53)

// wholeArgument judges val, the value of key in the request's map, as
// wholeInt does, with the one thing wholeInt cannot know: a float64 may be
// refined from the client's literal, or refused as inexact.
//
// On a marked request (see roundTripped) validation judged a round trip of the
// map, so the raw literal may hold a value it never checked: the float64 is
// range-checked, and one of 2^53 or more in size is ErrArgumentInexact. On
// any other request that carries raw arguments, a whole float64 the raw
// literal of key parses to is judged by that literal, so a number past 2^53 is
// read exactly; when the literal parses to another value, a hook or
// middleware changed the map, and the map wins. The string is the literal it
// judged, which shares the request's bytes, or "".
func (r CallToolRequest) wholeArgument(key string, val any, bits int, text bool) (int64, string, error) {
	x, ok := val.(float64)
	if !ok {
		n, err := wholeInt(val, bits, text)
		return n, "", err
	}
	if roundTripped(r.Params) {
		n, err := wholeFloat(x, bits)
		if err == nil && math.Abs(x) >= maxExactFloat {
			return 0, "", ErrArgumentInexact
		}
		return n, "", err
	}
	if len(r.Params.RawArguments) > 0 && x == math.Trunc(x) && !math.IsInf(x, 0) {
		if literal, found := lastTopLevelLiteral(r.Params.RawArguments, key); found {
			if f, err := strconv.ParseFloat(literal, 64); err == nil && f == x {
				n, err := wholeLiteral(literal, bits)
				return n, literal, err
			}
		}
	}
	n, err := wholeFloat(x, bits)
	return n, "", err
}

// lastTopLevelLiteral returns the text of the value of the last top-level
// occurrence of key in raw, a JSON object, as the client sent it, and false
// when raw is not an object or has no such key. The scan is depth-aware and
// compares each key as decoding reads it; strings and nested values are
// skipped. It allocates only for a key that holds an escape or invalid UTF-8.
// The literal shares raw's memory, so it is good only while raw is not written.
func lastTopLevelLiteral(raw json.RawMessage, key string) (literal string, ok bool) {
	i := skipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return "", false
	}
	i++
	for {
		i = skipSpace(raw, i)
		if i < len(raw) && raw[i] == ',' {
			i = skipSpace(raw, i+1)
		}
		if i >= len(raw) || raw[i] != '"' {
			return literal, ok
		}
		quoted, nameEnd := raw[i:], skipString(raw, i)
		if nameEnd < 0 {
			return literal, ok
		}
		quoted = quoted[:nameEnd-i]
		i = skipSpace(raw, nameEnd)
		if i >= len(raw) || raw[i] != ':' {
			return literal, ok
		}
		start := skipSpace(raw, i+1)
		end := skipValue(raw, start)
		if end < 0 {
			return literal, ok
		}
		if keyIs(quoted, key) {
			literal, ok = unsafe.String(&raw[start], end-start), true
		}
		i = end
	}
}

// keyIs reports whether quoted, a JSON string with its quotes, decodes to key.
// Only an escape or invalid UTF-8, which decoding replaces with U+FFFD, makes
// a name differ from its bytes, and encoding/json itself reads those.
func keyIs(quoted []byte, key string) bool {
	name := quoted[1 : len(quoted)-1]
	if bytes.IndexByte(name, '\\') >= 0 || !utf8.Valid(name) {
		var decoded string
		return json.Unmarshal(quoted, &decoded) == nil && decoded == key
	}
	return string(name) == key
}

// skipSpace returns the index of the first byte of raw at or after i that is
// not JSON white space, or len(raw).
func skipSpace(raw []byte, i int) int {
	for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\n' || raw[i] == '\r') {
		i++
	}
	return i
}

// stringStretch is how many bytes skipString reads one at a time before it
// jumps: a shorter string costs more in a jump than in the loop, and a string
// full of escaped quotes pays one jump per stretch.
const stringStretch = 32

// skipString returns the index after the string that starts at raw[i], a
// quote, or -1 when it is not closed. It reads a stretch a byte at a time, then
// jumps to the next quote; backslashes before that quote are counted back to
// where the jump began, a place outside any escape, and an even number leaves
// the quote closing the string.
func skipString(raw []byte, i int) int {
	i++
	for i < len(raw) {
		for end := min(i+stringStretch, len(raw)); i < end; i++ {
			switch raw[i] {
			case '\\':
				i++
			case '"':
				return i + 1
			}
		}
		if i >= len(raw) {
			return -1
		}
		start := i
		n := bytes.IndexByte(raw[i:], '"')
		if n < 0 {
			return -1
		}
		i += n
		back := i
		for back > start && raw[back-1] == '\\' {
			back--
		}
		if (i-back)%2 == 0 {
			return i + 1
		}
		i++
	}
	return -1
}

// skipValue returns the index after the JSON value that starts at raw[i], or
// -1 when there is none or it is not closed. A scalar other than a string ends
// at the first delimiter.
func skipValue(raw []byte, i int) int {
	if i >= len(raw) {
		return -1
	}
	switch raw[i] {
	case '"':
		return skipString(raw, i)
	case '{', '[':
		depth := 0
		for ; i < len(raw); i++ {
			switch raw[i] {
			case '"':
				if i = skipString(raw, i); i < 0 {
					return -1
				}
				i--
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return i + 1
				}
			}
		}
		return -1
	}
	start := i
	for i < len(raw) {
		switch raw[i] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			if i == start {
				return -1
			}
			return i
		}
		i++
	}
	return i
}

// scalarText returns v as text cut at argumentValueCut bytes on a UTF-8
// boundary, or "" when v is not a scalar.
func scalarText(v any) string {
	switch x := v.(type) {
	case string:
		return cutText(x)
	case json.Number:
		return cutText(string(x))
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32)
	case int:
		return strconv.FormatInt(int64(x), 10)
	case int8:
		return strconv.FormatInt(int64(x), 10)
	case int16:
		return strconv.FormatInt(int64(x), 10)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case uint:
		return strconv.FormatUint(uint64(x), 10)
	case uint8:
		return strconv.FormatUint(uint64(x), 10)
	case uint16:
		return strconv.FormatUint(uint64(x), 10)
	case uint32:
		return strconv.FormatUint(uint64(x), 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}

// cutText cuts s at argumentValueCut bytes, backing up to a rune start.
func cutText(s string) string {
	if len(s) <= argumentValueCut {
		return s
	}
	end := argumentValueCut
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

// wholeInt judges v as a whole number within the signed range of the given
// bit size (32 or 64). With text on a string is judged as strconv.Atoi would.
// On refusal it returns one of the ErrArgument sentinels, never an
// *ArgumentError.
func wholeInt(v any, bits int, text bool) (int64, error) {
	switch x := v.(type) {
	case json.Number:
		return wholeLiteral(string(x), bits)
	case float64:
		return wholeFloat(x, bits)
	case float32:
		return wholeFloat(float64(x), bits)
	case int:
		return wholeSigned(int64(x), bits)
	case int8:
		return wholeSigned(int64(x), bits)
	case int16:
		return wholeSigned(int64(x), bits)
	case int32:
		return wholeSigned(int64(x), bits)
	case int64:
		return wholeSigned(x, bits)
	case uint:
		return wholeUnsigned(uint64(x), bits)
	case uint8:
		return wholeUnsigned(uint64(x), bits)
	case uint16:
		return wholeUnsigned(uint64(x), bits)
	case uint32:
		return wholeUnsigned(uint64(x), bits)
	case uint64:
		return wholeUnsigned(x, bits)
	case string:
		if !text {
			return 0, ErrArgumentNotANumber
		}
		n, err := strconv.ParseInt(x, 10, bits)
		switch {
		case err == nil:
			return n, nil
		case errors.Is(err, strconv.ErrRange):
			return 0, ErrArgumentOutOfRange
		}
		return 0, ErrArgumentBadText
	}
	return 0, ErrArgumentNotANumber
}

// wholeSigned range-checks x against the signed width.
func wholeSigned(x int64, bits int) (int64, error) {
	limit := int64(1)<<(bits-1) - 1
	if x < -limit-1 || x > limit {
		return 0, ErrArgumentOutOfRange
	}
	return x, nil
}

// wholeUnsigned range-checks x against the signed width.
func wholeUnsigned(x uint64, bits int) (int64, error) {
	if x > uint64(1)<<(bits-1)-1 {
		return 0, ErrArgumentOutOfRange
	}
	return int64(x), nil
}

// wholeFloat judges a float. The bounds are exact powers of two with an
// exclusive upper bound, as in checkedSigned, because MaxInt64 converted to
// float64 rounds up to 2^63.
func wholeFloat(x float64, bits int) (int64, error) {
	if math.IsNaN(x) {
		return 0, ErrArgumentNotANumber
	}
	bound := math.Ldexp(1, bits-1)
	if x < -bound || x >= bound {
		return 0, ErrArgumentOutOfRange
	}
	if x != math.Trunc(x) {
		return 0, ErrArgumentFraction
	}
	return int64(x), nil
}

// exponentCap is where wholeLiteral stops growing an exponent: far past any
// literal that fits in memory, and small enough that it cannot overflow int64
// when added to a length.
const exponentCap = int64(1) << 40

// wholeLiteral judges s as a JSON number literal: a whole number within the
// signed range of the given bit size. It parses sign, digits, fraction and
// exponent in one pass, saturating the exponent. A zero mantissa is 0. The
// literal is whole if and only if every fraction digit the exponent does not
// shift into the integer part is zero, and it is in range only if its integer
// part, without leading zeros, has at most 19 digits and the value fits.
// It does not allocate.
func wholeLiteral(s string, bits int) (int64, error) {
	i := 0
	neg := false
	if i < len(s) && s[i] == '-' {
		neg = true
		i++
	}
	start := i
	switch {
	case i < len(s) && s[i] == '0':
		i++
	case i < len(s) && s[i] >= '1' && s[i] <= '9':
		for i < len(s) && isDigit(s[i]) {
			i++
		}
	default:
		return 0, ErrArgumentNotANumber
	}
	intS := s[start:i]
	fracS := ""
	if i < len(s) && s[i] == '.' {
		i++
		fs := i
		for i < len(s) && isDigit(s[i]) {
			i++
		}
		if i == fs {
			return 0, ErrArgumentNotANumber
		}
		fracS = s[fs:i]
	}
	var exp int64
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		expNeg := false
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			expNeg = s[i] == '-'
			i++
		}
		es := i
		for i < len(s) && isDigit(s[i]) {
			if exp < exponentCap {
				exp = exp*10 + int64(s[i]-'0')
			}
			i++
		}
		if i == es {
			return 0, ErrArgumentNotANumber
		}
		if exp > exponentCap {
			exp = exponentCap
		}
		if expNeg {
			exp = -exp
		}
	}
	if i != len(s) {
		return 0, ErrArgumentNotANumber
	}

	if firstNonzero(intS, fracS, 0) == -1 {
		return 0, nil
	}
	n := int64(len(intS) + len(fracS))
	point := int64(len(intS)) + exp
	if point < 0 {
		return 0, ErrArgumentFraction
	}
	if point < n && firstNonzero(intS, fracS, int(point)) != -1 {
		return 0, ErrArgumentFraction
	}

	whole := min(point, n)
	var u uint64
	count := int64(0)
	for k := range whole {
		d := uint64(digitAt(intS, fracS, int(k)))
		if count == 0 && d == 0 {
			continue
		}
		count++
		if count > 19 {
			return 0, ErrArgumentOutOfRange
		}
		u = u*10 + d
	}
	if point > n {
		if count+(point-n) > 19 {
			return 0, ErrArgumentOutOfRange
		}
		for range point - n {
			// Unreachable after the 19-digit bound above; kept as the explicit overflow check.
			if u > math.MaxUint64/10 {
				return 0, ErrArgumentOutOfRange
			}
			u *= 10
		}
	}

	limit := uint64(1) << (bits - 1)
	if neg {
		if u > limit {
			return 0, ErrArgumentOutOfRange
		}
		return int64(-u), nil
	}
	if u > limit-1 {
		return 0, ErrArgumentOutOfRange
	}
	return int64(u), nil
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// digitAt returns the k-th digit of intS followed by fracS.
func digitAt(intS, fracS string, k int) byte {
	if k < len(intS) {
		return intS[k] - '0'
	}
	return fracS[k-len(intS)] - '0'
}

// firstNonzero returns the index of the first nonzero digit at or after from in
// intS followed by fracS, or -1 when every one is zero.
func firstNonzero(intS, fracS string, from int) int {
	for k := from; k < len(intS)+len(fracS); k++ {
		if digitAt(intS, fracS, k) != 0 {
			return k
		}
	}
	return -1
}
