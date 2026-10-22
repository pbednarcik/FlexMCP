package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A number big.Rat cannot read: float64 reads it as 0, and jsonschema
// v6.0.2's validator, which reads every number it checks through big.Rat,
// panicked on it. Any client could send one.
const outOfRangeNumber = `0e10000000000000000000`

// outOfRangeServer validates a tool's limit as a number of at most 5, and its
// output's n the same way; the tool returns as its output what output holds.
func outOfRangeServer(t testing.TB, hooks *Hooks, output json.RawMessage) *MCPServer {
	t.Helper()
	opts := []ServerOption{WithToolCapabilities(false), WithInputSchemaValidation(), WithOutputSchemaValidation()}
	if hooks != nil {
		opts = append(opts, WithHooks(hooks))
	}
	srv := NewMCPServer("test", "1.0.0", opts...)
	tool := mcp.NewToolWithRawSchema("limited", "a bounded limit", json.RawMessage(
		`{"type":"object","properties":{"limit":{"type":"number","maximum":5}}}`))
	tool.RawOutputSchema = json.RawMessage(`{"type":"object","properties":{"n":{"type":"number","maximum":5}}}`)
	srv.AddTool(tool, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		result := mcp.NewToolResultText("ok")
		if output != nil {
			result.StructuredContent = output
		}
		return result, nil
	})
	return srv
}

// handled answers message through HandleMessage, failing t rather than the
// test binary when it panics.
func handled(t *testing.T, srv *MCPServer, message string) (out string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("HandleMessage panicked: %v", r)
		}
	}()
	b, err := json.Marshal(srv.HandleMessage(t.Context(), json.RawMessage(message)))
	require.NoError(t, err)
	return string(b)
}

func outOfRangeCall(arguments string) string {
	return `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"limited","arguments":` + arguments + `}}`
}

const outOfRangeFailure = `input schema validation failed: /limit: number ` + outOfRangeNumber + ` is out of range`

// A number the validator cannot read fails validation like any other value
// that does not fit the schema: a tool execution error naming it.
func TestInputValidationRefusesANumberItCannotRead(t *testing.T) {
	srv := outOfRangeServer(t, nil, nil)
	for _, literal := range []string{outOfRangeNumber, `0.0e-99999999999999999999`, `1e-1000001`, `-2.5e-9223372036854775808`} {
		out := handled(t, srv, outOfRangeCall(`{"limit":`+literal+`}`))
		assert.Contains(t, out, `"isError":true`, literal)
		assert.Contains(t, out, `/limit: number `+literal+` is out of range`, literal)
	}
	assert.Contains(t, handled(t, srv, outOfRangeCall(`{"limit":4}`)), `"text":"ok"`)
	assert.Contains(t, handled(t, srv, outOfRangeCall(`{"limit":0e1000000}`)), `"text":"ok"`)
}

// A hook that runs before the call may leave numbers the validator reads as
// written: they fail the same way.
func TestInputValidationRefusesANumberItCannotReadThatAHookLeft(t *testing.T) {
	h := &Hooks{}
	h.AddBeforeCallTool(func(_ context.Context, _ any, r *mcp.CallToolRequest) {
		r.Params.Arguments = map[string]any{"limit": json.Number(outOfRangeNumber)}
	})
	out := handled(t, outOfRangeServer(t, h, nil), outOfRangeCall(`{"limit":1}`))
	assert.Contains(t, out, `"isError":true`)
	assert.Contains(t, out, outOfRangeFailure)
}

// A handler's structured output that carries such a number, as a client
// sent it, fails output validation.
func TestOutputValidationRefusesANumberItCannotRead(t *testing.T) {
	srv := outOfRangeServer(t, nil, json.RawMessage(`{"n":`+outOfRangeNumber+`}`))
	out := handled(t, srv, outOfRangeCall(`{"limit":1}`))
	assert.Contains(t, out, `"isError":true`)
	assert.Contains(t, out, `output schema validation failed: /n: number `+outOfRangeNumber+` is out of range`)
}

// Over stdio the call is answered with the validation failure; it was
// answered with INTERNAL_ERROR, the panic recovered by the transport.
func TestStdioInputValidationRefusesANumberItCannotRead(t *testing.T) {
	stdin, stdinWriter := io.Pipe()
	stdoutReader, stdout := io.Pipe()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stdio := NewStdioServer(outOfRangeServer(t, nil, nil))
	stdio.SetErrorLogger(log.New(io.Discard, "", 0))
	listened := make(chan error, 1)
	go func() {
		listened <- stdio.Listen(ctx, stdin, stdout)
		_ = stdout.Close()
	}()
	go func() {
		for _, line := range []string{
			`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`,
			outOfRangeCall(`{"limit":` + outOfRangeNumber + `}`),
		} {
			if _, err := io.WriteString(stdinWriter, line+"\n"); err != nil {
				return
			}
		}
	}()
	var answer string
	scanner := bufio.NewScanner(stdoutReader)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), `"id":2`) {
			answer = scanner.Text()
			break
		}
	}
	require.NoError(t, scanner.Err())
	_ = stdinWriter.Close()
	go func() { _, _ = io.Copy(io.Discard, stdoutReader) }()
	require.NoError(t, <-listened)
	assert.Contains(t, answer, `"isError":true`)
	assert.Contains(t, answer, outOfRangeFailure)
}

// Over streamable HTTP the call is answered with the validation failure; the
// panic used to reach net/http, which dropped the connection unanswered.
func TestStreamableHTTPInputValidationRefusesANumberItCannotRead(t *testing.T) {
	ts := httptest.NewUnstartedServer(NewStreamableHTTPServer(outOfRangeServer(t, nil, nil)))
	ts.Config.ErrorLog = log.New(io.Discard, "", 0)
	ts.Start()
	defer ts.Close()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"_meta":{`+
		`"io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"test","version":"1.0"},`+
		`"io.modelcontextprotocol/protocolVersion":"2026-07-28"},"name":"limited","arguments":{"limit":%s}}}`, outOfRangeNumber)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ts.URL, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "limited")
	resp, err := ts.Client().Do(req)
	require.NoError(t, err, "the server dropped the connection")
	defer resp.Body.Close()
	answer, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(answer))
	assert.Contains(t, string(answer), `"isError":true`)
	assert.Contains(t, string(answer), outOfRangeFailure)
}

// Only a call whose number the validator reads fails: one in a member no
// numeric keyword reads still passes, as it always did.
func TestInputValidationPassesANumberItCannotReadWhereNoKeywordReadsIt(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation())
	srv.AddTool(mcp.NewToolWithRawSchema("loose", "a bounded limit and a free note", json.RawMessage(
		`{"type":"object","properties":{"limit":{"type":"number","maximum":5},"note":{}}}`)), okHandler)
	call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"loose","arguments":{"limit":1,"note":` + outOfRangeNumber + `}}}`
	assert.Contains(t, handled(t, srv, call), `"text":"ok"`)
}

// A schema whose bound big.Rat cannot read is refused at registration, as a
// schema that does not compile is, with the tool and the bound named.
func TestRegistrationNamesABoundItCannotRead(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation())
	tool := mcp.NewToolWithRawSchema("bounded", "a bound out of range", json.RawMessage(
		`{"type":"object","properties":{"n":{"type":"number","multipleOf":`+outOfRangeNumber+`}}}`))
	assert.PanicsWithValue(t, `tool "bounded" input schema cannot be compiled: compile tool "bounded" input schema: `+
		`/properties/n/multipleOf: number `+outOfRangeNumber+` is out of range`, func() { srv.AddTool(tool, okHandler) })
}
