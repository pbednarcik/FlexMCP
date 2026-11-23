package server

import (
	"strings"
	"testing"
)

// scanArgumentsByteByByte is scanArguments as it read a string before it
// skipped one with bytes.IndexByte: a byte at a time, a backslash taking the
// byte after it along. It is kept here as the answer scanArguments must give.
func scanArgumentsByteByByte(raw []byte, maxValues int) (exact, within bool) {
	depth, deepest, values := 0, 0, 0
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; {
		case c == ',':
			values++
		case c == '[' || c == '{':
			values++
			depth++
			deepest = max(deepest, depth)
		case c == ']' || c == '}':
			depth--
		case c == '"':
			for i++; i < len(raw) && raw[i] != '"'; i++ {
				if raw[i] == '\\' {
					i++
				}
			}
		case c == '-' || '0' <= c && c <= '9':
			digits := 0
			for ; i < len(raw); i++ {
				d := raw[i]
				if '0' <= d && d <= '9' {
					digits++
					continue
				}
				if d == 'e' || d == 'E' {
					return false, false
				}
				if d != '-' && d != '.' {
					break
				}
			}
			if digits > 15 {
				return false, false
			}
			i--
		}
	}
	return true, deepest <= maxArgumentDepth && (maxValues <= 0 || values <= maxValues)
}

// scanAgreementCases are texts whose strings end, or seem to, where a wrong
// skip would read their bodies as structure: an exponent, a long number, a
// comma or a bracket behind a quote it took for the closing one.
var scanAgreementCases = []string{
	`{}`,
	`{"a":"x","b":12}`,
	`{"a":"x\"y","b":12}`,
	`{"a":"\"1e5"}`,
	`{"a":"\\","b":1e3}`,
	`{"a":"\\\\","b":1e3}`,
	`{"a":"\\\"\\","b":[1,2,{"c":"d"}]}`,
	`{"a":"\\\\\"1e5\\\\","b":1}`,
	`{"a":"\\\\\\\"","n":1234567890123456}`,
	`{"a":"` + strings.Repeat(`ab\"`, 1000) + `","n":1234567890123456}`,
	`{"a":"` + strings.Repeat(`\\`, 1000) + `","n":1e1}`,
	`{"a":"` + strings.Repeat(`\\`, 999) + `\","n":1e1}`,
	`{"a":"" 1e5 ,,,, [[[["}`,
	`{"a":",,,,","b":"[[{{"}`,
	`["",1,"\\\\\"",2]`,
	`["\"",[["\\"]],"\\\""]`,
	`"`,
	`"\`,
	`"\\`,
	`{"a":"unterminated`,
	`{"a":"unterminated 1e5 ,,,, [[[[`,
	`{"a":"unterminated\" 1e5`,
	`{"a":"ends in a backslash\`,
	`{"a":"x"} "1e5`,
	`{"a":"` + strings.Repeat("abcdefgh", 4<<10) + `","b":0.000000000000001}`,
	// Nesting at maxArgumentDepth and past it, and the same brackets inside a
	// string, which nest nothing.
	strings.Repeat(`[`, maxArgumentDepth) + strings.Repeat(`]`, maxArgumentDepth),
	strings.Repeat(`[`, maxArgumentDepth+1) + strings.Repeat(`]`, maxArgumentDepth+1),
	`{"a":"` + strings.Repeat(`[`, maxArgumentDepth+1) + `"}`,
	`{"a":"` + strings.Repeat(`\"[`, maxArgumentDepth+1) + `"}`,
	// Escapes a short and a long gap apart, on either side of where a skip
	// may switch from reading bytes to searching for the next quote.
	`{"a":"` + strings.Repeat(`x`, 15) + `\"` + strings.Repeat(`y`, 16) + `\\"` + `,"n":1e5}`,
	`{"a":"` + strings.Repeat(`x`, 16) + `\\\"` + strings.Repeat(`y`, 17) + `\"1e5"}`,
	`{"a":"` + strings.Repeat(`x`, 100) + `\` + strings.Repeat(`\`, 63) + `"` + `,"n":1e5}`,
	`{"a":"` + strings.Repeat(strings.Repeat(`z`, 31)+`\"`, 64) + `"}`,
}

// scanAgreementLimits are the caps the cases are scanned under.
var scanAgreementLimits = []int{0, 1, 2, 3, DefaultMaxValidatedValues}

// scanArguments answers every text as the byte-by-byte scan does, under every
// cap: skipping a string by its next unescaped quote ends it where reading
// it a byte at a time does, an unterminated one at the end of the text.
func TestScanArgumentsAgreesWithByteScan(t *testing.T) {
	for _, in := range scanAgreementCases {
		for _, limit := range scanAgreementLimits {
			wantExact, wantWithin := scanArgumentsByteByByte([]byte(in), limit)
			exact, within := scanArguments([]byte(in), limit)
			if exact != wantExact || within != wantWithin {
				t.Errorf("%.80q under cap %d: exact %v, within %v; the byte scan says %v, %v", in, limit, exact, within, wantExact, wantWithin)
			}
		}
	}
}

// FuzzScanArgumentsAgrees checks scanArguments against the byte-by-byte scan
// on any bytes, JSON or not, under a small cap.
func FuzzScanArgumentsAgrees(f *testing.F) {
	for _, in := range scanAgreementCases {
		for _, limit := range scanAgreementLimits {
			f.Add([]byte(in), limit)
		}
	}
	f.Fuzz(func(t *testing.T, raw []byte, limit int) {
		wantExact, wantWithin := scanArgumentsByteByByte(raw, limit)
		exact, within := scanArguments(raw, limit)
		if exact != wantExact || within != wantWithin {
			t.Errorf("%q under cap %d: exact %v, within %v; the byte scan says %v, %v", raw, limit, exact, within, wantExact, wantWithin)
		}
	})
}
