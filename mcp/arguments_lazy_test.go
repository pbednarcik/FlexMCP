package mcp

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/internal/argsdecode"
)

// decodeOnDemand decodes data, tools/call params, as the server decodes a
// call to a tool that reads its arguments on demand.
func decodeOnDemand(data []byte) (CallToolRequest, error) {
	var r CallToolRequest
	argsdecode.SetOnDemand(&r.Params, func(string) bool { return true })
	err := json.Unmarshal(data, &r.Params)
	argsdecode.SetOnDemand(&r.Params, nil)
	return r, err
}

// decodeEager decodes data, tools/call params, as every call was decoded
// before the opt-in.
func decodeEager(data []byte) (CallToolRequest, error) {
	var r CallToolRequest
	err := json.Unmarshal(data, &r.Params)
	return r, err
}

// checkOnDemandDiff requires a call decoded on demand to fail as the call
// decoded at once fails, and otherwise to give through every reader of its
// arguments what the call decoded at once gives.
func checkOnDemandDiff(t *testing.T, arguments []byte) {
	t.Helper()
	params := append(append([]byte(`{"name":"echo","arguments":`), arguments...), '}')
	want, wantErr := decodeEager(params)
	got, gotErr := decodeOnDemand(params)
	if wantErr != nil {
		require.EqualError(t, gotErr, wantErr.Error(), "%q", arguments)
		return
	}
	require.NoError(t, gotErr, "%q", arguments)
	require.Nil(t, got.Params.onDemand, "%q: the decode keeps no lookup", arguments)
	require.Equal(t, want.Params.RawArguments, got.Params.RawArguments, "%q", arguments)
	if want.Params.Arguments != nil {
		require.Nil(t, got.Params.Arguments, "%q: Params.Arguments is left nil", arguments)
	}
	require.Equal(t, want.GetArguments(), got.GetArguments(), "%q", arguments)
	requireSameFloatBits(t, want.GetArguments(), got.GetArguments(), arguments)
	require.Equal(t, want.GetRawArguments(), got.GetRawArguments(), "%q", arguments)
	var wantBound, gotBound any
	wantBindErr, gotBindErr := want.BindArguments(&wantBound), got.BindArguments(&gotBound)
	require.Equal(t, wantBindErr, gotBindErr, "%q", arguments)
	require.Equal(t, wantBound, gotBound, "%q", arguments)
	encodedWant, err := json.Marshal(want.Params)
	require.NoError(t, err)
	encodedGot, err := json.Marshal(got.Params)
	require.NoError(t, err)
	require.Equal(t, string(encodedWant), string(encodedGot), "%q", arguments)
}

// A call decoded on demand gives what a call decoded at once gives, through
// every reader of its arguments, for every arguments corpus input; one that
// fails, fails with the same error.
func TestArgumentsOnDemandMatchEager(t *testing.T) {
	for _, s := range numbersDiffCorpus(t) {
		checkOnDemandDiff(t, []byte(s))
	}
}

// Every getter answers on demand what it answers on the map decoded at once.
func TestArgumentsOnDemandGetters(t *testing.T) {
	params := []byte(`{"name":"echo","arguments":{"s":"x","i":7,"f":1.5,"b":true,"ss":["a","b"],"is":[1,2],"fs":[0.5,-0],` +
		`"bs":[true,false],"n":null,"o":{"k":"v"},"bad":"7"}}`)
	want, err := decodeEager(params)
	require.NoError(t, err)
	got, err := decodeOnDemand(params)
	require.NoError(t, err)
	require.Nil(t, got.Params.Arguments)
	for _, key := range []string{"s", "i", "f", "b", "ss", "is", "fs", "bs", "n", "o", "bad", "missing"} {
		pair := func(f func(r CallToolRequest) any) {
			t.Helper()
			assert.Equal(t, f(want), f(got), "key %q", key)
		}
		pair(func(r CallToolRequest) any { return r.GetString(key, "d") })
		pair(func(r CallToolRequest) any { v, err := r.RequireString(key); return []any{v, err} })
		pair(func(r CallToolRequest) any { return r.GetInt(key, -1) })
		pair(func(r CallToolRequest) any { v, err := r.RequireInt(key); return []any{v, err} })
		pair(func(r CallToolRequest) any { return r.GetFloat(key, -1) })
		pair(func(r CallToolRequest) any { v, err := r.RequireFloat(key); return []any{v, err} })
		pair(func(r CallToolRequest) any { return r.GetBool(key, true) })
		pair(func(r CallToolRequest) any { v, err := r.RequireBool(key); return []any{v, err} })
		pair(func(r CallToolRequest) any { return r.GetStringSlice(key, nil) })
		pair(func(r CallToolRequest) any { v, err := r.RequireStringSlice(key); return []any{v, err} })
		pair(func(r CallToolRequest) any { return r.GetIntSlice(key, nil) })
		pair(func(r CallToolRequest) any { v, err := r.RequireIntSlice(key); return []any{v, err} })
		pair(func(r CallToolRequest) any { return r.GetFloatSlice(key, nil) })
		pair(func(r CallToolRequest) any { v, err := r.RequireFloatSlice(key); return []any{v, err} })
		pair(func(r CallToolRequest) any { return r.GetBoolSlice(key, nil) })
		pair(func(r CallToolRequest) any { v, err := r.RequireBoolSlice(key); return []any{v, err} })
		pair(func(r CallToolRequest) any { return ParseArgument(r, key, "d") })
	}
}

// A call decoded at once, as for a tool that did not opt in, is decoded as
// before: its Arguments are set and nothing is held for later.
func TestArgumentsOnDemandOnlyWhenAsked(t *testing.T) {
	params := []byte(`{"name":"echo","arguments":{"a":1}}`)
	var r CallToolRequest
	argsdecode.SetOnDemand(&r.Params, func(name string) bool { return name != "echo" })
	require.NoError(t, json.Unmarshal(params, &r.Params))
	assert.Equal(t, map[string]any{"a": 1.0}, r.Params.Arguments)
	assert.Nil(t, r.Params.lazy)
}

// A map a hook assigns to Params.Arguments is what the getters return.
func TestArgumentsOnDemandAssignedArgumentsWin(t *testing.T) {
	r, err := decodeOnDemand([]byte(`{"name":"echo","arguments":{"a":"sent"}}`))
	require.NoError(t, err)
	r.Params.Arguments = map[string]any{"a": "assigned"}
	assert.Equal(t, "assigned", r.GetString("a", ""))
}

// Copies of one request, read from many goroutines, share one map, decoded
// once.
func TestArgumentsOnDemandDecodedOnce(t *testing.T) {
	r, err := decodeOnDemand([]byte(`{"name":"echo","arguments":{"a":"x","b":[1,2]}}`))
	require.NoError(t, err)
	maps := make([]map[string]any, 16)
	var wg sync.WaitGroup
	for i := range maps {
		wg.Go(func() {
			request := r
			_ = request.GetString("a", "")
			maps[i] = request.GetArguments()
		})
	}
	wg.Wait()
	for _, m := range maps {
		assert.Equal(t, reflect.ValueOf(maps[0]).Pointer(), reflect.ValueOf(m).Pointer(), "every copy reads the same map")
	}
	assert.Equal(t, "x", maps[0]["a"])
}

// The range scan parses only literals that can be out of range: one with an
// exponent, or of more than 308 digits.
func TestArgumentNumbersInRange(t *testing.T) {
	for raw, want := range map[string]bool{
		`{"a":1,"b":[2.5,-0,{"c":"1e400"}]}`: true,
		`{"a":1e308}`:                        true,
		`{"a":1e309}`:                        false,
		`[1,-1E+400,2]`:                      false,
		`{"a":[[[1e-400]]]}`:                 true,
		`{"s\"1e400":"\\","n":1}`:            true,
	} {
		assert.Equal(t, want, argumentNumbersInRange([]byte(raw)), raw)
	}
	// Strings of every length up to 19, with an escape at every place in
	// them, then a number.
	for n := range 20 {
		for at := -1; at < n; at++ {
			for _, esc := range []string{`\"`, `\\`, `\\u0022`, `\n`} {
				body := []byte(strings.Repeat("x", n))
				if at >= 0 {
					body = append(body[:at:at], append([]byte(esc), body[at:]...)...)
				}
				for _, tail := range []string{`1e400`, `1e5`, `[1e400]`, `{"k":1e400}`} {
					for _, raw := range []string{
						`["` + string(body) + `",` + tail + `]`,
						`{"` + string(body) + `":` + tail + `}`,
						`["` + string(body) + `","1e400",` + tail + `,"` + string(body) + `"]`,
					} {
						var v any
						want := json.Unmarshal([]byte(raw), &v) == nil
						require.Equal(t, want, argumentNumbersInRange([]byte(raw)), raw)
					}
				}
			}
		}
	}
	nines := make([]byte, 309)
	for i := range nines {
		nines[i] = '9'
	}
	assert.True(t, argumentNumbersInRange(nines[:308]))
	assert.False(t, argumentNumbersInRange(nines))
	if !raceEnabled {
		plain := []byte(`{"items":[{"id":1,"name":"x","tags":["a","b"]}],"n":-2.5,"s":"e\"E"}`)
		assert.Zero(t, testing.AllocsPerRun(100, func() { argumentNumbersInRange(plain) }))
	}
}

// FuzzArgumentNumbersInRange holds the range scan to json.Unmarshal into an
// any, for any valid JSON text: it accepts exactly what json.Unmarshal does.
func FuzzArgumentNumbersInRange(f *testing.F) {
	for _, s := range numbersDiffCorpus(f) {
		if len(s) < 4096 {
			f.Add([]byte(s))
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if !json.Valid(data) {
			return
		}
		var v any
		want := json.Unmarshal(data, &v) == nil
		if got := argumentNumbersInRange(data); got != want {
			t.Fatalf("argumentNumbersInRange(%q) = %v, json.Unmarshal accepts it: %v", data, got, want)
		}
	})
}
