package server

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// skipServer serves hookTool as "t", registered with SkipInputValidation,
// and the same tool as "u", registered without it, under input validation
// with an input validation handler that refuses; hooks counts its calls.
func skipServer(t *testing.T, opts ...ServerOption) (srv *MCPServer, hooks *atomic.Int32) {
	t.Helper()
	hooks = &atomic.Int32{}
	srv = NewMCPServer("test", "1.0.0", append([]ServerOption{
		WithToolCapabilities(false), WithTaskCapabilities(true, true, true),
		WithInputValidationHandler(func(context.Context, mcp.Tool, CallArguments, InputValidationFailure) (*mcp.CallToolResult, bool) {
			hooks.Add(1)
			return nil, true
		}),
	}, opts...)...)
	ran := func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ran"), nil
	}
	other := hookTool()
	other.Name = "u"
	srv.AddTools(
		ServerTool{Tool: other, Handler: ran},
		ServerTool{Tool: hookTool(), Handler: ran, SkipInputValidation: true},
	)
	return srv, hooks
}

// skipCall calls the tool name with arguments, as a legacy task when task
// is set, and returns the tool result's text and isError.
func skipCall(t *testing.T, srv *MCPServer, name, arguments string, task bool) (string, bool) {
	t.Helper()
	params := `{"name":"` + name + `","arguments":` + arguments
	if task {
		params += `,"task":{}`
	}
	msg := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":` + params + `}}`)
	if !task {
		out, err := json.Marshal(srv.HandleMessage(t.Context(), msg))
		if err != nil {
			t.Fatal(err)
		}
		return hookAnswer(t, string(out))
	}
	// callForAnswer reads the task's result; it calls "t" only.
	if name != "t" {
		t.Fatalf("a legacy task of %s", name)
	}
	return hookAnswer(t, hookCall(t, srv, arguments, true))
}

// A tool registered with SkipInputValidation gets its calls unvalidated, on
// the synchronous site and on the legacy task site, with validation before
// the middleware or inside it: arguments its schema refuses reach the
// handler, and the input validation handler is not called.
func TestSkipInputValidationReachesHandler(t *testing.T) {
	for name, opts := range map[string][]ServerOption{
		"before the middleware": nil,
		"inside the middleware": {WithInputValidationInsideMiddleware()},
	} {
		t.Run(name, func(t *testing.T) {
			bothSites(t, func(t *testing.T, task bool) {
				srv, hooks := skipServer(t, opts...)
				text, isError := skipCall(t, srv, "t", `{"n":11}`, task)
				assert.Equal(t, "ran", text, "the skipped tool's call was refused")
				assert.False(t, isError)
				assert.Zero(t, hooks.Load(), "the input validation handler was called")
			})
		})
	}
}

// SkipInputValidation is the tool's own: another tool of the same server,
// registered without it, still has its calls refused.
func TestSkipInputValidationOtherToolsStillRefused(t *testing.T) {
	for name, opts := range map[string][]ServerOption{
		"before the middleware": nil,
		"inside the middleware": {WithInputValidationInsideMiddleware()},
	} {
		t.Run(name, func(t *testing.T) {
			srv, hooks := skipServer(t, opts...)
			text, isError := skipCall(t, srv, "u", `{"n":11}`, false)
			assert.True(t, isError, "the other tool's call was not refused: %s", text)
			assert.True(t, strings.HasPrefix(text, "input schema validation failed: "), text)
			assert.Equal(t, int32(1), hooks.Load())

			text, isError = skipCall(t, srv, "t", `{"n":11}`, false)
			assert.Equal(t, "ran", text)
			assert.False(t, isError)
			assert.Equal(t, int32(1), hooks.Load())
		})
	}
}

// ListTools hands back each registration whole, SkipInputValidation and
// every other field of ServerTool as GetTool has them, so a server set again
// from it, SetTools(ListTools()...), leaves the skipped tool unvalidated.
func TestSkipInputValidationSurvivesListTools(t *testing.T) {
	srv, hooks := skipServer(t)
	listed := srv.ListTools()
	require.Contains(t, listed, "t")
	assert.True(t, listed["t"].SkipInputValidation, "ListTools dropped SkipInputValidation")
	for name, tool := range listed {
		assertSameServerTool(t, srv.GetTool(name), tool)
	}

	var tools []ServerTool
	for _, tool := range listed {
		tools = append(tools, *tool)
	}
	srv.SetTools(tools...)
	text, isError := skipCall(t, srv, "t", `{"n":11}`, false)
	assert.Equal(t, "ran", text, "the skipped tool's call was refused after SetTools(ListTools())")
	assert.False(t, isError)
	assert.Zero(t, hooks.Load())
}

// assertSameServerTool asserts that got holds every field of want, a func
// field as the same function.
func assertSameServerTool(t *testing.T, want, got *ServerTool) {
	t.Helper()
	require.NotNil(t, want)
	require.NotNil(t, got)
	w, g := reflect.ValueOf(want).Elem(), reflect.ValueOf(got).Elem()
	for i := range w.NumField() {
		name := w.Type().Field(i).Name
		if w.Field(i).Kind() == reflect.Func {
			assert.Equal(t, w.Field(i).Pointer(), g.Field(i).Pointer(), "field %s", name)
			continue
		}
		assert.Equal(t, w.Field(i).Interface(), g.Field(i).Interface(), "field %s", name)
	}
}

// SkipInputValidation holds however the tool is registered: by SetTools, and
// by registering it again with the flag turned on or off, which the next
// call follows.
func TestSkipInputValidationFollowsRegistration(t *testing.T) {
	ran := func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ran"), nil
	}
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation())
	srv.SetTools(ServerTool{Tool: hookTool(), Handler: ran, SkipInputValidation: true})
	text, isError := skipCall(t, srv, "t", `{"n":11}`, false)
	assert.Equal(t, "ran", text, "set by SetTools")
	assert.False(t, isError)

	srv.AddTools(ServerTool{Tool: hookTool(), Handler: ran})
	text, isError = skipCall(t, srv, "t", `{"n":11}`, false)
	assert.True(t, isError, "registered again without the flag: %s", text)

	srv.AddTools(ServerTool{Tool: hookTool(), Handler: ran, SkipInputValidation: true})
	text, isError = skipCall(t, srv, "t", `{"n":11}`, false)
	assert.Equal(t, "ran", text, "registered again with the flag")
	assert.False(t, isError)
}

// A tool registered with SkipInputValidation still has its input schema
// compiled at registration: one that does not compile is refused there.
func TestSkipInputValidationStillCompilesTheSchema(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation())
	bad := mcp.NewToolWithRawSchema("bad", "", json.RawMessage(`{"type":"object","properties":{"n":{"type":"no such type"}}}`))
	assert.Panics(t, func() {
		srv.AddTools(ServerTool{Tool: bad, Handler: okHandler, SkipInputValidation: true})
	})
	assert.Nil(t, srv.GetTool("bad"), "the tool was registered")
}
