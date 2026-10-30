package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ones is n items of 1, as the text of a JSON array.
func ones(n int) string {
	return `[` + strings.TrimSuffix(strings.Repeat(`1,`, n), `,`) + `]`
}

// tooMany is the default refusal of a call past limit.
func tooMany(limit int) string {
	return "input schema validation failed: <root>: arguments hold more than " + strconv.Itoa(limit) + " values"
}

// valuesServer serves a tool free of any constraint, with the options opts.
func valuesServer(opts ...ServerOption) *MCPServer {
	srv := NewMCPServer("test", "1.0.0", append([]ServerOption{WithToolCapabilities(false)}, opts...)...)
	srv.AddTool(mcp.NewToolWithRawSchema("free", "", json.RawMessage(`{"type":"object"}`)), okHandler)
	return srv
}

// The cap holds at its edge whichever way validation reads the arguments: the
// scan of their text, or the tree a hook or a number with an exponent leaves.
// The object holding the array counts one.
func TestMaxValidatedValuesHoldsAtItsEdge(t *testing.T) {
	const limit = 5
	hooks := &Hooks{}
	hooks.AddBeforeCallTool(func(context.Context, any, *mcp.CallToolRequest) {})
	for name, srv := range map[string]*MCPServer{
		"scanned": valuesServer(WithInputSchemaValidation(), WithMaxValidatedValues(limit)),
		"hooked":  valuesServer(WithInputSchemaValidation(), WithMaxValidatedValues(limit), WithHooks(hooks)),
	} {
		t.Run(name, func(t *testing.T) {
			assert.Contains(t, boundsCall(t, srv, "free", `{"n":`+ones(4)+`}`), `"text":"ok"`)
			assert.Equal(t, tooMany(limit), resultText(t, srv, "free", `{"n":`+ones(5)+`}`))
			assert.Contains(t, boundsCall(t, srv, "free", `{"n":`+ones(5)+`}`), `"isError":true`)
		})
	}
	t.Run("exponent", func(t *testing.T) {
		srv := valuesServer(WithInputSchemaValidation(), WithMaxValidatedValues(limit))
		assert.Contains(t, boundsCall(t, srv, "free", `{"n":[1e0,1,1,1]}`), `"text":"ok"`)
		assert.Equal(t, tooMany(limit), resultText(t, srv, "free", `{"n":[1e0,1,1,1,1]}`))
	})
}

// A long string is one value, whatever it holds: the cap is on values, not on
// bytes.
func TestMaxValidatedValuesCountsAStringOnce(t *testing.T) {
	srv := valuesServer(WithInputSchemaValidation(), WithMaxValidatedValues(3))
	text := strings.Repeat(`a,[{`, 1000)
	assert.Contains(t, boundsCall(t, srv, "free", `{"a":"`+text+`","b":"`+text+`"}`), `"text":"ok"`)
}

// Without a repeated member name, valuesPast counts of a tree what
// scanArguments counts of its text, so a call has one verdict whichever way it
// is read.
func TestValuesPastAgreesWithTheScan(t *testing.T) {
	for _, raw := range []string{
		`{}`, `[]`, `[[]]`, `[{}]`, `[[],[]]`, `{"a":{}}`, `{"a":[],"b":{}}`, `{"a":1}`, `{"a":1,"b":2}`,
		`[1,2,3]`, `[[1],[2,3],{}]`, `{"a":[1,{"b":[2,3]}],"c":"x,y"}`, `"s"`, `7`, `null`, `true`,
		`{"a,b":"[{,"}`, `{"a":"\",[","b":[true,false,null]}`, ` [ 1 , 2 ] `,
	} {
		var value any
		require.NoError(t, json.Unmarshal([]byte(raw), &value), raw)
		for limit := 1; limit < 12; limit++ {
			room := limit
			exact, within := scanArguments([]byte(raw), limit)
			require.True(t, exact, raw)
			assert.Equal(t, !valuesPast(value, &room), within, "%s at %d", raw, limit)
		}
	}
}

// The text counts a repeated member name and the tree doesn't: the scan sends
// such a call to the tree, which judges it, so it is not refused for members it
// doesn't hold.
func TestMaxValidatedValuesCountsARepeatedNameOnce(t *testing.T) {
	const raw = `{"a":1,"a":1,"a":1}`
	_, within := scanArguments([]byte(raw), 2)
	assert.False(t, within)
	srv := valuesServer(WithInputSchemaValidation(), WithMaxValidatedValues(2))
	assert.Contains(t, boundsCall(t, srv, "free", raw), `"text":"ok"`)
}

// A call past the cap and past another bound gets the cap's answer, whichever
// way it is read.
func TestMaxValidatedValuesComesFirst(t *testing.T) {
	hooks := &Hooks{}
	hooks.AddBeforeCallTool(func(context.Context, any, *mcp.CallToolRequest) {})
	for name, opts := range map[string][]ServerOption{
		"plain":  {WithInputSchemaValidation(), WithMaxValidatedValues(3)},
		"hooked": {WithInputSchemaValidation(), WithMaxValidatedValues(3), WithHooks(hooks)},
	} {
		srv := valuesServer(opts...)
		number := `0.` + strings.Repeat(`7`, maxNumberDigits+1)
		assert.Equal(t, tooMany(3), resultText(t, srv, "free", `{"n":[1,1,1,`+number+`]}`), name)
		deep := strings.Repeat(`[`, maxArgumentDepth+1) + strings.Repeat(`]`, maxArgumentDepth+1)
		assert.Equal(t, tooMany(3), resultText(t, srv, "free", `{"n":`+deep+`}`), name)
	}
}

// Over streamable HTTP, on the modern era, a refusal is a tool execution error,
// not a protocol error.
func TestStreamableHTTPRefusesACallPastTheCap(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation(), WithMaxValidatedValues(2))
	srv.AddTool(mcp.NewToolWithRawSchema("echo", "", json.RawMessage(`{"type":"object"}`)), okHandler)
	body, header := modernToolCall(t, map[string]any{"n": []any{1, 2}})
	w := serveOne(NewStreamableHTTPServer(srv), body, header)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"isError":true`)
	assert.Contains(t, w.Body.String(), "arguments hold more than 2 values")
}

// A task's call is under the cap too: the task ends with the refusal.
func TestMaxValidatedValuesOnATask(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation(),
		WithMaxValidatedValues(2), WithTaskCapabilities(true, true, true))
	tool := mcp.NewToolWithRawSchema("slow", "", json.RawMessage(`{"type":"object"}`))
	tool.Execution = &mcp.ToolExecution{TaskSupport: mcp.TaskSupportOptional}
	srv.AddTool(tool, okHandler)
	created, reqErr := srv.handleTaskAugmentedToolCall(t.Context(), 1, mcp.CallToolRequest{Params: mcp.CallToolParams{
		Name: "slow", Arguments: map[string]any{"n": []any{1, 2}}, Task: &mcp.TaskParams{},
	}})
	require.Nil(t, reqErr)
	result, reqErr := srv.handleTaskResult(t.Context(), 2, mcp.TaskResultRequest{Params: mcp.TaskResultParams{TaskId: created.Task.TaskId}})
	require.Nil(t, reqErr)
	require.True(t, result.IsError)
	assert.Equal(t, tooMany(2), result.Content[0].(mcp.TextContent).Text)
}

// Without a cap the scan judges depth alone.
func TestScanArgumentsTakesNoCap(t *testing.T) {
	for _, limit := range []int{0, -1} {
		_, within := scanArguments([]byte(ones(1000)), limit)
		assert.True(t, within, "%d", limit)
		_, within = scanArguments([]byte(strings.Repeat(`[`, maxArgumentDepth+1)+strings.Repeat(`]`, maxArgumentDepth+1)), limit)
		assert.False(t, within, "%d", limit)
	}
}

// The default holds, and a call within it passes.
func TestMaxValidatedValuesDefault(t *testing.T) {
	assert.Equal(t, 100_000, DefaultMaxValidatedValues)
	srv := valuesServer(WithInputSchemaValidation())
	assert.Contains(t, boundsCall(t, srv, "free", `{"n":`+ones(DefaultMaxValidatedValues-1)+`}`), `"text":"ok"`)
	assert.Equal(t, tooMany(DefaultMaxValidatedValues), resultText(t, srv, "free", `{"n":`+ones(DefaultMaxValidatedValues)+`}`))
}

// Zero or less removes the cap.
func TestMaxValidatedValuesRemoved(t *testing.T) {
	for _, n := range []int{0, -1} {
		srv := valuesServer(WithInputSchemaValidation(), WithMaxValidatedValues(n))
		assert.Contains(t, boundsCall(t, srv, "free", `{"n":`+ones(DefaultMaxValidatedValues+1)+`}`), `"text":"ok"`, "%d", n)
	}
}

// The option's order among the options doesn't matter, and it turns nothing on.
func TestMaxValidatedValuesOrder(t *testing.T) {
	refuse := func(context.Context, mcp.Tool, CallArguments, InputValidationFailure) (*mcp.CallToolResult, bool) {
		return mcp.NewToolResultError("refused"), true
	}
	for name, opts := range map[string][]ServerOption{
		"before validation": {WithMaxValidatedValues(2), WithInputSchemaValidation()},
		"after validation":  {WithInputSchemaValidation(), WithMaxValidatedValues(2)},
		"before handler":    {WithMaxValidatedValues(2), WithInputValidationHandler(refuse)},
		"after handler":     {WithInputValidationHandler(refuse), WithMaxValidatedValues(2)},
	} {
		srv := valuesServer(opts...)
		assert.Equal(t, 2, srv.inputValidator.maxValues, name)
		assert.Contains(t, boundsCall(t, srv, "free", `{"n":`+ones(2)+`}`), `"isError":true`, name)
	}

	srv := valuesServer(WithMaxValidatedValues(2))
	assert.Nil(t, srv.inputValidator)
	assert.Contains(t, boundsCall(t, srv, "free", `{"n":`+ones(50)+`}`), `"text":"ok"`)
}

// A handler sees the cap as a number, and one that steps aside lets the call
// run, as for the other refusals made before the validator reads.
func TestMaxValidatedValuesReachesTheHandler(t *testing.T) {
	var seen InputValidationFailure
	var refuse bool
	srv := valuesServer(WithMaxValidatedValues(2), WithInputValidationHandler(
		func(_ context.Context, _ mcp.Tool, _ CallArguments, failure InputValidationFailure) (*mcp.CallToolResult, bool) {
			seen = failure
			return mcp.NewToolResultError("refused"), refuse
		}))

	assert.Contains(t, boundsCall(t, srv, "free", `{"n":`+ones(2)+`}`), `"text":"ok"`)
	limit := 2
	assert.Equal(t, InputValidationFailure{Incomplete: true, Violations: []InputViolation{{
		Keyword:  "tooManyValues",
		Expected: ViolationExpected{Limit: &limit},
		Actual:   ViolationActual{Kind: "object"},
		Message:  "<root>: arguments hold more than 2 values",
	}}}, seen)

	refuse = true
	assert.Contains(t, boundsCall(t, srv, "free", `{"n":`+ones(2)+`}`), `"text":"refused"`)
}

// A refused call never reaches the validator and is never decoded again for
// its numbers: it costs what decoding it once does, not what recording its
// failures would.
func TestMaxValidatedValuesKeepsTheValidatorOut(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	const values = 100_000
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation(), WithMaxValidatedValues(values-1))
	srv.AddTool(mcp.NewToolWithRawSchema("texts", "", json.RawMessage(
		`{"type":"object","properties":{"n":{"type":"array","items":{"type":"string"}}}}`)), okHandler)
	for name, array := range map[string]string{
		"exact":    ones(values),
		"exponent": strings.Replace(ones(values), `1]`, `1e0]`, 1),
	} {
		message := json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"texts","arguments":{"n":` + array + `}}}`)
		require.Equal(t, tooMany(values-1), resultText(t, srv, "texts", `{"n":`+array+`}`), name)
		allocs := testing.AllocsPerRun(1, func() { srv.HandleMessage(t.Context(), message) })
		assert.LessOrEqual(t, allocs, float64(values*5/2), "%s: allocations to refuse %d failing values", name, values)
	}
}

// A tool whose schema registration didn't keep is read per call, under the cap
// all the same.
func TestMaxValidatedValuesWithoutAKeptSchema(t *testing.T) {
	v := newInputSchemaValidator()
	v.maxValues = 2
	tool := mcp.NewToolWithRawSchema("free", "", json.RawMessage(`{"type":"object"}`))
	var value any
	require.NoError(t, json.Unmarshal([]byte(`{"n":[1,2]}`), &value))
	ran, err := v.validateRegistered(tool, nil, false, callArguments{raw: json.RawMessage(`{"n":[1,2]}`), decoded: value})
	assert.True(t, ran)
	assert.ErrorContains(t, err, "more than 2 values")
}

// The default text knows the failure as its own.
func TestTooManyValuesText(t *testing.T) {
	text, known := formatValidationText(&tooManyValues{limit: 5})
	assert.Equal(t, tooMany(5), text)
	assert.True(t, known)
}

// Finding the values within the cap allocates nothing.
func TestValuesPastAllocations(t *testing.T) {
	value := map[string]any{"a": []any{json.Number("1"), "s", map[string]any{"b": true}}, "c": []any{}}
	count := func() bool {
		room := 100
		return valuesPast(value, &room)
	}
	assert.False(t, count())
	assert.Zero(t, testing.AllocsPerRun(100, func() { _ = count() }))
}
