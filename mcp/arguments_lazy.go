package mcp

import (
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/pbednarcik/flexmcp/internal/argsdecode"
)

func init() {
	argsdecode.SetOnDemand = func(params any, onDemand func(name string) bool) {
		params.(*CallToolParams).onDemand = onDemand
	}
	argsdecode.AtOnce = func(params any) {
		p := params.(*CallToolParams)
		if p.Arguments == nil && p.lazy != nil {
			p.lazy.atOnce.Store(true)
			p.Arguments = p.lazy.get()
			p.lazy = nil
		}
	}
	argsdecode.Decoded = func(params any) any {
		p := params.(*CallToolParams)
		if p.Arguments == nil && p.lazy != nil {
			return p.lazy.get()
		}
		return p.Arguments
	}
	argsdecode.MarkRoundTrip = func(params any) {
		params.(*CallToolParams).roundTrip = true
	}
	argsdecode.Settle = func(params any) {
		p := params.(*CallToolParams)
		if p.Arguments == nil && p.lazy != nil && p.lazy.atOnce.Load() {
			p.Arguments = p.lazy.get()
			p.lazy = nil
		}
	}
}

// lazyArguments holds the arguments of a call whose tool reads them on
// demand, and builds from them, the first time a getter asks, the value
// json.Unmarshal into an any builds: once per call, shared by every copy of
// the request, which all hold the same pointer.
type lazyArguments struct {
	once  sync.Once
	raw   json.RawMessage
	value any
	// atOnce records that a copy of the request was given its Arguments at
	// once (argsdecode.AtOnce), for argsdecode.Settle to give every other.
	atOnce atomic.Bool
}

// get returns the arguments decoded, decoding them on the first call.
func (l *lazyArguments) get() any {
	l.once.Do(func() {
		if value, ok := decodeArgumentsFast(l.raw); ok {
			l.value = value
			return
		}
		// The decode cannot fail: the arguments are valid JSON, since the
		// request's decode copied them, and their numbers are in range
		// (argumentNumbersInRange).
		_ = json.Unmarshal(l.raw, &l.value)
	})
	return l.value
}

// argumentNumbersInRange reports whether every number in raw, a valid JSON
// text, is in a float64's range: the one check json.Unmarshal into an any
// makes that copying raw does not, so arguments it accepts decode on demand
// exactly as at once, and a call it refuses is decoded at once, to fail with
// json.Unmarshal's error. Only a literal with an exponent, or of more than 308
// digits, can be out of range, so only those are parsed; the scan allocates
// for nothing else.
func argumentNumbersInRange(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		switch c := raw[i]; {
		case c == '"':
			for i++; i < len(raw) && raw[i] != '"'; i++ {
				if raw[i] == '\\' {
					i++
				}
			}
		case c == '-' || '0' <= c && c <= '9':
			start, digits, exponent := i, 0, false
			for ; i < len(raw); i++ {
				d := raw[i]
				if '0' <= d && d <= '9' {
					digits++
				} else if d == 'e' || d == 'E' {
					exponent = true
				} else if d != '-' && d != '+' && d != '.' {
					break
				}
			}
			if exponent || digits > 308 {
				if _, err := strconv.ParseFloat(string(raw[start:i]), 64); err != nil {
					return false
				}
			}
			i--
		}
	}
	return true
}
