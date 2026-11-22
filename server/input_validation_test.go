package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// fakeListTool returns a tool that mirrors the kubernetes_list shape used in
// the bug report: a `continue` pagination token (the standard k8s name) plus
// strict additionalProperties: false so unknown args like `cursor` are
// rejected.
func fakeListTool() mcp.Tool {
	return mcp.NewTool("kubernetes_list",
		mcp.WithDescription("List Kubernetes resources"),
		mcp.WithString("resourceType", mcp.Required(),
			mcp.Description("Type of resource to list")),
		mcp.WithString("continue",
			mcp.Description("Continue token from previous paginated request")),
		mcp.WithSchemaAdditionalProperties(false),
	)
}

func okHandler(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultText("ok"), nil
}

func callTool(t *testing.T, srv *MCPServer, toolName string, args any) mcp.JSONRPCMessage {
	t.Helper()
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return srv.HandleMessage(t.Context(), raw)
}

// requireToolErrorContaining asserts that the response is a successful
// JSON-RPC response carrying a tool execution error (CallToolResult.IsError =
// true) whose text contains the supplied substring. This is the SEP-1303
// shape that lets the model receive the validation message in its context.
func requireToolErrorContaining(t *testing.T, resp mcp.JSONRPCMessage, want string) {
	t.Helper()
	jr, ok := resp.(mcp.JSONRPCResponse)
	require.True(t, ok, "expected JSON-RPC response, got %T", resp)
	result, ok := jr.Result.(*mcp.CallToolResult)
	require.True(t, ok, "expected *mcp.CallToolResult, got %T", jr.Result)
	require.True(t, result.IsError, "expected IsError=true, got %#v", result)
	require.NotEmpty(t, result.Content, "expected at least one content entry")
	tc, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected TextContent, got %T", result.Content[0])
	require.Contains(t, tc.Text, want)
}

func requireToolSuccess(t *testing.T, resp mcp.JSONRPCMessage) *mcp.CallToolResult {
	t.Helper()
	jr, ok := resp.(mcp.JSONRPCResponse)
	require.True(t, ok, "expected JSON-RPC response, got %T", resp)
	result, ok := jr.Result.(*mcp.CallToolResult)
	require.True(t, ok, "expected *mcp.CallToolResult, got %T", jr.Result)
	require.False(t, result.IsError, "expected IsError=false, got %#v", result)
	return result
}

// TestInputSchemaValidation covers the matrix of validation outcomes for a
// range of tools: strict, permissive, no schema, raw schema, broken schema.
// Each row exercises exactly one server lifecycle, registers the listed
// tools, makes a single tool call, and asserts on either success or an error
// containing a specific substring.
//
// Scenarios that need multi-step setup (cache invalidation,
// recompilation-on-schema-change) live in their own dedicated tests.
func TestInputSchemaValidation(t *testing.T) {
	rawStrictTool := mcp.Tool{
		Name:        "raw",
		Description: "Tool with raw JSON Schema",
		RawInputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"name": {"type": "string"}},
			"required": ["name"],
			"additionalProperties": false
		}`),
	}
	noSchemaTool := mcp.Tool{Name: "noschema", Description: "no schema"}
	permissiveTool := mcp.NewTool("permissive",
		mcp.WithString("name", mcp.Required()),
	)
	nestedStrictTool := mcp.Tool{
		Name: "nested",
		RawInputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"filter": {
					"type": "object",
					"properties": {"label": {"type": "string"}},
					"required": ["label"],
					"additionalProperties": false
				}
			},
			"required": ["filter"]
		}`),
	}

	tests := []struct {
		name              string
		options           []ServerOption
		tool              mcp.Tool
		args              map[string]any
		wantSuccess       bool
		wantErrContains   string
		wantHandlerCalled bool
	}{
		{
			// Regression guard: with validation off, the kubernetes_list
			// scenario from the bug report (sending `cursor` instead of
			// `continue`) silently succeeds. Pinned so we never flip the
			// default to strict and break existing servers.
			name:              "disabled by default accepts unknown property",
			options:           nil,
			tool:              fakeListTool(),
			args:              map[string]any{"resourceType": "pods", "cursor": "abc"},
			wantSuccess:       true,
			wantHandlerCalled: true,
		},
		{
			// The motivating fix: with validation on and additionalProperties:
			// false, an unknown `cursor` parameter must surface as a tool
			// execution error so the model can retry with `continue`.
			name:            "strict schema rejects unknown property",
			options:         []ServerOption{WithInputSchemaValidation()},
			tool:            fakeListTool(),
			args:            map[string]any{"resourceType": "pods", "cursor": "abc"},
			wantErrContains: "cursor",
		},
		{
			name:            "strict schema rejects missing required",
			options:         []ServerOption{WithInputSchemaValidation()},
			tool:            fakeListTool(),
			args:            map[string]any{"continue": "abc"},
			wantErrContains: "resourceType",
		},
		{
			name:            "strict schema rejects wrong type",
			options:         []ServerOption{WithInputSchemaValidation()},
			tool:            fakeListTool(),
			args:            map[string]any{"resourceType": 123},
			wantErrContains: "resourceType",
		},
		{
			name:              "valid call passes through to handler",
			options:           []ServerOption{WithInputSchemaValidation()},
			tool:              fakeListTool(),
			args:              map[string]any{"resourceType": "pods", "continue": "abc"},
			wantSuccess:       true,
			wantHandlerCalled: true,
		},
		{
			// Tools that omit additionalProperties: false continue to accept
			// extras even with validation enabled. This preserves back-compat
			// with permissive schemas the author chose deliberately.
			name:              "permissive schema accepts extras",
			options:           []ServerOption{WithInputSchemaValidation()},
			tool:              permissiveTool,
			args:              map[string]any{"name": "alice", "extra": "field"},
			wantSuccess:       true,
			wantHandlerCalled: true,
		},
		{
			name:            "raw input schema participates in validation",
			options:         []ServerOption{WithInputSchemaValidation()},
			tool:            rawStrictTool,
			args:            map[string]any{"name": "alice", "typo": 1},
			wantErrContains: "typo",
		},
		{
			// Tools with no input schema at all (effectively zero-arg tools)
			// are unaffected: there is nothing to validate against.
			name:              "tool without schema is unaffected",
			options:           []ServerOption{WithInputSchemaValidation()},
			tool:              noSchemaTool,
			args:              map[string]any{"anything": "goes"},
			wantSuccess:       true,
			wantHandlerCalled: true,
		},
		{
			name:            "nested error message mentions field path",
			options:         []ServerOption{WithInputSchemaValidation()},
			tool:            nestedStrictTool,
			args:            map[string]any{"filter": map[string]any{"label": "x", "extra": true}},
			wantErrContains: "filter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewMCPServer("test", "1.0.0", tt.options...)
			handlerCalled := false
			srv.AddTool(tt.tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				handlerCalled = true
				return mcp.NewToolResultText("ok"), nil
			})

			resp := callTool(t, srv, tt.tool.Name, tt.args)

			if tt.wantSuccess {
				requireToolSuccess(t, resp)
			} else {
				requireToolErrorContaining(t, resp, tt.wantErrContains)
			}
			assert.Equal(t, tt.wantHandlerCalled, handlerCalled, "handler invocation expectation")
		})
	}
}

// TestInputSchemaValidation_RecompilesOnSchemaChange documents that
// re-registering a tool with a different schema honours the new schema on
// the next call. A fresh schema has a fresh digest, so it replaces the cached
// entry rather than reusing the previous compilation.
func TestInputSchemaValidation_RecompilesOnSchemaChange(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation())
	srv.AddTool(fakeListTool(), okHandler)

	resp := callTool(t, srv, "kubernetes_list", map[string]any{
		"resourceType": "pods",
		"cursor":       "abc",
	})
	requireToolErrorContaining(t, resp, "cursor")

	relaxed := mcp.NewTool("kubernetes_list",
		mcp.WithDescription("List Kubernetes resources"),
		mcp.WithString("resourceType", mcp.Required()),
		mcp.WithString("continue"),
	)
	srv.AddTool(relaxed, okHandler)

	resp = callTool(t, srv, "kubernetes_list", map[string]any{
		"resourceType": "pods",
		"cursor":       "abc",
	})
	requireToolSuccess(t, resp)
}

// TestInputSchemaValidation_DeleteToolInvalidatesCache makes a validated
// call first to populate the cache, then deletes the tool and asserts the
// cache entry is gone. Without the warmup, the cache would be empty
// regardless of whether DeleteTools invalidated it (compilation is lazy).
func TestInputSchemaValidation_DeleteToolInvalidatesCache(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation())
	srv.AddTool(fakeListTool(), okHandler)

	resp := callTool(t, srv, "kubernetes_list", map[string]any{
		"resourceType": "pods",
		"continue":     "abc",
	})
	requireToolSuccess(t, resp)
	require.Len(t, srv.inputValidator.cached, 1, "cache should be populated after a validated call")

	srv.DeleteTools("kubernetes_list")
	require.Len(t, srv.inputValidator.cached, 0, "cache should be empty after DeleteTools")
}

// compiledEntries counts the compiled schemas c holds for the tool called
// name.
func compiledEntries(c *compiledSchemaCache, name string) int {
	if _, ok := c.cached[name]; ok {
		return 1
	}
	return 0
}

// holdsSchema reports whether c holds the compilation of schemaJSON for the
// tool called name.
func holdsSchema(c *compiledSchemaCache, name string, schemaJSON []byte) bool {
	entry, ok := c.cached[name]
	return ok && entry.digest == schemaDigest(schemaJSON)
}

// The schema caches keep only a tool's current schemas: a tool registered
// again and again with a new schema each time leaves one compiled input
// schema and one output schema, the last registered, and none once it is
// deleted.
func TestSchemaCachesKeepOnlyTheCurrentSchema(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation(), WithOutputSchemaValidation())
	var schema json.RawMessage
	for i := range 500 {
		schema = json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"n%d":{"type":"number"}}}`, i))
		srv.AddTool(mcp.Tool{Name: "churn", RawInputSchema: schema, RawOutputSchema: schema}, okHandler)
	}
	caches := map[string]*compiledSchemaCache{
		"input":  srv.inputValidator.compiledSchemaCache,
		"output": srv.outputValidator.compiledSchemaCache,
	}
	for kind, c := range caches {
		assert.Equal(t, 1, compiledEntries(c, "churn"), "%s schemas cached for one tool", kind)
		assert.True(t, holdsSchema(c, "churn", schema), "the %s schema cached is not the last registered", kind)
	}

	srv.DeleteTools("churn")
	for kind, c := range caches {
		assert.Zero(t, compiledEntries(c, "churn"), "%s schemas cached for a deleted tool", kind)
	}
}

// cachedDigests returns the digest of every schema c holds, by tool name.
func cachedDigests(c *compiledSchemaCache) map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	digests := make(map[string]string, len(c.cached))
	for name, entry := range c.cached {
		digests[name] = entry.digest
	}
	return digests
}

// numberSchema is an object schema with one number property called prop.
func numberSchema(prop string) json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"` + prop + `":{"type":"number"}}}`)
}

// A refused registration leaves the schema caches as they were, on every
// batch path: a batch refused for its last entry caches no schema of its
// earlier ones, neither a new tool's nor a registered tool's replacement.
func TestRejectedRegistrationLeavesTheSchemaCaches(t *testing.T) {
	tool := func(name, prop string) mcp.Tool {
		return mcp.Tool{
			Name: name, RawInputSchema: numberSchema(prop), RawOutputSchema: numberSchema(prop),
			Execution: &mcp.ToolExecution{TaskSupport: mcp.TaskSupportOptional},
		}
	}
	bad := mcp.Tool{Name: "bad", RawInputSchema: json.RawMessage(`{"$ref":"missing-schema.json"}`)}
	for name, register := range map[string]func(*MCPServer, ...mcp.Tool){
		"AddTools": func(s *MCPServer, tools ...mcp.Tool) {
			batch := make([]ServerTool, 0, len(tools))
			for _, tool := range tools {
				batch = append(batch, ServerTool{Tool: tool, Handler: okHandler})
			}
			s.AddTools(batch...)
		},
		"AddTaskTools": func(s *MCPServer, tools ...mcp.Tool) {
			batch := make([]ServerTaskTool, 0, len(tools))
			for _, tool := range tools {
				batch = append(batch, ServerTaskTool{Tool: tool, Handler: okTaskHandler})
			}
			s.AddTaskTools(batch...)
		},
		"SetTools": func(s *MCPServer, tools ...mcp.Tool) {
			batch := make([]ServerTool, 0, len(tools))
			for _, tool := range tools {
				batch = append(batch, ServerTool{Tool: tool, Handler: okHandler})
			}
			s.SetTools(batch...)
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation(), WithOutputSchemaValidation())
			register(srv, tool("registered", "a"))
			input, output := cachedDigests(srv.inputValidator.compiledSchemaCache), cachedDigests(srv.outputValidator.compiledSchemaCache)
			require.Contains(t, input, "registered")
			require.Contains(t, output, "registered")

			assert.Panics(t, func() { register(srv, tool("registered", "b"), tool("new", "c"), bad) })
			assert.Equal(t, input, cachedDigests(srv.inputValidator.compiledSchemaCache), "input schemas cached")
			assert.Equal(t, output, cachedDigests(srv.outputValidator.compiledSchemaCache), "output schemas cached")
		})
	}
}

// A tool registered again without schemas leaves none of its old ones cached.
func TestSchemaFreeReregistrationDropsTheCachedSchemas(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation(), WithOutputSchemaValidation())
	srv.AddTool(mcp.Tool{Name: "t", RawInputSchema: numberSchema("a"), RawOutputSchema: numberSchema("a")}, okHandler)
	caches := map[string]*compiledSchemaCache{
		"input":  srv.inputValidator.compiledSchemaCache,
		"output": srv.outputValidator.compiledSchemaCache,
	}
	for kind, c := range caches {
		require.Equal(t, 1, compiledEntries(c, "t"), "%s schemas cached for the tool", kind)
	}

	srv.AddTool(mcp.Tool{Name: "t"}, okHandler)
	for kind, c := range caches {
		assert.Zero(t, compiledEntries(c, "t"), "%s schemas cached for a tool that has none", kind)
	}
}

// TestInputSchemaValidation_NestedPathInError keeps a focused assertion that
// the validator's error message reaches into nested objects. It deliberately
// duplicates one of the table cases above with extra path-substring checks
// that would clutter the table assertions.
func TestInputSchemaValidation_NestedPathInError(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation())
	tool := mcp.Tool{
		Name: "nested",
		RawInputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"filter": {
					"type": "object",
					"properties": {"label": {"type": "string"}},
					"required": ["label"],
					"additionalProperties": false
				}
			},
			"required": ["filter"]
		}`),
	}
	srv.AddTool(tool, okHandler)

	resp := callTool(t, srv, "nested", map[string]any{
		"filter": map[string]any{"label": "x", "extra": true},
	})
	jr, ok := resp.(mcp.JSONRPCResponse)
	require.True(t, ok)
	result, ok := jr.Result.(*mcp.CallToolResult)
	require.True(t, ok)
	require.True(t, result.IsError)
	tc := result.Content[0].(mcp.TextContent)
	require.True(t, strings.Contains(tc.Text, "/filter") || strings.Contains(tc.Text, "filter"),
		"error message should reference the nested path: %s", tc.Text)
}

// An element checked by items after prefixItems, or by additionalItems after
// an items array, is named by its own index: the second element here.
func TestInputSchemaValidation_ItemsAfterAPrefixNameTheirElement(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation())
	srv.AddTool(mcp.NewToolWithRawSchema("prefixed", "", json.RawMessage(
		`{"type":"object","properties":{"a":{"prefixItems":[{}],"items":{"type":"integer"}}}}`)), okHandler)
	srv.AddTool(mcp.NewToolWithRawSchema("draft7", "", json.RawMessage(
		`{"$schema":"http://json-schema.org/draft-07/schema#","type":"object","properties":{"a":{"items":[{}],"additionalItems":{"type":"integer"}}}}`)), okHandler)
	for _, tool := range []string{"prefixed", "draft7"} {
		assert.Equal(t, "input schema validation failed: /a/1: got string, want integer", resultText(t, srv, tool, `{"a":["x","y"]}`), tool)
	}
}

// TestInputSchemaValidation_MessagesAreSentences pins the message the model
// receives: the instance location, then the validator's own English
// sentence for the violation, never a Go struct dump.
func TestInputSchemaValidation_MessagesAreSentences(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation())
	tool := mcp.Tool{
		Name: "validated",
		RawInputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"id": {"type": "number"},
				"fields": {"type": "object", "minProperties": 1}
			},
			"required": ["id", "fields"]
		}`),
	}
	srv.AddTool(tool, okHandler)

	tests := []struct {
		name            string
		args            map[string]any
		wantContains    []string
		wantNotContains []string
	}{
		{
			name:            "minProperties says what was got and wanted",
			args:            map[string]any{"id": 23582, "fields": map[string]any{}},
			wantContains:    []string{"/fields: minProperties: got 0, want 1"},
			wantNotContains: []string{"%!s", "&{"},
		},
		{
			name:            "required names the missing property",
			args:            map[string]any{"id": 23582},
			wantContains:    []string{"<root>: missing property 'fields'"},
			wantNotContains: []string{"%!s", "&{"},
		},
		{
			name:            "type says what was got and wanted",
			args:            map[string]any{"id": "abc"},
			wantContains:    []string{"/id: got string, want number"},
			wantNotContains: []string{"%!s", "&{"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := callTool(t, srv, tool.Name, tt.args)
			jr, ok := resp.(mcp.JSONRPCResponse)
			require.True(t, ok)
			result, ok := jr.Result.(*mcp.CallToolResult)
			require.True(t, ok)
			require.True(t, result.IsError)
			tc := result.Content[0].(mcp.TextContent)
			for _, want := range tt.wantContains {
				require.Contains(t, tc.Text, want, "full message: %s", tc.Text)
			}
			for _, omit := range tt.wantNotContains {
				require.NotContains(t, tc.Text, omit, "full message: %s", tc.Text)
			}
		})
	}
}

// TestInputSchemaValidation_Edges pins the edges the matrix above leaves
// open: a schema that is valid JSON but does not compile, arguments that
// are not an object, arguments that are absent, and several violations in
// one call.
func TestInputSchemaValidation_Edges(t *testing.T) {
	twoFieldTool := mcp.NewTool("two",
		mcp.WithString("a", mcp.Required()),
		mcp.WithString("b"),
	)

	tests := []struct {
		name              string
		tool              mcp.Tool
		args              any
		wantErrContains   []string
		wantHandlerCalled bool
	}{
		{
			name:            "arguments that are an array are rejected at the root",
			tool:            twoFieldTool,
			args:            []any{"a", "b"},
			wantErrContains: []string{"<root>: got array, want object"},
		},
		{
			name:            "arguments that are a string are rejected at the root",
			tool:            twoFieldTool,
			args:            "a=1",
			wantErrContains: []string{"<root>: got string, want object"},
		},
		{
			name:            "absent arguments are an empty object, so required fails",
			tool:            twoFieldTool,
			args:            nil,
			wantErrContains: []string{"<root>: missing property 'a'"},
		},
		{
			name:            "every violation is reported, separated by semicolons",
			tool:            twoFieldTool,
			args:            map[string]any{"b": 1},
			wantErrContains: []string{"<root>: missing property 'a'; /b: got number, want string"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation())
			handlerCalled := false
			srv.AddTool(tt.tool, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				handlerCalled = true
				return mcp.NewToolResultText("ok"), nil
			})

			resp := callTool(t, srv, tt.tool.Name, tt.args)

			if tt.wantHandlerCalled {
				requireToolSuccess(t, resp)
			}
			for _, want := range tt.wantErrContains {
				requireToolErrorContaining(t, resp, want)
			}
			assert.Equal(t, tt.wantHandlerCalled, handlerCalled, "handler invocation expectation")
		})
	}
}

// A top-level keyword the structured schema does not model is validated
// too: an argument set that breaks a top-level oneOf is refused.
func TestInputSchemaValidation_AdditionalSchemaKeywords(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation())
	tool := mcp.NewTool("order",
		mcp.WithString("sku"),
		mcp.WithNumber("quantity"),
	)
	tool.InputSchema.AdditionalFields = map[string]any{
		"oneOf": []any{
			map[string]any{"required": []any{"sku"}},
			map[string]any{"required": []any{"quantity"}},
		},
	}
	srv.AddTool(tool, okHandler)

	requireToolErrorContaining(t, callTool(t, srv, "order", map[string]any{"sku": "a", "quantity": 1}), "oneOf")
	resp := callTool(t, srv, "order", map[string]any{"sku": "a"})
	jr, ok := resp.(mcp.JSONRPCResponse)
	require.True(t, ok, "expected JSON-RPC response, got %T", resp)
	result, ok := jr.Result.(*mcp.CallToolResult)
	require.True(t, ok, "expected *mcp.CallToolResult, got %T", jr.Result)
	require.False(t, result.IsError, "%#v", result)
}

// TestSchemaValidation_FallbackFieldSelection validates the arguments
// encoding/json binds against a tag-fallback schema: a field promoted from an
// unexported embedding is accepted, and a depth-0 field's type wins over the
// embedded field it shadows.
func TestSchemaValidation_FallbackFieldSelection(t *testing.T) {
	type private struct {
		Count int `json:"count"`
	}
	type promoted struct {
		private
		Mode string `json:"mode" jsonschema:"enum=fast,enum=safe"`
	}
	type Inner struct {
		Count int `json:"count"`
	}
	type shadowed struct {
		Count string `json:"count"`
		Inner
		Mode string `json:"mode" jsonschema:"enum=fast,enum=safe"`
	}
	for _, test := range []struct {
		name    string
		tool    mcp.Tool
		target  any
		count   any
		success bool
	}{
		{name: "promoted field accepted", tool: mcp.NewTool("fallback", mcp.WithInputSchema[promoted](), mcp.WithOutputSchema[promoted]()), target: &promoted{}, count: 7, success: true},
		{name: "promoted field typed", tool: mcp.NewTool("fallback", mcp.WithInputSchema[promoted](), mcp.WithOutputSchema[promoted]()), target: &promoted{}, count: "7"},
		{name: "shallow field accepted", tool: mcp.NewTool("fallback", mcp.WithInputSchema[shadowed](), mcp.WithOutputSchema[shadowed]()), target: &shadowed{}, count: "x", success: true},
		{name: "shadowed type rejected", tool: mcp.NewTool("fallback", mcp.WithInputSchema[shadowed](), mcp.WithOutputSchema[shadowed]()), target: &shadowed{}, count: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.NotEmpty(t, test.tool.RawInputSchema)
			called := false
			srv := NewMCPServer("fallback", "1.0", WithInputSchemaValidation(), WithOutputSchemaValidation())
			srv.AddTool(test.tool, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				called = true
				if err := req.BindArguments(test.target); err != nil {
					return nil, err
				}
				return mcp.NewToolResultStructuredOnly(test.target), nil
			})
			args := map[string]any{"count": test.count, "mode": "fast"}
			response := callTool(t, srv, "fallback", args)
			if test.success {
				requireToolSuccess(t, response)
				got, err := json.Marshal(test.target)
				require.NoError(t, err)
				want, err := json.Marshal(args)
				require.NoError(t, err)
				assert.JSONEq(t, string(want), string(got))
			} else {
				requireToolErrorContaining(t, response, "count")
			}
			assert.Equal(t, test.success, called)
		})
	}
}

// TestSchemaValidation_NativeJSONNameShadowing validates against the depth-0
// field encoding/json binds, not the embedded field of the same JSON name
// that jsonschema-go lets win.
func TestSchemaValidation_NativeJSONNameShadowing(t *testing.T) {
	type Node struct {
		Count int `json:"count"`
	}
	type Plain struct {
		Other int `json:"other"`
	}
	type ShadowNode struct {
		Hidden Node `json:"node,omitempty"` //nolint:modernize // Preserve omitempty to exercise native optional-field metadata before shadow selection.
	}
	type request struct {
		Node Plain `json:"node"`
		ShadowNode
	}
	tool := mcp.NewTool("shadow", mcp.WithInputSchema[request](), mcp.WithOutputSchema[request]())
	for _, test := range []struct {
		name    string
		field   string
		value   any
		success bool
	}{
		{name: "selected field accepted", field: "other", value: 5, success: true},
		{name: "selected field typed", field: "other", value: "5"},
		{name: "shadowed field rejected", field: "count", value: 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.NotEmpty(t, tool.RawInputSchema)
			called := false
			srv := NewMCPServer("shadow", "1.0", WithInputSchemaValidation(), WithOutputSchemaValidation())
			srv.AddTool(tool, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				called = true
				var target request
				if err := req.BindArguments(&target); err != nil {
					return nil, err
				}
				return mcp.NewToolResultStructuredOnly(target), nil
			})
			args := map[string]any{"node": map[string]any{test.field: test.value}}
			response := callTool(t, srv, "shadow", args)
			if test.success {
				requireToolSuccess(t, response)
			} else {
				requireToolErrorContaining(t, response, test.field)
			}
			assert.Equal(t, test.success, called)
		})
	}
}

func TestSchemaValidation_QuotedIgnoredShadow(t *testing.T) {
	type Embedded struct {
		Other int `json:"value,string"`
	}
	type request struct {
		Embedded
		Selected []func() `json:"value,omitempty"`
	}
	for _, value := range []request{{Other: 7}, {Other: 7, Selected: []func(){}}} {
		encoded, err := json.Marshal(value) //nolint:staticcheck // Both nil and empty unsupported slices are omitted before encoding their elements.
		require.NoError(t, err)
		assert.JSONEq(t, `{}`, string(encoded))
	}
	raw, err := mcp.SchemaForRaw[request]()
	require.NoError(t, err)
	var schema map[string]any
	require.NoError(t, json.Unmarshal(raw, &schema))
	properties, _ := schema["properties"].(map[string]any)
	assert.NotContains(t, properties, "value", "the selected unsupported field must be ignored")
	tool := mcp.NewTool("quoted", mcp.WithInputSchema[request](), mcp.WithOutputSchema[request]())
	for _, test := range []struct {
		name    string
		args    map[string]any
		success bool
	}{
		{name: "omitted unsupported field", args: map[string]any{}, success: true},
		{name: "shadowed integer rejected", args: map[string]any{"value": 5}},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			srv := NewMCPServer("quoted", "1.0", WithInputSchemaValidation(), WithOutputSchemaValidation())
			srv.AddTool(tool, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				called = true
				var target request
				if err := req.BindArguments(&target); err != nil {
					return nil, err
				}
				return mcp.NewToolResultStructuredOnly(target), nil
			})
			response := callTool(t, srv, "quoted", test.args)
			if test.success {
				requireToolSuccess(t, response)
			} else {
				requireToolErrorContaining(t, response, "value")
			}
			assert.Equal(t, test.success, called)
		})
	}
}

func TestSchemaValidation_QuotedPromotedNestedFields(t *testing.T) {
	type quoted struct {
		Count int `json:"count,string"`
	}
	type plain struct {
		Count int `json:"count"`
	}
	type EmbeddedQuoted struct {
		Nested quoted `json:"nested"`
	}
	type EmbeddedPlain struct {
		Nested plain `json:"nested"`
	}
	type outerPlain struct {
		EmbeddedQuoted
		Nested plain `json:"nested"`
	}
	type outerQuoted struct {
		Nested quoted `json:"nested"`
		EmbeddedPlain
	}
	type private struct {
		Nested quoted `json:"nested"`
	}
	type privateRequest struct {
		private
	}
	plainTool := mcp.NewTool("quoted", mcp.WithInputSchema[outerPlain](), mcp.WithOutputSchema[outerPlain]())
	quotedTool := mcp.NewTool("quoted", mcp.WithInputSchema[outerQuoted](), mcp.WithOutputSchema[outerQuoted]())
	privateTool := mcp.NewTool("quoted", mcp.WithInputSchema[privateRequest](), mcp.WithOutputSchema[privateRequest]())
	tests := []struct {
		name    string
		tool    mcp.Tool
		target  any
		count   any
		success bool
	}{
		{name: "shadowed quoted field accepts number", tool: plainTool, target: &outerPlain{}, count: 42, success: true},
		{name: "shadowed quoted field rejects string", tool: plainTool, target: &outerPlain{}, count: "42"},
		{name: "shadowed plain field accepts string", tool: quotedTool, target: &outerQuoted{}, count: "42", success: true},
		{name: "shadowed plain field rejects number", tool: quotedTool, target: &outerQuoted{}, count: 42},
		{name: "private embedding accepts string", tool: privateTool, target: &privateRequest{}, count: "42", success: true},
		{name: "private embedding rejects number", tool: privateTool, target: &privateRequest{}, count: 42},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			srv := NewMCPServer("quoted", "1.0", WithInputSchemaValidation(), WithOutputSchemaValidation())
			srv.AddTool(test.tool, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				called = true
				if err := req.BindArguments(test.target); err != nil {
					return nil, err
				}
				return mcp.NewToolResultStructuredOnly(test.target), nil
			})
			args := map[string]any{"nested": map[string]any{"count": test.count}}
			response := callTool(t, srv, "quoted", args)
			if test.success {
				requireToolSuccess(t, response)
				got, err := json.Marshal(test.target)
				require.NoError(t, err)
				want, err := json.Marshal(args)
				require.NoError(t, err)
				assert.JSONEq(t, string(want), string(got))
			} else {
				requireToolErrorContaining(t, response, "count")
			}
			assert.Equal(t, test.success, called)
		})
	}
}

func TestSchemaValidation_QuotedPrivateEmbeddingFallback(t *testing.T) {
	type private struct {
		Count int `json:"count,string"`
	}
	type request struct {
		private
		Mode string `json:"mode" jsonschema:"enum=fast,enum=safe"`
	}
	raw, err := mcp.SchemaForRaw[request]()
	require.NoError(t, err)
	require.Contains(t, string(raw), `"count"`)
	tool := mcp.NewTool("quoted", mcp.WithInputSchema[request](), mcp.WithOutputSchema[request]())
	for _, test := range []struct {
		name    string
		count   any
		success bool
	}{
		{name: "quoted number", count: "7", success: true},
		{name: "unquoted number", count: 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			srv := NewMCPServer("quoted", "1.0", WithInputSchemaValidation(), WithOutputSchemaValidation())
			srv.AddTool(tool, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				called = true
				var args request
				if err := req.BindArguments(&args); err != nil {
					return nil, err
				}
				require.Equal(t, 7, args.Count)
				return mcp.NewToolResultStructuredOnly(args), nil
			})
			response := callTool(t, srv, "quoted", map[string]any{"count": test.count, "mode": "fast"})
			if test.success {
				requireToolSuccess(t, response)
			} else {
				requireToolErrorContaining(t, response, "count")
			}
			assert.Equal(t, test.success, called)
		})
	}
}

func TestSchemaValidation_QuotedShadowingFallback(t *testing.T) {
	type Node struct {
		Count int `json:"count,string"`
	}
	type ShadowNode struct {
		Other int `json:"other"`
	}
	type Shadow struct {
		Node ShadowNode `json:"node"`
	}
	type request struct {
		Node Node   `json:"node"`
		Mode string `json:"mode" jsonschema:"enum=fast,enum=safe"`
		Shadow
	}
	raw, err := mcp.SchemaForRaw[request]()
	require.NoError(t, err)
	require.NotContains(t, string(raw), `"other"`)
	tool := mcp.NewTool("quoted", mcp.WithInputSchema[request](), mcp.WithOutputSchema[request]())
	for _, test := range []struct {
		name    string
		count   any
		success bool
	}{
		{name: "quoted number", count: "5", success: true},
		{name: "unquoted number", count: 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			srv := NewMCPServer("quoted", "1.0", WithInputSchemaValidation(), WithOutputSchemaValidation())
			srv.AddTool(tool, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				called = true
				var args request
				if err := req.BindArguments(&args); err != nil {
					return nil, err
				}
				require.Equal(t, 5, args.Node.Count)
				return mcp.NewToolResultStructuredOnly(args), nil
			})
			args := map[string]any{"node": map[string]any{"count": test.count}, "mode": "fast"}
			response := callTool(t, srv, "quoted", args)
			if test.success {
				requireToolSuccess(t, response)
			} else {
				requireToolErrorContaining(t, response, "count")
			}
			assert.Equal(t, test.success, called)
		})
	}
}

func TestSchemaValidation_QuotedStringEnums(t *testing.T) {
	type normal struct {
		Mode string `json:"mode,string" jsonschema:"Run mode,enum=fast,enum=safe"`
	}
	type fallback struct {
		Mode string `json:"mode,string" jsonschema:"enum=fast,enum=safe"`
	}
	for _, schema := range []struct {
		name   string
		tool   mcp.Tool
		target any
	}{
		{name: "normal", tool: mcp.NewTool("quoted", mcp.WithInputSchema[normal](), mcp.WithOutputSchema[normal]()), target: &normal{}},
		{name: "fallback", tool: mcp.NewTool("quoted", mcp.WithInputSchema[fallback](), mcp.WithOutputSchema[fallback]()), target: &fallback{}},
	} {
		for _, test := range []struct {
			name    string
			mode    string
			success bool
		}{
			{name: "encoded enum", mode: `"fast"`, success: true},
			{name: "outside enum", mode: `"slow"`},
			{name: "missing encoding level", mode: "fast"},
		} {
			t.Run(schema.name+"/"+test.name, func(t *testing.T) {
				called := false
				srv := NewMCPServer("quoted", "1.0", WithInputSchemaValidation(), WithOutputSchemaValidation())
				srv.AddTool(schema.tool, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					called = true
					if err := req.BindArguments(schema.target); err != nil {
						return nil, err
					}
					return mcp.NewToolResultStructuredOnly(schema.target), nil
				})
				args := map[string]any{"mode": test.mode}
				response := callTool(t, srv, "quoted", args)
				if test.success {
					requireToolSuccess(t, response)
					encoded, err := json.Marshal(schema.target)
					require.NoError(t, err)
					want, err := json.Marshal(args)
					require.NoError(t, err)
					assert.JSONEq(t, string(want), string(encoded))
				} else {
					requireToolErrorContaining(t, response, "mode")
				}
				assert.Equal(t, test.success, called)
			})
		}
	}
}

func TestSchemaValidation_QuotedField(t *testing.T) {
	type request struct {
		Count int    `json:"count,string"`
		Mode  string `json:"mode" jsonschema:"enum=fast,enum=safe"`
	}
	tests := []struct {
		name    string
		count   any
		success bool
	}{
		{name: "quoted number", count: "42", success: true},
		{name: "unquoted number", count: 42, success: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			called := false
			srv := NewMCPServer("quoted", "1.0", WithInputSchemaValidation(), WithOutputSchemaValidation())
			srv.AddTool(mcp.NewTool("quoted", mcp.WithInputSchema[request](), mcp.WithOutputSchema[request]()),
				func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					called = true
					var args request
					if err := req.BindArguments(&args); err != nil {
						return nil, err
					}
					assert.Equal(t, 42, args.Count)
					return mcp.NewToolResultStructuredOnly(args), nil
				})
			response := callTool(t, srv, "quoted", map[string]any{"count": test.count, "mode": "fast"})
			if test.success {
				requireToolSuccess(t, response)
			} else {
				requireToolErrorContaining(t, response, "count")
			}
			assert.Equal(t, test.success, called)
		})
	}
}

func TestSchemaValidation_QuotedJSONNameShadowing(t *testing.T) {
	type Node struct {
		Count int `json:"count,string"`
	}
	type Plain struct {
		Other int `json:"other"`
	}
	type ShadowPlain struct {
		Hidden Plain `json:"node,omitempty"` //nolint:modernize // Preserve omitempty to exercise native optional-field metadata before shadow selection.
	}
	type ShadowQuoted struct {
		Hidden Node `json:"node,omitempty"` //nolint:modernize // Preserve omitempty to exercise native optional-field metadata before shadow selection.
	}
	type quotedRequest struct {
		Node Node `json:"node"`
		ShadowPlain
	}
	type plainRequest struct {
		Node Plain `json:"node"`
		ShadowQuoted
	}
	for _, test := range []struct {
		name    string
		tool    mcp.Tool
		target  any
		field   string
		value   any
		success bool
	}{
		{name: "quoted accepts string", tool: mcp.NewTool("quoted", mcp.WithInputSchema[quotedRequest](), mcp.WithOutputSchema[quotedRequest]()), target: &quotedRequest{}, field: "count", value: "5", success: true},
		{name: "quoted rejects number", tool: mcp.NewTool("quoted", mcp.WithInputSchema[quotedRequest](), mcp.WithOutputSchema[quotedRequest]()), target: &quotedRequest{}, field: "count", value: 5},
		{name: "plain accepts number", tool: mcp.NewTool("quoted", mcp.WithInputSchema[plainRequest](), mcp.WithOutputSchema[plainRequest]()), target: &plainRequest{}, field: "other", value: 5, success: true},
		{name: "plain rejects string", tool: mcp.NewTool("quoted", mcp.WithInputSchema[plainRequest](), mcp.WithOutputSchema[plainRequest]()), target: &plainRequest{}, field: "other", value: "5"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.NotEmpty(t, test.tool.RawInputSchema)
			called := false
			srv := NewMCPServer("quoted", "1.0", WithInputSchemaValidation(), WithOutputSchemaValidation())
			srv.AddTool(test.tool, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				called = true
				if err := req.BindArguments(test.target); err != nil {
					return nil, err
				}
				return mcp.NewToolResultStructuredOnly(test.target), nil
			})
			args := map[string]any{"node": map[string]any{test.field: test.value}}
			response := callTool(t, srv, "quoted", args)
			if test.success {
				requireToolSuccess(t, response)
				got, err := json.Marshal(test.target)
				require.NoError(t, err)
				want, err := json.Marshal(args)
				require.NoError(t, err)
				assert.JSONEq(t, string(want), string(got))
			} else {
				requireToolErrorContaining(t, response, test.field)
			}
			assert.Equal(t, test.success, called)
		})
	}
}

func TestSchemaValidation_QuotedNullableEnums(t *testing.T) {
	type normal struct {
		Mode *string `json:"mode,string" jsonschema:"Run mode,enum=fast,enum=safe"`
	}
	type fallback struct {
		Mode *string `json:"mode,string" jsonschema:"enum=fast,enum=safe"`
	}
	for _, schema := range []struct {
		name   string
		tool   mcp.Tool
		target any
	}{
		{name: "normal", tool: mcp.NewTool("quoted", mcp.WithInputSchema[normal](), mcp.WithOutputSchema[normal]()), target: &normal{}},
		{name: "fallback", tool: mcp.NewTool("quoted", mcp.WithInputSchema[fallback](), mcp.WithOutputSchema[fallback]()), target: &fallback{}},
	} {
		for _, test := range []struct {
			name    string
			mode    any
			success bool
		}{
			{name: "nil enum", mode: nil, success: true},
			{name: "encoded enum", mode: `"fast"`, success: true},
			{name: "outside enum", mode: `"slow"`},
			{name: "missing encoding level", mode: "fast"},
		} {
			t.Run(schema.name+"/"+test.name, func(t *testing.T) {
				called := false
				srv := NewMCPServer("quoted", "1.0", WithInputSchemaValidation(), WithOutputSchemaValidation())
				srv.AddTool(schema.tool, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					called = true
					if err := req.BindArguments(schema.target); err != nil {
						return nil, err
					}
					return mcp.NewToolResultStructuredOnly(schema.target), nil
				})
				args := map[string]any{"mode": test.mode}
				response := callTool(t, srv, "quoted", args)
				if test.success {
					requireToolSuccess(t, response)
					encoded, err := json.Marshal(schema.target)
					require.NoError(t, err)
					want, err := json.Marshal(args)
					require.NoError(t, err)
					assert.JSONEq(t, string(want), string(encoded))
				} else {
					requireToolErrorContaining(t, response, "mode")
				}
				assert.Equal(t, test.success, called)
			})
		}
	}
}

type quotedCustomJSONStruct struct {
	Count int `json:"count,string"`
}

func (value quotedCustomJSONStruct) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Count int `json:"count"`
	}{Count: value.Count})
}

func TestSchemaValidation_QuotedCustomStructEncoding(t *testing.T) {
	type input struct {
		Count int `json:"count"`
	}
	type nested struct {
		Value quotedCustomJSONStruct `json:"value"`
	}
	type sliced struct {
		Values []quotedCustomJSONStruct `json:"values"`
	}
	for _, test := range []struct {
		name   string
		tool   mcp.Tool
		output func(int) any
	}{
		{name: "root", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[quotedCustomJSONStruct]()), output: func(count int) any { return quotedCustomJSONStruct{Count: count} }},
		{name: "nested", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[nested]()), output: func(count int) any { return nested{Value: quotedCustomJSONStruct{Count: count}} }},
		{name: "slice", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[sliced]()), output: func(count int) any { return sliced{Values: []quotedCustomJSONStruct{{Count: count}}} }},
		{name: "map", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[map[string]quotedCustomJSONStruct]()), output: func(count int) any { return map[string]quotedCustomJSONStruct{"entry": {Count: count}} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			srv := NewMCPServer("quoted", "1.0", WithInputSchemaValidation(), WithOutputSchemaValidation())
			srv.AddTool(test.tool, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				called = true
				var target input
				if err := req.BindArguments(&target); err != nil {
					return nil, err
				}
				assert.Equal(t, 17, target.Count)
				return mcp.NewToolResultStructuredOnly(test.output(target.Count)), nil
			})
			result := callTool(t, srv, "quoted", map[string]any{"count": 17})
			requireToolSuccess(t, result)
			assert.True(t, called)
		})
	}
}

func TestSchemaValidation_QuotedSelectedEnums(t *testing.T) {
	type private struct {
		Mode string `json:"mode,string" jsonschema:"Allowed modes,enum=fast,enum=safe"`
	}
	type privateNormal struct{ private }
	type privateFallback struct {
		private
		Trigger string `json:"trigger,omitempty" jsonschema:"enum=run"`
	}
	type Old struct {
		Other string `json:"mode,string" jsonschema:"Old modes,enum=old"`
	}
	type shadowNormal struct {
		Mode string `json:"mode,string" jsonschema:"Allowed modes,enum=fast,enum=safe"`
		Old
	}
	type shadowFallback struct {
		Mode string `json:"mode,string" jsonschema:"Allowed modes,enum=fast,enum=safe"`
		Old
		Trigger string `json:"trigger,omitempty" jsonschema:"enum=run"`
	}
	for _, schema := range []struct {
		name   string
		tool   mcp.Tool
		target any
		raw    func() (json.RawMessage, error)
	}{
		{name: "private normal", tool: mcp.NewTool("quoted", mcp.WithInputSchema[privateNormal](), mcp.WithOutputSchema[privateNormal]()), target: &privateNormal{}, raw: func() (json.RawMessage, error) { return mcp.SchemaForRaw[privateNormal]() }},
		{name: "private fallback", tool: mcp.NewTool("quoted", mcp.WithInputSchema[privateFallback](), mcp.WithOutputSchema[privateFallback]()), target: &privateFallback{}, raw: func() (json.RawMessage, error) { return mcp.SchemaForRaw[privateFallback]() }},
		{name: "shadow normal", tool: mcp.NewTool("quoted", mcp.WithInputSchema[shadowNormal](), mcp.WithOutputSchema[shadowNormal]()), target: &shadowNormal{}, raw: func() (json.RawMessage, error) { return mcp.SchemaForRaw[shadowNormal]() }},
		{name: "shadow fallback", tool: mcp.NewTool("quoted", mcp.WithInputSchema[shadowFallback](), mcp.WithOutputSchema[shadowFallback]()), target: &shadowFallback{}, raw: func() (json.RawMessage, error) { return mcp.SchemaForRaw[shadowFallback]() }},
	} {
		raw, err := schema.raw()
		require.NoError(t, err)
		var parsed map[string]any
		require.NoError(t, json.Unmarshal(raw, &parsed))
		mode := parsed["properties"].(map[string]any)["mode"].(map[string]any)
		assert.Equal(t, "Allowed modes", mode["description"], schema.name)
		assert.Equal(t, []any{`"fast"`, `"safe"`}, mode["enum"], schema.name)
		for _, test := range []struct {
			name, mode string
			success    bool
		}{
			{name: "selected enum", mode: `"fast"`, success: true},
			{name: "shadowed enum", mode: `"old"`},
			{name: "outside enum", mode: `"slow"`},
		} {
			t.Run(schema.name+"/"+test.name, func(t *testing.T) {
				called := false
				srv := NewMCPServer("quoted", "1.0", WithInputSchemaValidation(), WithOutputSchemaValidation())
				srv.AddTool(schema.tool, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					called = true
					if err := req.BindArguments(schema.target); err != nil {
						return nil, err
					}
					return mcp.NewToolResultStructuredOnly(schema.target), nil
				})
				args := map[string]any{"mode": test.mode}
				result := callTool(t, srv, "quoted", args)
				if test.success {
					requireToolSuccess(t, result)
					encoded, err := json.Marshal(schema.target)
					require.NoError(t, err)
					want, err := json.Marshal(args)
					require.NoError(t, err)
					assert.JSONEq(t, string(want), string(encoded))
				} else {
					requireToolErrorContaining(t, result, "mode")
				}
				assert.Equal(t, test.success, called)
			})
		}
	}
}

type quotedAddressableJSONStruct struct {
	Count int `json:"count,string"`
}

func (value *quotedAddressableJSONStruct) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Count int `json:"count"`
	}{Count: value.Count})
}

func TestSchemaValidation_QuotedCustomStructAddressability(t *testing.T) {
	type input struct {
		Count int `json:"count"`
	}
	type scalarFields struct {
		Count quotedAddressableJSONScalar `json:"count,string"`
	}
	type scalarSlice struct {
		Items []scalarFields `json:"items"`
	}
	type customSliceOutput struct {
		Items quotedCustomJSONSlice `json:"items"`
	}
	type mixed struct {
		Map     map[string]quotedAddressableJSONStruct `json:"map"`
		Slice   []quotedAddressableJSONStruct          `json:"slice"`
		Pointer *quotedAddressableJSONStruct           `json:"pointer"`
	}
	type nested struct {
		Value quotedAddressableJSONStruct `json:"value"`
	}
	type sliced struct {
		Items []quotedAddressableJSONStruct `json:"items"`
	}
	type arrayed struct {
		Items [1]quotedAddressableJSONStruct `json:"items"`
	}
	type embedded struct {
		Value quotedAddressableJSONStruct `json:"value"`
	}
	type promoted struct{ *embedded }
	type promotedFallback struct {
		*embedded
		Trigger string `json:"trigger,omitempty" jsonschema:"enum=run"`
	}
	for _, test := range []struct {
		name   string
		tool   mcp.Tool
		output any
		want   string
	}{
		{name: "value root", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[quotedAddressableJSONStruct]()), output: quotedAddressableJSONStruct{Count: 17}, want: `{"count":"17"}`},
		{name: "pointer root", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[*quotedAddressableJSONStruct]()), output: &quotedAddressableJSONStruct{Count: 17}, want: `{"count":17}`},
		{name: "map value", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[map[string]quotedAddressableJSONStruct]()), output: map[string]quotedAddressableJSONStruct{"entry": {Count: 17}}, want: `{"entry":{"count":"17"}}`},
		{name: "slice element", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[sliced]()), output: sliced{Items: []quotedAddressableJSONStruct{{Count: 17}}}, want: `{"items":[{"count":17}]}`},
		{name: "nested value", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[nested]()), output: nested{Value: quotedAddressableJSONStruct{Count: 17}}, want: `{"value":{"count":"17"}}`},
		{name: "nested pointer", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[*nested]()), output: &nested{Value: quotedAddressableJSONStruct{Count: 17}}, want: `{"value":{"count":17}}`},
		{name: "array value", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[arrayed]()), output: arrayed{Items: [1]quotedAddressableJSONStruct{{Count: 17}}}, want: `{"items":[{"count":"17"}]}`},
		{name: "array pointer", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[*arrayed]()), output: &arrayed{Items: [1]quotedAddressableJSONStruct{{Count: 17}}}, want: `{"items":[{"count":17}]}`},
		{name: "promoted pointer", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[promoted]()), output: promoted{embedded: &embedded{Value: quotedAddressableJSONStruct{Count: 17}}}, want: `{"value":{"count":17}}`},
		{name: "promoted pointer fallback", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[promotedFallback]()), output: promotedFallback{embedded: &embedded{Value: quotedAddressableJSONStruct{Count: 17}}}, want: `{"value":{"count":17}}`},
		{name: "scalar value", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[scalarFields]()), output: scalarFields{Count: 17}, want: `{"count":"17"}`},
		{name: "scalar pointer", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[*scalarFields]()), output: &scalarFields{Count: 17}, want: `{"count":17}`},
		{name: "scalar map", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[map[string]scalarFields]()), output: map[string]scalarFields{"entry": {Count: 17}}, want: `{"entry":{"count":"17"}}`},
		{name: "scalar slice", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[scalarSlice]()), output: scalarSlice{Items: []scalarFields{{Count: 17}}}, want: `{"items":[{"count":17}]}`},
		{name: "mixed contexts", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[mixed]()), output: mixed{Map: map[string]quotedAddressableJSONStruct{"entry": {Count: 17}}, Slice: []quotedAddressableJSONStruct{{Count: 17}}, Pointer: &quotedAddressableJSONStruct{Count: 17}}, want: `{"map":{"entry":{"count":"17"}},"slice":[{"count":17}],"pointer":{"count":17}}`},
		{name: "custom map encoding", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[quotedCustomJSONMap]()), output: quotedCustomJSONMap{"entry": {Count: 17}}, want: `{"entry":{"count":17}}`},
		{name: "custom slice encoding", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[customSliceOutput]()), output: customSliceOutput{Items: quotedCustomJSONSlice{{Count: 17}}}, want: `{"items":[{"count":17}]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.output)
			require.NoError(t, err)
			assert.JSONEq(t, test.want, string(encoded))
			srv := NewMCPServer("quoted", "1.0", WithInputSchemaValidation(), WithOutputSchemaValidation())
			srv.AddTool(test.tool, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var target input
				if err := req.BindArguments(&target); err != nil {
					return nil, err
				}
				assert.Equal(t, 17, target.Count)
				return mcp.NewToolResultStructuredOnly(test.output), nil
			})
			requireToolSuccess(t, callTool(t, srv, "quoted", map[string]any{"count": 17}))
		})
	}
}

type quotedAddressableNode struct {
	Count int `json:"count,string"`
}

type quotedAddressableOther struct {
	Other int `json:"other"`
}

type quotedAddressableShadow struct {
	Hidden quotedAddressableOther `json:"node,omitempty"` //nolint:modernize // Preserve native optional-field metadata in the shadow fixture.
}

type quotedAddressableSelection struct {
	Node quotedAddressableNode `json:"node"`
	quotedAddressableShadow
}

func (value *quotedAddressableSelection) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Node quotedAddressableOther `json:"node"`
	}{Node: value.Hidden})
}

func TestSchemaValidation_QuotedCustomNativeSelection(t *testing.T) {
	type input struct {
		Count int `json:"count"`
	}
	value := quotedAddressableSelection{Node: quotedAddressableNode{Count: 17}, Hidden: quotedAddressableOther{Other: 17}}
	for _, test := range []struct {
		name   string
		tool   mcp.Tool
		output any
		want   string
	}{
		{name: "value uses selected fields", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[quotedAddressableSelection]()), output: value, want: `{"node":{"count":"17"}}`},
		{name: "pointer uses custom encoding", tool: mcp.NewTool("quoted", mcp.WithInputSchema[input](), mcp.WithOutputSchema[*quotedAddressableSelection]()), output: &value, want: `{"node":{"other":17}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.output)
			require.NoError(t, err)
			assert.JSONEq(t, test.want, string(encoded))
			srv := NewMCPServer("quoted", "1.0", WithInputSchemaValidation(), WithOutputSchemaValidation())
			srv.AddTool(test.tool, func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var target input
				if err := req.BindArguments(&target); err != nil {
					return nil, err
				}
				assert.Equal(t, 17, target.Count)
				return mcp.NewToolResultStructuredOnly(test.output), nil
			})
			requireToolSuccess(t, callTool(t, srv, "quoted", map[string]any{"count": 17}))
		})
	}
}

type quotedAddressableJSONScalar int

func (value *quotedAddressableJSONScalar) MarshalJSON() ([]byte, error) {
	return json.Marshal(int(*value))
}

type quotedCustomPlainNode struct {
	Count int `json:"count"`
}

type quotedCustomJSONMap map[string]quotedAddressableNode

func (value quotedCustomJSONMap) MarshalJSON() ([]byte, error) {
	output := make(map[string]quotedCustomPlainNode, len(value))
	for key, item := range value {
		output[key] = quotedCustomPlainNode(item)
	}
	return json.Marshal(output)
}

type quotedCustomJSONSlice []quotedAddressableNode

func (value quotedCustomJSONSlice) MarshalJSON() ([]byte, error) {
	output := make([]quotedCustomPlainNode, len(value))
	for i, item := range value {
		output[i] = quotedCustomPlainNode(item)
	}
	return json.Marshal(output)
}
