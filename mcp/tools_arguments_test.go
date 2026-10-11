package mcp

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCallToolRequestArgumentsFromTheWire(t *testing.T) {
	// Arguments decoded from JSON arrive as float64, string and []any; every
	// helper must read them as the type the handler asked for.
	var request CallToolRequest
	require.NoError(t, json.Unmarshal([]byte(`{"params":{"name":"calc","arguments":{
		"n": 7, "s": "8", "fs": "2.5", "t": "true", "one": 1, "zero": 0,
		"ints": [1, "2", 3.5], "floats": [1, "2.5"], "bools": [true, "false", 1, 0.0], "strs": ["a", "b"]
	}}}`), &request))

	assert.Equal(t, 7, request.GetInt("n", -1))
	assert.Equal(t, 8, request.GetInt("s", -1))
	n, err := request.RequireInt("n")
	require.NoError(t, err)
	assert.Equal(t, 7, n)
	n, err = request.RequireInt("s")
	require.NoError(t, err)
	assert.Equal(t, 8, n)

	assert.Equal(t, 2.5, request.GetFloat("fs", -1))
	f, err := request.RequireFloat("fs")
	require.NoError(t, err)
	assert.Equal(t, 2.5, f)

	assert.True(t, request.GetBool("t", false))
	for key, want := range map[string]bool{"t": true, "one": true, "zero": false} {
		b, err := request.RequireBool(key)
		require.NoError(t, err, key)
		assert.Equal(t, want, b, key)
	}

	assert.Equal(t, []int{1, 2, 3}, request.GetIntSlice("ints", nil))
	ints, err := request.RequireIntSlice("ints")
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3}, ints)

	assert.Equal(t, []float64{1, 2.5}, request.GetFloatSlice("floats", nil))
	floats, err := request.RequireFloatSlice("floats")
	require.NoError(t, err)
	assert.Equal(t, []float64{1, 2.5}, floats)

	assert.Equal(t, []bool{true, false, true, false}, request.GetBoolSlice("bools", nil))
	bools, err := request.RequireBoolSlice("bools")
	require.NoError(t, err)
	assert.Equal(t, []bool{true, false, true, false}, bools)

	assert.Equal(t, []string{"a", "b"}, request.GetStringSlice("strs", nil))
	strs, err := request.RequireStringSlice("strs")
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b"}, strs)
}

func TestCallToolRequestArgumentsBuiltInProcess(t *testing.T) {
	// A handler invoked in process sees Go-typed values rather than what
	// encoding/json produces; the helpers must read those too.
	request := CallToolRequest{}
	request.Params.Arguments = map[string]any{
		"n":         3,
		"zero":      0,
		"ints":      []int{1, 2},
		"floats":    []float64{1.5},
		"bools":     []bool{true},
		"strs":      []string{"a"},
		"anyInts":   []any{1, 2.0, "3"},
		"anyFloats": []any{1, 2.5, "3.5"},
		"anyBools":  []any{1, 0.0, "true", false},
	}

	n, err := request.RequireInt("n")
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	assert.Equal(t, 3.0, request.GetFloat("n", -1))
	f, err := request.RequireFloat("n")
	require.NoError(t, err)
	assert.Equal(t, 3.0, f)

	assert.True(t, request.GetBool("n", false))
	b, err := request.RequireBool("n")
	require.NoError(t, err)
	assert.True(t, b)
	b, err = request.RequireBool("zero")
	require.NoError(t, err)
	assert.False(t, b)

	assert.Equal(t, []int{1, 2}, request.GetIntSlice("ints", nil))
	ints, err := request.RequireIntSlice("ints")
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2}, ints)
	assert.Equal(t, []int{1, 2, 3}, request.GetIntSlice("anyInts", nil))
	ints, err = request.RequireIntSlice("anyInts")
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3}, ints)

	assert.Equal(t, []float64{1.5}, request.GetFloatSlice("floats", nil))
	floats, err := request.RequireFloatSlice("floats")
	require.NoError(t, err)
	assert.Equal(t, []float64{1.5}, floats)
	assert.Equal(t, []float64{1, 2.5, 3.5}, request.GetFloatSlice("anyFloats", nil))
	floats, err = request.RequireFloatSlice("anyFloats")
	require.NoError(t, err)
	assert.Equal(t, []float64{1, 2.5, 3.5}, floats)

	assert.Equal(t, []bool{true}, request.GetBoolSlice("bools", nil))
	bools, err := request.RequireBoolSlice("bools")
	require.NoError(t, err)
	assert.Equal(t, []bool{true}, bools)
	assert.Equal(t, []bool{true, false, true, false}, request.GetBoolSlice("anyBools", nil))
	bools, err = request.RequireBoolSlice("anyBools")
	require.NoError(t, err)
	assert.Equal(t, []bool{true, false, true, false}, bools)

	assert.Equal(t, []string{"a"}, request.GetStringSlice("strs", nil))
	strs, err := request.RequireStringSlice("strs")
	require.NoError(t, err)
	assert.Equal(t, []string{"a"}, strs)
}

func TestCallToolRequestRequireRefusesUnconvertibleArguments(t *testing.T) {
	request := CallToolRequest{}
	request.Params.Arguments = map[string]any{
		"n":      1,
		"word":   "abc",
		"ints":   []any{"x"},
		"floats": []any{"x"},
		"bools":  []any{"x"},
		"objs":   []any{map[string]any{}},
	}

	tests := []struct {
		name string
		call func() error
		want string
	}{
		{"string from a number", func() error { _, err := request.RequireString("n"); return err }, `argument "n" is not a string`},
		{"int from a word", func() error { _, err := request.RequireInt("word"); return err }, `argument "word" cannot be converted to int`},
		{"float from a word", func() error { _, err := request.RequireFloat("word"); return err }, `argument "word" cannot be converted to float64`},
		{"bool from a word", func() error { _, err := request.RequireBool("word"); return err }, `argument "word" cannot be converted to bool`},
		{"string slice from a scalar", func() error { _, err := request.RequireStringSlice("n"); return err }, `argument "n" is not a string slice`},
		{"int slice from a scalar", func() error { _, err := request.RequireIntSlice("n"); return err }, `argument "n" is not an int slice`},
		{"int slice with an unconvertible item", func() error { _, err := request.RequireIntSlice("ints"); return err }, `item 0 in argument "ints" cannot be converted to int`},
		{"int slice with an object item", func() error { _, err := request.RequireIntSlice("objs"); return err }, `item 0 in argument "objs" is not an int`},
		{"float slice from a scalar", func() error { _, err := request.RequireFloatSlice("n"); return err }, `argument "n" is not a float64 slice`},
		{"float slice with an unconvertible item", func() error { _, err := request.RequireFloatSlice("floats"); return err }, `item 0 in argument "floats" cannot be converted to float64`},
		{"float slice with an object item", func() error { _, err := request.RequireFloatSlice("objs"); return err }, `item 0 in argument "objs" is not a float64`},
		{"bool slice from a scalar", func() error { _, err := request.RequireBoolSlice("n"); return err }, `argument "n" is not a bool slice`},
		{"bool slice with an unconvertible item", func() error { _, err := request.RequireBoolSlice("bools"); return err }, `item 0 in argument "bools" cannot be converted to bool`},
		{"bool slice with an object item", func() error { _, err := request.RequireBoolSlice("objs"); return err }, `item 0 in argument "objs" is not a bool`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.EqualError(t, tt.call(), tt.want)
		})
	}
}

func TestCallToolParamsWithoutOrUnencodableArguments(t *testing.T) {
	var params CallToolParams
	require.NoError(t, json.Unmarshal([]byte(`{"name":"ping"}`), &params))
	assert.Equal(t, "ping", params.Name)
	assert.Nil(t, params.Arguments)
	assert.Nil(t, params.RawArguments)
	assert.Error(t, json.Unmarshal([]byte(`{"name":`), &params))

	request := CallToolRequest{}
	request.Params.Arguments = map[string]any{"ch": make(chan int)}
	var target struct {
		Ch int `json:"ch"`
	}
	assert.ErrorContains(t, request.BindArguments(&target), "failed to marshal arguments")
}
