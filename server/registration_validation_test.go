package server

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/mark3labs/mcp-go/mcp"
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
