package server

import (
	"context"
	"encoding/json"
	"math"
	"math/big"
	"strings"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Literals past the number bounds, each in float64's range, so the request's
// decode lets it through to validation.
func numbersPastBounds() []string {
	return []string{
		`1e-999999`, `-1e-1000000`, `1e-1101`, `1.5e-1100`, `1e-99999999999999999999`,
		`0.` + strings.Repeat(`0`, 1099) + `1`,
		`0.` + strings.Repeat(`7`, 1101),
		`-0.` + strings.Repeat(`0`, 1100),
		`25` + strings.Repeat(`0`, 1000) + `e-99999`,
	}
}

// exactSmallest is the exact decimal of the smallest float64, in fixed and in
// scientific notation.
func exactSmallest() (fixed, scientific string) {
	fixed = new(big.Rat).SetFloat64(math.SmallestNonzeroFloat64).FloatString(1074)
	digits := strings.TrimLeft(strings.TrimPrefix(fixed, "0."), "0")
	return fixed, digits[:1] + "." + digits[1:] + "e-324"
}

// Numbers a client sends: whatever float64 holds, written shortest or
// exactly, and every integer it reaches.
func numbersWithinBounds() []string {
	fixed, scientific := exactSmallest()
	maxFloat := new(big.Rat).SetFloat64(math.MaxFloat64).FloatString(0)
	return []string{
		`0`, `-0`, `1.5`, `5e-324`, `4.9406564584124654e-324`, `1.7976931348623157e308`, `9007199254740993`,
		fixed, scientific, maxFloat, maxFloat + `.0`, `1e-1100`, `1e308`, `-25e-1099`, `0e1000000`,
		`115792089237316195423570985008687907853269984665640564039457584007913129639935`,
		`1` + strings.Repeat(`0`, 308),
		`0.` + strings.Repeat(`7`, 1099),
		`7` + strings.Repeat(`7`, 1099) + `e-1100`,
	}
}

// boundsCall answers a tools/call of tool with arguments through HandleMessage.
func boundsCall(t *testing.T, srv *MCPServer, tool, arguments string) string {
	t.Helper()
	return handled(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"`+tool+`","arguments":`+arguments+`}}`)
}

// resultText is the text of the one content block of the tools/call result
// srv answers for arguments.
func resultText(t *testing.T, srv *MCPServer, tool, arguments string) string {
	t.Helper()
	var answer struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal([]byte(boundsCall(t, srv, tool, arguments)), &answer))
	require.Len(t, answer.Result.Content, 1)
	return answer.Result.Content[0].Text
}

// refusedNumber is the refusal of literal at pointer, the literal cut after
// 64 bytes.
func refusedNumber(pointer, literal string) string {
	if len(literal) > 64 {
		literal = literal[:64] + "..."
	}
	return "input schema validation failed: " + pointer + ": number " + literal + " is out of range"
}

// A number past the bounds fails validation as out of range before the
// validator reads it, under every keyword that reads a number, and where no
// keyword reads it.
func TestInputValidationRefusesANumberPastItsBounds(t *testing.T) {
	keywords := map[string]string{
		"integer":          `{"type":"integer"}`,
		"minimum":          `{"type":"number","minimum":0}`,
		"maximum":          `{"maximum":5}`,
		"exclusiveMinimum": `{"exclusiveMinimum":0}`,
		"exclusiveMaximum": `{"exclusiveMaximum":5}`,
		"multipleOf":       `{"multipleOf":0.5}`,
		"enum":             `{"enum":[1,2,3]}`,
		"const":            `{"const":1}`,
		"free":             `{}`,
	}
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation())
	for name, schema := range keywords {
		srv.AddTool(mcp.NewToolWithRawSchema(name, "", json.RawMessage(`{"type":"object","properties":{"n":`+schema+`}}`)), okHandler)
	}
	srv.AddTool(mcp.NewToolWithRawSchema("unique", "", json.RawMessage(
		`{"type":"object","properties":{"n":{"type":"array","uniqueItems":true}}}`)), okHandler)
	for _, literal := range numbersPastBounds() {
		for name := range keywords {
			out := boundsCall(t, srv, name, `{"n":`+literal+`}`)
			assert.Contains(t, out, `"isError":true`, "%s %.40s", name, literal)
			assert.Equal(t, refusedNumber("/n", literal), resultText(t, srv, name, `{"n":`+literal+`}`), "%s %.40s", name, literal)
		}
		assert.Equal(t, refusedNumber("/n/2", literal), resultText(t, srv, "unique", `{"n":[1,2,`+literal+`]}`), "unique %.40s", literal)
	}
	assert.Equal(t, "input schema validation failed: /n: number 1e-1101 is out of range",
		resultText(t, srv, "integer", `{"n":1e-1101}`))
	assert.Equal(t, "input schema validation failed: /n: number 0."+strings.Repeat("7", 62)+"... is out of range",
		resultText(t, srv, "free", `{"n":0.`+strings.Repeat(`7`, 1101)+`}`))
}

// hookedServer validates n as a number, with hooks before every request, so
// validation reads the arguments as the hooks leave them: unchanged by a
// no-op, or with n the literal leave holds, when it holds one.
func hookedServer(leave json.Number) *MCPServer {
	hooks := &Hooks{}
	hooks.AddBeforeAny(func(context.Context, any, mcp.MCPMethod, any) {})
	if leave != "" {
		hooks.AddBeforeCallTool(func(_ context.Context, _ any, r *mcp.CallToolRequest) {
			r.Params.Arguments = map[string]any{"n": leave}
		})
	}
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation(), WithHooks(hooks))
	srv.AddTool(mcp.NewToolWithRawSchema("hooked", "", json.RawMessage(`{"type":"object","properties":{"n":{"type":"number"}}}`)), okHandler)
	return srv
}

// A number past the bounds that a hook leaves is refused as one a client
// sends is.
func TestInputValidationRefusesANumberPastItsBoundsUnderAHook(t *testing.T) {
	assert.Equal(t, "input schema validation failed: /n: number 1e-1101 is out of range",
		resultText(t, hookedServer("1e-1101"), "hooked", `{"n":1}`))
}

// Every number a client sends is validated as it was: none is out of range.
func TestInputValidationKeepsTheNumbersClientsSend(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation())
	srv.AddTool(mcp.NewToolWithRawSchema("kept", "", json.RawMessage(
		`{"type":"object","properties":{"n":{"type":"number","minimum":-1e1200,"maximum":1e1200}}}`)), okHandler)
	srv.AddTool(mcp.NewToolWithRawSchema("integer", "", json.RawMessage(
		`{"type":"object","properties":{"i":{"type":"integer"}}}`)), okHandler)
	for _, literal := range numbersWithinBounds() {
		assert.Contains(t, boundsCall(t, srv, "kept", `{"n":`+literal+`}`), `"text":"ok"`, "%.40s", literal)
	}
	fixed, _ := exactSmallest()
	assert.Equal(t, "input schema validation failed: /i: got number, want integer", resultText(t, srv, "integer", `{"i":`+fixed+`}`))
	maxFloat := new(big.Rat).SetFloat64(math.MaxFloat64).FloatString(0)
	assert.Contains(t, boundsCall(t, srv, "integer", `{"i":`+maxFloat+`}`), `"text":"ok"`)
}

// Output validation reads a handler's numbers as it always did: the bounds
// are the arguments' alone.
func TestOutputValidationKeepsABigNumber(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithOutputSchemaValidation())
	tool := mcp.NewTool("big")
	tool.RawOutputSchema = json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`)
	n := new(big.Int).Lsh(big.NewInt(1), 4300)
	require.Len(t, n.String(), 1295)
	srv.AddTool(tool, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		result := mcp.NewToolResultText("ok")
		result.StructuredContent = map[string]any{"n": n}
		return result, nil
	})
	out := boundsCall(t, srv, "big", `{}`)
	assert.Contains(t, out, `"text":"ok"`)
	assert.NotContains(t, out, `"isError":true`)

	small := outOfRangeServer(t, nil, json.RawMessage(`{"n":1e-1101}`))
	assert.Contains(t, handled(t, small, outOfRangeCall(`{"limit":1}`)), `"text":"ok"`)
}

// A call of a thousand numbers at a scale of a million is refused before the
// validator reads one: the refusal allocates what decoding the call does.
func TestInputValidationBoundsTheWorkANumberTakes(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation())
	srv.AddTool(mcp.NewToolWithRawSchema("ints", "", json.RawMessage(
		`{"type":"object","properties":{"n":{"type":"array","items":{"type":"integer"}}}}`)), okHandler)
	message := json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"ints","arguments":{"n":[` +
		strings.TrimSuffix(strings.Repeat(`1e-999999,`, 1000), ",") + `]}}}`)
	require.Equal(t, refusedNumber("/n/0", "1e-999999"), resultText(t, srv, "ints", `{"n":[1e-999999]}`))
	allocs := testing.AllocsPerRun(1, func() { srv.HandleMessage(t.Context(), message) })
	assert.LessOrEqual(t, allocs, 6000.0, "allocations to refuse 1000 numbers at a scale of a million")
}

// The bounds hold at their edges: 1100 digits and a scale of 1100 either way
// are within, one more is past; a zero is past only by its digits; the
// exponent saturates rather than wraps; a literal that is no JSON number's is
// big.Rat's to judge.
func TestNumberPastBounds(t *testing.T) {
	for literal, past := range map[string]bool{
		`0`: false, `-1.5`: false, `1e-1100`: false, `1e-1101`: true, `1e1100`: false, `1E+1101`: true,
		`-15e-1100`: false, `1.5e-1100`: true, `0.5e1101`: false, `0.5e1102`: true,
		`1` + strings.Repeat(`0`, 1099): false, `1` + strings.Repeat(`0`, 1100): true,
		`0.` + strings.Repeat(`0`, 1098) + `1`: false, `0.` + strings.Repeat(`0`, 1099) + `1`: true,
		`0.` + strings.Repeat(`0`, 500) + `1e-599`: false, `0.` + strings.Repeat(`0`, 500) + `1e-700`: true,
		`0e1000000`: false, `-0.0e-99999999999999999999`: false, `0.` + strings.Repeat(`0`, 1100): true,
		`1e99999999999999999999999999999999999999`: true, `1e-99999999999999999999999999999999999999`: true,
		`0.` + strings.Repeat(`7`, 1099) + `e2199`: false, `0.` + strings.Repeat(`7`, 1099) + `e2200`: true,
		`0.` + strings.Repeat(`7`, 1099) + `e99999999999`: true, `-7` + strings.Repeat(`7`, 1099) + `e-1100`: false,
		`0x1p-99999`: false, `1/3`: false, `1e5x`: false,
	} {
		assert.Equal(t, past, numberPastBounds(literal), "%.40s", literal)
	}
}

// Finding nothing past the bounds allocates nothing.
func TestArgumentsPastBoundsAllocations(t *testing.T) {
	value := map[string]any{"a": json.Number("1.5"), "b": []any{json.Number("5e-324"), "s", true, nil, 2.5}, "c": map[string]any{"d": json.Number("-3")}}
	assert.False(t, argumentsPastBounds(value, 1))
	assert.Zero(t, testing.AllocsPerRun(100, func() { _ = argumentsPastBounds(value, 1) }))
}
