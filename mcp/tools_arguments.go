package mcp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
)

// GetArguments returns the Arguments as map[string]any for backward compatibility
// If Arguments is not a map, it returns an empty map
func (r CallToolRequest) GetArguments() map[string]any {
	if args, ok := r.Params.Arguments.(map[string]any); ok {
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
		return fmt.Errorf("target must be a non-nil pointer")
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
