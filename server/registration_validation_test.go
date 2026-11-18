package server

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// registrationPaths registers one tool through each path that accepts a
// tool definition.
var registrationPaths = []struct {
	name     string
	register func(srv *MCPServer, tool mcp.Tool)
}{
	{"AddTools", func(srv *MCPServer, tool mcp.Tool) {
		srv.AddTools(ServerTool{Tool: tool, Handler: func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil }})
	}},
	{"SetTools", func(srv *MCPServer, tool mcp.Tool) {
		srv.SetTools(ServerTool{Tool: tool, Handler: func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil }})
	}},
	{"AddTaskTools", func(srv *MCPServer, tool mcp.Tool) {
		srv.AddTaskTools(ServerTaskTool{Tool: tool, Handler: func(context.Context, mcp.CallToolRequest) (*mcp.CreateTaskResult, error) { return nil, nil }})
	}},
}

// TestToolNamesAreCheckedAtRegistration pins Tools.md row 7: a tool name is
// 1 to 128 characters of ASCII letters, digits, '_', '-' and '.', and a
// definition that breaks that is refused at registration rather than
// advertised to clients that will refuse it.
func TestToolNamesAreCheckedAtRegistration(t *testing.T) {
	bad := map[string]string{
		"empty":     "",
		"a space":   "get weather",
		"a slash":   "files/read",
		"non-ASCII": "héllo",
		"129 chars": strings.Repeat("a", 129),
		"a colon":   "ns:tool",
		"a brace":   "tool{1}",
	}
	good := []string{"a", "get_weather", "files.read", "tool-1", "CamelCase", strings.Repeat("a", 128)}

	for _, path := range registrationPaths {
		t.Run(path.name, func(t *testing.T) {
			for reason, name := range bad {
				srv := NewMCPServer("names", "1.0.0", WithToolCapabilities(true))
				want := fmt.Sprintf("tool name %q is not 1 to 128 characters of ASCII letters, digits, '_', '-' and '.'", name)
				assert.PanicsWithValue(t, want, func() { path.register(srv, mcp.NewTool(name)) }, "%s %q", reason, name)
				assert.Empty(t, srv.ListTools(), "%s %q was registered", reason, name)
			}
			for _, name := range good {
				srv := NewMCPServer("names", "1.0.0", WithToolCapabilities(true))
				assert.NotPanics(t, func() { path.register(srv, mcp.NewTool(name)) }, "%q", name)
			}
		})
	}
}

// panicMessage runs f and returns what it panicked with, or "" when it did
// not panic.
func panicMessage(f func()) (message string) {
	defer func() {
		if r := recover(); r != nil {
			message = fmt.Sprint(r)
		}
	}()
	f()
	return ""
}

// TestInputSchemaIsCompiledAtRegistration pins BaseProtocol.md rows 32 and
// 34: with input validation on, a tool whose schema cannot be compiled (an
// unsupported dialect, a $ref that would need the network) is refused at
// registration, never silently served unvalidated.
func TestInputSchemaIsCompiledAtRegistration(t *testing.T) {
	bad := map[string]string{
		"a network $ref":         `{"$ref":"https://example.com/schema.json"}`,
		"an unsupported dialect": `{"$schema":"http://json-schema.org/draft-03/schema#","type":"object"}`,
		"malformed JSON":         `{"type": "object", "properties": {`,
	}
	for _, path := range registrationPaths {
		t.Run(path.name, func(t *testing.T) {
			for reason, schema := range bad {
				srv := NewMCPServer("schemas", "1.0.0", WithToolCapabilities(true), WithInputSchemaValidation())
				tool := mcp.NewToolWithRawSchema("tool", "a tool", []byte(schema))
				assert.Contains(t, panicMessage(func() { path.register(srv, tool) }), `tool "tool" input schema cannot be compiled`, reason)
				assert.Empty(t, srv.ListTools(), "%s was registered", reason)
			}
			// An output schema is held to the same standard.
			srv := NewMCPServer("schemas", "1.0.0", WithToolCapabilities(true), WithOutputSchemaValidation())
			tool := mcp.NewTool("tool", mcp.WithRawOutputSchema([]byte(bad["malformed JSON"])))
			assert.Contains(t, panicMessage(func() { path.register(srv, tool) }), `tool "tool" output schema cannot be compiled`)

			// Without validation the schema is the client's to read, and
			// registration does not compile it.
			srv = NewMCPServer("schemas", "1.0.0", WithToolCapabilities(true))
			assert.NotPanics(t, func() { path.register(srv, mcp.NewToolWithRawSchema("tool", "a tool", []byte(bad["a network $ref"]))) })
		})
	}
}

// TestRegistrationRefusesAToolItCannotEncode pins BaseProtocol.md rows 32
// and 34 for a tool tools/list could not write: a structured schema holding
// an infinity or an unreadable json.Number, or a raw schema that is not JSON,
// is refused at registration with validation on or off, never registered to
// fail every list of the server's tools. With validation on a raw schema that
// is not JSON is refused first as a schema that does not compile. The lock is
// let go after every refusal, and nothing of a refused batch is registered.
func TestRegistrationRefusesAToolItCannotEncode(t *testing.T) {
	inf := func() mcp.Tool {
		tool := mcp.NewTool("bad")
		tool.InputSchema = mcp.ToolInputSchema{Type: "object", Properties: map[string]any{"s": map[string]any{"type": "string", "maximum": math.Inf(1)}}}
		return tool
	}
	bogus := func() mcp.Tool {
		tool := mcp.NewTool("bad")
		tool.InputSchema = mcp.ToolInputSchema{Type: "object", Properties: map[string]any{"s": map[string]any{"maximum": json.Number("bogus")}}}
		return tool
	}
	outInf := func() mcp.Tool {
		tool := mcp.NewTool("bad")
		tool.OutputSchema = mcp.ToolOutputSchema{Type: "object", Properties: map[string]any{"n": map[string]any{"minimum": math.Inf(-1)}}}
		return tool
	}
	rawIn := func() mcp.Tool { return mcp.NewToolWithRawSchema("bad", "", json.RawMessage(`{"type":`)) }
	rawOut := func() mcp.Tool {
		tool := mcp.NewTool("bad")
		tool.RawOutputSchema = json.RawMessage(`{"type":`)
		return tool
	}
	for _, tc := range []struct {
		name string
		tool func() mcp.Tool
		raw  string // the kind of a raw schema, which validation compiles first
	}{
		{"typed input +Inf", inf, ""},
		{"typed input bogus number", bogus, ""},
		{"typed output -Inf", outInf, ""},
		{"raw input not JSON", rawIn, "input"},
		{"raw output not JSON", rawOut, "output"},
	} {
		for _, validate := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s validate=%v", tc.name, validate), func(t *testing.T) {
				opts := []ServerOption{WithToolCapabilities(false)}
				if validate {
					opts = append(opts, WithInputSchemaValidation(), WithOutputSchemaValidation())
				}
				srv := NewMCPServer("t", "1.0.0", opts...)
				msg := panicMessage(func() { srv.AddTool(tc.tool(), okHandler) })
				want := `tool "bad" cannot be encoded: `
				if validate && tc.raw != "" {
					want = `tool "bad" ` + tc.raw + ` schema cannot be compiled: decode tool "bad" ` + tc.raw + ` schema: `
				}
				assert.True(t, strings.HasPrefix(msg, want), "refusal %q, want the prefix %q", msg, want)
				require.True(t, srv.toolsMu.TryLock(), "toolsMu is held after the refusal")
				srv.toolsMu.Unlock()
				assert.Empty(t, srv.ListTools(), "a refused tool was registered")
			})
		}
	}
	t.Run("every registration path refuses it", func(t *testing.T) {
		for _, path := range registrationPaths {
			srv := NewMCPServer("t", "1.0.0", WithToolCapabilities(false))
			msg := panicMessage(func() { path.register(srv, inf()) })
			assert.True(t, strings.HasPrefix(msg, `tool "bad" cannot be encoded: `), "%s: refusal %q", path.name, msg)
			require.True(t, srv.toolsMu.TryLock(), "%s: toolsMu is held after the refusal", path.name)
			srv.toolsMu.Unlock()
		}
	})
	t.Run("a batch with one tool it cannot encode registers none", func(t *testing.T) {
		srv := NewMCPServer("t", "1.0.0", WithToolCapabilities(false))
		msg := panicMessage(func() {
			srv.AddTools(ServerTool{Tool: mcp.NewTool("good"), Handler: okHandler}, ServerTool{Tool: inf(), Handler: okHandler})
		})
		assert.Contains(t, msg, "cannot be encoded")
		assert.Empty(t, srv.ListTools())
	})
	t.Run("an encodable typed schema still validates the call", func(t *testing.T) {
		srv := NewMCPServer("t", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation())
		tool := mcp.NewTool("good")
		tool.InputSchema = mcp.ToolInputSchema{Type: "object", Properties: map[string]any{"s": map[string]any{"type": "string"}}}
		srv.AddTool(tool, okHandler)
		call := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"good","arguments":{"s":5}}}`
		assert.Contains(t, handled(t, srv, call), "got number, want string")
	})
}
