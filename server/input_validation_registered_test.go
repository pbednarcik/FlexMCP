package server

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// relaxedListTool is fakeListTool without additionalProperties: false, so an
// unknown `cursor` argument passes.
func relaxedListTool() mcp.Tool {
	return mcp.NewTool("kubernetes_list",
		mcp.WithDescription("List Kubernetes resources"),
		mcp.WithString("resourceType", mcp.Required()),
		mcp.WithString("continue"),
	)
}

// callWithCursor calls kubernetes_list with the unknown `cursor` argument and
// reports whether validation refused it.
func callWithCursor(t *testing.T, srv *MCPServer) (refused bool) {
	t.Helper()
	resp := callTool(t, srv, "kubernetes_list", map[string]any{"resourceType": "pods", "cursor": "abc"})
	jr, ok := resp.(mcp.JSONRPCResponse)
	require.True(t, ok, "expected JSON-RPC response, got %T", resp)
	result, ok := jr.Result.(*mcp.CallToolResult)
	require.True(t, ok, "expected *mcp.CallToolResult, got %T", jr.Result)
	if !result.IsError {
		return false
	}
	tc, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok, "expected TextContent, got %T", result.Content[0])
	require.Contains(t, tc.Text, "cursor")
	return true
}

// The schema a call is validated against is the one the tool was last
// registered with, on every registration path, in both directions: a stricter
// schema refuses what the old one let through, and a relaxed one lets it
// through again.
func TestInputValidationReregistrationUsesTheNewSchema(t *testing.T) {
	for name, register := range map[string]func(*MCPServer, mcp.Tool){
		"AddTool": func(s *MCPServer, tool mcp.Tool) { s.AddTool(tool, okHandler) },
		"AddTools": func(s *MCPServer, tool mcp.Tool) {
			s.AddTools(ServerTool{Tool: tool, Handler: okHandler})
		},
		"SetTools": func(s *MCPServer, tool mcp.Tool) {
			s.SetTools(ServerTool{Tool: tool, Handler: okHandler})
		},
		"DeleteTools then AddTool": func(s *MCPServer, tool mcp.Tool) {
			s.DeleteTools(tool.Name)
			s.AddTool(tool, okHandler)
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation())
			register(srv, relaxedListTool())
			assert.False(t, callWithCursor(t, srv), "relaxed schema, first registration")
			register(srv, fakeListTool())
			assert.True(t, callWithCursor(t, srv), "strict schema registered over the relaxed one")
			register(srv, relaxedListTool())
			assert.False(t, callWithCursor(t, srv), "relaxed schema registered over the strict one")
		})
	}
}

// A raw schema registered again is read again too.
func TestInputValidationReregistrationUsesTheNewRawSchema(t *testing.T) {
	raw := func(additional bool) mcp.Tool {
		schema := `{"type":"object","properties":{"resourceType":{"type":"string"}},"additionalProperties":false}`
		if additional {
			schema = strings.Replace(schema, "false", "true", 1)
		}
		return mcp.NewToolWithRawSchema("kubernetes_list", "", json.RawMessage(schema))
	}
	srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation())
	srv.AddTool(raw(true), okHandler)
	assert.False(t, callWithCursor(t, srv))
	srv.AddTool(raw(false), okHandler)
	assert.True(t, callWithCursor(t, srv))
}

// Calls validate while the tool is registered again and again. Under -race
// this shows the kept schema is read under the same lock it is written
// under; every call is answered under one schema or the other.
func TestInputValidationCallsRaceReregistration(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithInputSchemaValidation())
	srv.AddTool(relaxedListTool(), okHandler)
	msg := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"kubernetes_list","arguments":{"resourceType":"pods","cursor":"abc"}}}`)

	var registrar, callers sync.WaitGroup
	stop := make(chan struct{})
	registrar.Go(func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				srv.AddTool(fakeListTool(), okHandler)
			} else {
				srv.SetTools(ServerTool{Tool: relaxedListTool(), Handler: okHandler})
			}
		}
	})
	for range 4 {
		callers.Go(func() {
			for range 200 {
				out, err := json.Marshal(srv.HandleMessage(t.Context(), msg))
				if err != nil {
					t.Error(err)
					return
				}
				if s := string(out); !strings.Contains(s, `"text":"ok"`) && !strings.Contains(s, `cursor`) {
					t.Errorf("answered under neither schema: %s", s)
					return
				}
			}
		})
	}
	callers.Wait()
	close(stop)
	registrar.Wait()
}

// structSchemaTool is a tool whose input schema is built with mcp.NewTool's
// options, so it reaches the server as a structured InputSchema.
func structSchemaTool() mcp.Tool {
	return mcp.NewTool("structured",
		mcp.WithDescription("a tool with a structured input schema"),
		mcp.WithString("region", mcp.Required(), mcp.Description("the region to query"),
			mcp.Enum("us-east-1", "us-west-2", "eu-west-1", "ap-south-1")),
		mcp.WithNumber("limit", mcp.Description("the most rows to return"), mcp.Min(1), mcp.Max(1000)),
		mcp.WithBoolean("verbose", mcp.Description("include every column")),
		mcp.WithString("cursor", mcp.Description("where the previous page ended")),
		mcp.WithArray("tags", mcp.Description("tags to match"), mcp.WithStringItems()),
		mcp.WithObject("filter", mcp.Description("a filter on the rows"), mcp.Properties(map[string]any{
			"label": map[string]any{"type": "string"},
			"since": map[string]any{"type": "string", "format": "date-time"},
		})),
		mcp.WithSchemaAdditionalProperties(false),
	)
}

// A tool built with a structured input schema is validated against the
// schema registration compiled, not one marshalled and hashed again per call:
// 96 allocations when it was, 71 with the schema kept from registration, 70
// with the arguments passed to the validator unboxed.
func TestInputValidationStructSchemaAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation())
	srv.AddTool(structSchemaTool(), okHandler)
	ctx := t.Context()
	msg := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"structured","arguments":{"region":"us-east-1","limit":5}}}`)
	out, err := json.Marshal(srv.HandleMessage(ctx, msg))
	require.NoError(t, err)
	require.Contains(t, string(out), `"text":"ok"`)
	refused, err := json.Marshal(srv.HandleMessage(ctx, json.RawMessage(strings.Replace(string(msg), `"limit":5`, `"limit":5000`, 1))))
	require.NoError(t, err)
	require.Contains(t, string(refused), `"isError":true`)

	allocs := testing.AllocsPerRun(1000, func() { srv.HandleMessage(ctx, msg) })
	assert.LessOrEqual(t, allocs, 70.0, "allocations per validated tools/call on a structured schema")
}
