package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The Parse* helpers accept whatever a tool call's arguments decoded to and
// coerce it to the type the caller asked for. These are the only conversions
// the library performs; coerce_golden_test.go pins their behaviour to what
// spf13/cast v1.7.1 answered, which the library used before.

var errNegativeUnsigned = errors.New("negative value for an unsigned integer")

func toBool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		b, _ := strconv.ParseBool(x)
		return b
	case json.Number:
		n, err := parseInt(string(x))
		return err == nil && n != 0
	}
	f, err := toFloat64E(v)
	return err == nil && f != 0
}

func toInt64(v any) int64     { n, _ := toSigned[int64](v); return n }
func toInt32(v any) int32     { n, _ := toSigned[int32](v); return n }
func toInt16(v any) int16     { n, _ := toSigned[int16](v); return n }
func toInt8(v any) int8       { n, _ := toSigned[int8](v); return n }
func toInt(v any) int         { n, _ := toSigned[int](v); return n }
func toUint(v any) uint       { n, _ := toUnsigned[uint](v); return n }
func toUint64(v any) uint64   { n, _ := toUnsigned[uint64](v); return n }
func toUint32(v any) uint32   { n, _ := toUnsigned[uint32](v); return n }
func toUint16(v any) uint16   { n, _ := toUnsigned[uint16](v); return n }
func toUint8(v any) uint8     { n, _ := toUnsigned[uint8](v); return n }
func toFloat64(v any) float64 { f, _ := toFloat64E(v); return f }
func toInt64E(v any) (int64, error) {
	return toSigned[int64](v)
}

// toSigned converts with Go's own conversion from each source kind, so a
// value outside T's range wraps or saturates exactly as T(x) does.
func toSigned[T int | int8 | int16 | int32 | int64](v any) (T, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case int:
		return T(x), nil
	case int8:
		return T(x), nil
	case int16:
		return T(x), nil
	case int32:
		return T(x), nil
	case int64:
		return T(x), nil
	case uint:
		return T(x), nil
	case uint8:
		return T(x), nil
	case uint16:
		return T(x), nil
	case uint32:
		return T(x), nil
	case uint64:
		return T(x), nil
	case float32:
		return T(x), nil
	case float64:
		return T(x), nil
	case string:
		n, err := parseInt(x)
		return T(n), err
	case json.Number:
		n, err := parseInt(string(x))
		return T(n), err
	}
	return 0, fmt.Errorf("cannot convert %T to %T", v, T(0))
}

func toUnsigned[T uint | uint8 | uint16 | uint32 | uint64](v any) (T, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case int:
		if x < 0 {
			return 0, errNegativeUnsigned
		}
		return T(x), nil
	case int8:
		if x < 0 {
			return 0, errNegativeUnsigned
		}
		return T(x), nil
	case int16:
		if x < 0 {
			return 0, errNegativeUnsigned
		}
		return T(x), nil
	case int32:
		if x < 0 {
			return 0, errNegativeUnsigned
		}
		return T(x), nil
	case int64:
		if x < 0 {
			return 0, errNegativeUnsigned
		}
		return T(x), nil
	case uint:
		return T(x), nil
	case uint8:
		return T(x), nil
	case uint16:
		return T(x), nil
	case uint32:
		return T(x), nil
	case uint64:
		return T(x), nil
	case float32:
		if x < 0 {
			return 0, errNegativeUnsigned
		}
		return T(x), nil
	case float64:
		if x < 0 {
			return 0, errNegativeUnsigned
		}
		return T(x), nil
	case string:
		return unsignedFromText[T](x)
	case json.Number:
		return unsignedFromText[T](string(x))
	}
	return 0, fmt.Errorf("cannot convert %T to %T", v, T(0))
}

func unsignedFromText[T uint | uint8 | uint16 | uint32 | uint64](s string) (T, error) {
	n, err := parseInt(s)
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, errNegativeUnsigned
	}
	return T(n), nil
}

// parseInt reads an integer in any Go base prefix ("0x1A", "010"), accepting
// a zero fraction ("3.0") and nothing else after the point.
func parseInt(s string) (int64, error) {
	if i := strings.IndexByte(s, '.'); i >= 0 && strings.Trim(s[i+1:], "0") == "" {
		s = s[:i]
	}
	return strconv.ParseInt(s, 0, 0)
}

func toFloat64E(v any) (float64, error) {
	switch x := v.(type) {
	case nil:
		return 0, nil
	case bool:
		if x {
			return 1, nil
		}
		return 0, nil
	case int:
		return float64(x), nil
	case int8:
		return float64(x), nil
	case int16:
		return float64(x), nil
	case int32:
		return float64(x), nil
	case int64:
		return float64(x), nil
	case uint:
		return float64(x), nil
	case uint8:
		return float64(x), nil
	case uint16:
		return float64(x), nil
	case uint32:
		return float64(x), nil
	case uint64:
		return float64(x), nil
	case float32:
		return float64(x), nil
	case float64:
		return x, nil
	case string:
		return strconv.ParseFloat(x, 64)
	case json.Number:
		return x.Float64()
	}
	return 0, fmt.Errorf("cannot convert %T to float64", v)
}

func toFloat32(v any) float32 {
	switch x := v.(type) {
	case string:
		f, _ := strconv.ParseFloat(x, 32)
		return float32(f)
	case float32:
		return x
	}
	f, _ := toFloat64E(v)
	return float32(f)
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'f', -1, 32)
	case int:
		return strconv.Itoa(x)
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
	case json.Number:
		return string(x)
	case []byte:
		return string(x)
	case error:
		return x.Error()
	case fmt.Stringer:
		return x.String()
	}
	return ""
}

// toStringMap answers an empty map, never nil, for anything it cannot read.
func toStringMap(v any) map[string]any {
	switch x := v.(type) {
	case map[string]any:
		return x
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			m[toString(k)] = val
		}
		return m
	case string:
		m := map[string]any{}
		if json.Unmarshal([]byte(x), &m) != nil {
			return map[string]any{}
		}
		return m
	}
	return map[string]any{}
}

// toStringSlice splits a string on whitespace, stringifies the elements of a
// slice, wraps any other scalar, and answers nil for what it cannot read.
func toStringSlice(v any) []string {
	switch x := v.(type) {
	case nil:
		return nil
	case []string:
		return x
	case []any:
		out := make([]string, len(x))
		for i, e := range x {
			out[i] = toString(e)
		}
		return out
	case []int:
		out := make([]string, len(x))
		for i, e := range x {
			out[i] = strconv.Itoa(e)
		}
		return out
	case string:
		return strings.Fields(x)
	}
	if s := toString(v); s != "" {
		return []string{s}
	}
	return nil
}
