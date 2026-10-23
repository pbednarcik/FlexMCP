package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
)

// GetArguments returns the Arguments as map[string]any for backward
// compatibility, nil when they are not an object. For a call to a tool that
// reads its arguments on demand, whose Arguments are nil, it returns the map
// decoded from RawArguments the first time a getter asks: the same map for
// every getter, and every copy of the request, within the call. A map set in
// Arguments, as by a hook, is returned instead.
func (r CallToolRequest) GetArguments() map[string]any {
	if args, ok := r.Params.Arguments.(map[string]any); ok {
		return args
	}
	if r.Params.Arguments == nil && r.Params.lazy != nil {
		args, _ := r.Params.lazy.get().(map[string]any)
		return args
	}
	return nil
}

// GetRawArguments returns the original arguments payload when available.
// For JSON-RPC requests this is json.RawMessage; otherwise it falls back to Arguments.
func (r CallToolRequest) GetRawArguments() any {
	if len(r.Params.RawArguments) > 0 {
		return r.Params.RawArguments
	}
	return r.Params.Arguments
}

// BindArguments unmarshals the Arguments into the provided struct
// This is useful for working with strongly-typed arguments
func (r CallToolRequest) BindArguments(target any) error {
	if target == nil || reflect.ValueOf(target).Kind() != reflect.Pointer {
		return errors.New("target must be a non-nil pointer")
	}

	if len(r.Params.RawArguments) > 0 {
		return json.Unmarshal(r.Params.RawArguments, target)
	}

	// Fast-path: already raw JSON
	if raw, ok := r.Params.Arguments.(json.RawMessage); ok {
		return json.Unmarshal(raw, target)
	}

	data, err := json.Marshal(r.Params.Arguments)
	if err != nil {
		return fmt.Errorf("failed to marshal arguments: %w", err)
	}

	return json.Unmarshal(data, target)
}

// GetString returns a string argument by key, or the default value if not found
func (r CallToolRequest) GetString(key string, defaultValue string) string {
	args := r.GetArguments()
	if val, ok := args[key]; ok {
		if str, ok := val.(string); ok {
			return str
		}
	}
	return defaultValue
}

// RequireString returns a string argument by key, or an error if not found or not a string
func (r CallToolRequest) RequireString(key string) (string, error) {
	args := r.GetArguments()
	if val, ok := args[key]; ok {
		if str, ok := val.(string); ok {
			return str, nil
		}
		return "", fmt.Errorf("argument %q is not a string", key)
	}
	return "", fmt.Errorf("required argument %q not found", key)
}

// GetInt returns an int argument by key, or the default value if not found
func (r CallToolRequest) GetInt(key string, defaultValue int) int {
	args := r.GetArguments()
	if val, ok := args[key]; ok {
		switch v := val.(type) {
		case int:
			return v
		case float64:
			return int(v)
		case string:
			if i, err := strconv.Atoi(v); err == nil {
				return i
			}
		}
	}
	return defaultValue
}

// RequireInt returns an int argument by key, or an error if not found or not convertible to int
func (r CallToolRequest) RequireInt(key string) (int, error) {
	args := r.GetArguments()
	if val, ok := args[key]; ok {
		switch v := val.(type) {
		case int:
			return v, nil
		case float64:
			return int(v), nil
		case string:
			if i, err := strconv.Atoi(v); err == nil {
				return i, nil
			}
			return 0, fmt.Errorf("argument %q cannot be converted to int", key)
		default:
			return 0, fmt.Errorf("argument %q is not an int", key)
		}
	}
	return 0, fmt.Errorf("required argument %q not found", key)
}

// GetFloat returns a float64 argument by key, or the default value if not found
func (r CallToolRequest) GetFloat(key string, defaultValue float64) float64 {
	args := r.GetArguments()
	if val, ok := args[key]; ok {
		switch v := val.(type) {
		case float64:
			return v
		case int:
			return float64(v)
		case string:
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f
			}
		}
	}
	return defaultValue
}

// RequireFloat returns a float64 argument by key, or an error if not found or not convertible to float64
func (r CallToolRequest) RequireFloat(key string) (float64, error) {
	args := r.GetArguments()
	if val, ok := args[key]; ok {
		switch v := val.(type) {
		case float64:
			return v, nil
		case int:
			return float64(v), nil
		case string:
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				return f, nil
			}
			return 0, fmt.Errorf("argument %q cannot be converted to float64", key)
		default:
			return 0, fmt.Errorf("argument %q is not a float64", key)
		}
	}
	return 0, fmt.Errorf("required argument %q not found", key)
}

// GetBool returns a bool argument by key, or the default value if not found
func (r CallToolRequest) GetBool(key string, defaultValue bool) bool {
	args := r.GetArguments()
	if val, ok := args[key]; ok {
		switch v := val.(type) {
		case bool:
			return v
		case string:
			if b, err := strconv.ParseBool(v); err == nil {
				return b
			}
		case int:
			return v != 0
		case float64:
			return v != 0
		}
	}
	return defaultValue
}

// RequireBool returns a bool argument by key, or an error if not found or not convertible to bool
func (r CallToolRequest) RequireBool(key string) (bool, error) {
	args := r.GetArguments()
	if val, ok := args[key]; ok {
		switch v := val.(type) {
		case bool:
			return v, nil
		case string:
			if b, err := strconv.ParseBool(v); err == nil {
				return b, nil
			}
			return false, fmt.Errorf("argument %q cannot be converted to bool", key)
		case int:
			return v != 0, nil
		case float64:
			return v != 0, nil
		default:
			return false, fmt.Errorf("argument %q is not a bool", key)
		}
	}
	return false, fmt.Errorf("required argument %q not found", key)
}

// GetStringSlice returns a string slice argument by key, or the default value if not found
func (r CallToolRequest) GetStringSlice(key string, defaultValue []string) []string {
	args := r.GetArguments()
	if val, ok := args[key]; ok {
		switch v := val.(type) {
		case []string:
			return v
		case []any:
			result := make([]string, 0, len(v))
			for _, item := range v {
				if str, ok := item.(string); ok {
					result = append(result, str)
				}
			}
			return result
		}
	}
	return defaultValue
}

// RequireStringSlice returns a string slice argument by key, or an error if not found or not convertible to string slice
func (r CallToolRequest) RequireStringSlice(key string) ([]string, error) {
	args := r.GetArguments()
	if val, ok := args[key]; ok {
		switch v := val.(type) {
		case []string:
			return v, nil
		case []any:
			result := make([]string, 0, len(v))
			for i, item := range v {
				if str, ok := item.(string); ok {
					result = append(result, str)
				} else {
					return nil, fmt.Errorf("item %d in argument %q is not a string", i, key)
				}
			}
			return result, nil
		default:
			return nil, fmt.Errorf("argument %q is not a string slice", key)
		}
	}
	return nil, fmt.Errorf("required argument %q not found", key)
}

// GetIntSlice returns an int slice argument by key, or the default value if not found
func (r CallToolRequest) GetIntSlice(key string, defaultValue []int) []int {
	args := r.GetArguments()
	if val, ok := args[key]; ok {
		switch v := val.(type) {
		case []int:
			return v
		case []any:
			result := make([]int, 0, len(v))
			for _, item := range v {
				switch num := item.(type) {
				case int:
					result = append(result, num)
				case float64:
					result = append(result, int(num))
				case string:
					if i, err := strconv.Atoi(num); err == nil {
						result = append(result, i)
					}
				}
			}
			return result
		}
	}
	return defaultValue
}

// RequireIntSlice returns an int slice argument by key, or an error if not found or not convertible to int slice
func (r CallToolRequest) RequireIntSlice(key string) ([]int, error) {
	args := r.GetArguments()
	if val, ok := args[key]; ok {
		switch v := val.(type) {
		case []int:
			return v, nil
		case []any:
			result := make([]int, 0, len(v))
			for i, item := range v {
				switch num := item.(type) {
				case int:
					result = append(result, num)
				case float64:
					result = append(result, int(num))
				case string:
					if n, err := strconv.Atoi(num); err == nil {
						result = append(result, n)
					} else {
						return nil, fmt.Errorf("item %d in argument %q cannot be converted to int", i, key)
					}
				default:
					return nil, fmt.Errorf("item %d in argument %q is not an int", i, key)
				}
			}
			return result, nil
		default:
			return nil, fmt.Errorf("argument %q is not an int slice", key)
		}
	}
	return nil, fmt.Errorf("required argument %q not found", key)
}

// GetFloatSlice returns a float64 slice argument by key, or the default value if not found
func (r CallToolRequest) GetFloatSlice(key string, defaultValue []float64) []float64 {
	args := r.GetArguments()
	if val, ok := args[key]; ok {
		switch v := val.(type) {
		case []float64:
			return v
		case []any:
			result := make([]float64, 0, len(v))
			for _, item := range v {
				switch num := item.(type) {
				case float64:
					result = append(result, num)
				case int:
					result = append(result, float64(num))
				case string:
					if f, err := strconv.ParseFloat(num, 64); err == nil {
						result = append(result, f)
					}
				}
			}
			return result
		}
	}
	return defaultValue
}

// RequireFloatSlice returns a float64 slice argument by key, or an error if not found or not convertible to float64 slice
func (r CallToolRequest) RequireFloatSlice(key string) ([]float64, error) {
	args := r.GetArguments()
	if val, ok := args[key]; ok {
		switch v := val.(type) {
		case []float64:
			return v, nil
		case []any:
			result := make([]float64, 0, len(v))
			for i, item := range v {
				switch num := item.(type) {
				case float64:
					result = append(result, num)
				case int:
					result = append(result, float64(num))
				case string:
					if f, err := strconv.ParseFloat(num, 64); err == nil {
						result = append(result, f)
					} else {
						return nil, fmt.Errorf("item %d in argument %q cannot be converted to float64", i, key)
					}
				default:
					return nil, fmt.Errorf("item %d in argument %q is not a float64", i, key)
				}
			}
			return result, nil
		default:
			return nil, fmt.Errorf("argument %q is not a float64 slice", key)
		}
	}
	return nil, fmt.Errorf("required argument %q not found", key)
}

// GetBoolSlice returns a bool slice argument by key, or the default value if not found
func (r CallToolRequest) GetBoolSlice(key string, defaultValue []bool) []bool {
	args := r.GetArguments()
	if val, ok := args[key]; ok {
		switch v := val.(type) {
		case []bool:
			return v
		case []any:
			result := make([]bool, 0, len(v))
			for _, item := range v {
				switch b := item.(type) {
				case bool:
					result = append(result, b)
				case string:
					if parsed, err := strconv.ParseBool(b); err == nil {
						result = append(result, parsed)
					}
				case int:
					result = append(result, b != 0)
				case float64:
					result = append(result, b != 0)
				}
			}
			return result
		}
	}
	return defaultValue
}

// RequireBoolSlice returns a bool slice argument by key, or an error if not found or not convertible to bool slice
func (r CallToolRequest) RequireBoolSlice(key string) ([]bool, error) {
	args := r.GetArguments()
	if val, ok := args[key]; ok {
		switch v := val.(type) {
		case []bool:
			return v, nil
		case []any:
			result := make([]bool, 0, len(v))
			for i, item := range v {
				switch b := item.(type) {
				case bool:
					result = append(result, b)
				case string:
					if parsed, err := strconv.ParseBool(b); err == nil {
						result = append(result, parsed)
					} else {
						return nil, fmt.Errorf("item %d in argument %q cannot be converted to bool", i, key)
					}
				case int:
					result = append(result, b != 0)
				case float64:
					result = append(result, b != 0)
				default:
					return nil, fmt.Errorf("item %d in argument %q is not a bool", i, key)
				}
			}
			return result, nil
		default:
			return nil, fmt.Errorf("argument %q is not a bool slice", key)
		}
	}
	return nil, fmt.Errorf("required argument %q not found", key)
}

func ParseArgument(request CallToolRequest, key string, defaultVal any) any {
	args := request.GetArguments()
	if _, ok := args[key]; !ok {
		return defaultVal
	} else {
		return args[key]
	}
}

// ParseBoolean extracts and converts a boolean parameter from a CallToolRequest.
// If the key is not found in the Arguments map, the defaultValue is returned.
// Strings are read as strconv.ParseBool does ("true", "1", "f", ...).
func ParseBoolean(request CallToolRequest, key string, defaultValue bool) bool {
	v := ParseArgument(request, key, defaultValue)
	return toBool(v)
}

// ParseInt64 extracts and converts an int64 parameter from a CallToolRequest.
// It accepts the built-in integer types, floats (truncated toward zero), bools
// (as 0 or 1), and strings or json.Number holding a base-10 integer with an
// optional sign and surrounding space; base prefixes, decimal points and
// exponents are invalid. A missing key, nil, an unsupported type (pointers and
// named types included), invalid text, NaN, an infinity or a value outside
// int64's range returns defaultValue.
func ParseInt64(request CallToolRequest, key string, defaultValue int64) int64 {
	n, ok := checkedSigned(ParseArgument(request, key, defaultValue), 64)
	if !ok {
		return defaultValue
	}
	return n
}

// ParseInt32 extracts and converts an int32 parameter from a CallToolRequest.
// It accepts what ParseInt64 accepts and returns defaultValue for a value
// outside int32's range.
func ParseInt32(request CallToolRequest, key string, defaultValue int32) int32 {
	n, ok := checkedSigned(ParseArgument(request, key, defaultValue), 32)
	if !ok {
		return defaultValue
	}
	return int32(n)
}

// ParseInt16 extracts and converts an int16 parameter from a CallToolRequest.
// It accepts what ParseInt64 accepts and returns defaultValue for a value
// outside int16's range.
func ParseInt16(request CallToolRequest, key string, defaultValue int16) int16 {
	n, ok := checkedSigned(ParseArgument(request, key, defaultValue), 16)
	if !ok {
		return defaultValue
	}
	return int16(n)
}

// ParseInt8 extracts and converts an int8 parameter from a CallToolRequest.
// It accepts what ParseInt64 accepts and returns defaultValue for a value
// outside int8's range.
func ParseInt8(request CallToolRequest, key string, defaultValue int8) int8 {
	n, ok := checkedSigned(ParseArgument(request, key, defaultValue), 8)
	if !ok {
		return defaultValue
	}
	return int8(n)
}

// ParseInt extracts and converts an int parameter from a CallToolRequest.
// It accepts what ParseInt64 accepts and returns defaultValue for a value
// outside int's range on the target platform.
func ParseInt(request CallToolRequest, key string, defaultValue int) int {
	n, ok := checkedSigned(ParseArgument(request, key, defaultValue), strconv.IntSize)
	if !ok {
		return defaultValue
	}
	return int(n)
}

// ParseUInt extracts and converts a uint parameter from a CallToolRequest.
// It accepts what ParseUInt64 accepts and returns defaultValue for a value
// outside uint's range on the target platform.
func ParseUInt(request CallToolRequest, key string, defaultValue uint) uint {
	n, ok := checkedUnsigned(ParseArgument(request, key, defaultValue), strconv.IntSize)
	if !ok {
		return defaultValue
	}
	return uint(n)
}

// ParseUInt64 extracts and converts a uint64 parameter from a CallToolRequest.
// It accepts the built-in integer types, floats (truncated toward zero), bools
// (as 0 or 1), and strings or json.Number holding an unsigned base-10 integer
// with surrounding space; a sign, base prefixes, decimal points and exponents
// are invalid. A missing key, nil, an unsupported type (pointers and named
// types included), invalid text, any negative number (fractions above -1
// included), NaN, an infinity or a value outside uint64's range returns
// defaultValue.
func ParseUInt64(request CallToolRequest, key string, defaultValue uint64) uint64 {
	n, ok := checkedUnsigned(ParseArgument(request, key, defaultValue), 64)
	if !ok {
		return defaultValue
	}
	return n
}

// ParseUInt32 extracts and converts a uint32 parameter from a CallToolRequest.
// It accepts what ParseUInt64 accepts and returns defaultValue for a value
// outside uint32's range.
func ParseUInt32(request CallToolRequest, key string, defaultValue uint32) uint32 {
	n, ok := checkedUnsigned(ParseArgument(request, key, defaultValue), 32)
	if !ok {
		return defaultValue
	}
	return uint32(n)
}

// ParseUInt16 extracts and converts a uint16 parameter from a CallToolRequest.
// It accepts what ParseUInt64 accepts and returns defaultValue for a value
// outside uint16's range.
func ParseUInt16(request CallToolRequest, key string, defaultValue uint16) uint16 {
	n, ok := checkedUnsigned(ParseArgument(request, key, defaultValue), 16)
	if !ok {
		return defaultValue
	}
	return uint16(n)
}

// ParseUInt8 extracts and converts a uint8 parameter from a CallToolRequest.
// It accepts what ParseUInt64 accepts and returns defaultValue for a value
// outside uint8's range.
func ParseUInt8(request CallToolRequest, key string, defaultValue uint8) uint8 {
	n, ok := checkedUnsigned(ParseArgument(request, key, defaultValue), 8)
	if !ok {
		return defaultValue
	}
	return uint8(n)
}

// ParseFloat32 extracts and converts a float32 parameter from a CallToolRequest.
func ParseFloat32(request CallToolRequest, key string, defaultValue float32) float32 {
	v := ParseArgument(request, key, defaultValue)
	return toFloat32(v)
}

// ParseFloat64 extracts and converts a float64 parameter from a CallToolRequest.
func ParseFloat64(request CallToolRequest, key string, defaultValue float64) float64 {
	v := ParseArgument(request, key, defaultValue)
	return toFloat64(v)
}

// ParseString extracts and converts a string parameter from a CallToolRequest.
func ParseString(request CallToolRequest, key string, defaultValue string) string {
	v := ParseArgument(request, key, defaultValue)
	return toString(v)
}

// ParseStringMap extracts and converts a string map parameter from a CallToolRequest.
func ParseStringMap(request CallToolRequest, key string, defaultValue map[string]any) map[string]any {
	v := ParseArgument(request, key, defaultValue)
	return toStringMap(v)
}
