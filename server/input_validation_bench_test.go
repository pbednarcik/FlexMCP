package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
)

// BenchmarkStreamableHTTPToolCallValidated drives ServeHTTP with one modern
// tools/call per iteration to a tool whose input is validated: bench/compare's
// small and complex calls, the complex one with an item's id written with an
// exponent, with a before-call hook, after which validation reads the
// arguments the hook left, and with an input validation handler, which a
// passing call never reaches.
func BenchmarkStreamableHTTPToolCallValidated(b *testing.B) {
	for _, leg := range []struct {
		name     string
		complex  bool
		exponent bool
		hook     bool
		handler  bool
	}{
		{name: "small"},
		{name: "complex", complex: true},
		{name: "complex-exponent", complex: true, exponent: true},
		{name: "complex-hook", complex: true, hook: true},
		{name: "complex-handler", complex: true, handler: true},
	} {
		b.Run(leg.name, func(b *testing.B) {
			options := []ServerOption{WithToolCapabilities(false), WithInputSchemaValidation()}
			if leg.hook {
				hooks := &Hooks{}
				hooks.AddBeforeCallTool(func(context.Context, any, *mcp.CallToolRequest) {})
				options = append(options, WithHooks(hooks))
			}
			if leg.handler {
				options = append(options, refusingInputValidation())
			}
			schema, arguments, filler := json.RawMessage(smallSchema), json.RawMessage(smallArguments), ""
			if leg.complex {
				schema, arguments, filler = complexRawSchema(), complexArguments(), complexResultFiller
			}
			srv := NewMCPServer("bench", "1.0.0", options...)
			srv.AddTool(mcp.NewToolWithRawSchema("echo", "echoes region", schema),
				func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return mcp.NewToolResultText(request.GetString("region", "") + filler), nil
				})
			httpSrv := NewStreamableHTTPServer(srv)
			body, header := modernToolCall(b, arguments)
			if leg.exponent {
				body = bytes.Replace(body, []byte(`"id":0,`), []byte(`"id":0e0,`), 1)
				if !bytes.Contains(body, []byte(`"id":0e0,`)) {
					b.Fatal("no item id to write with an exponent")
				}
			}

			// One call outside the loop proves the leg answers the region, and
			// the same call with the region a number is refused.
			if w := serveOne(httpSrv, body, header); w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"text":"us-east-1`)) {
				b.Fatalf("leg %s: status %d body %.300s", leg.name, w.Code, w.Body.String())
			}
			bad := bytes.Replace(body, []byte(`"region":"us-east-1"`), []byte(`"region":5`), 1)
			if w := serveOne(httpSrv, bad, header); !bytes.Contains(w.Body.Bytes(), []byte(`"isError":true`)) {
				b.Fatalf("leg %s: invalid region not refused: %.300s", leg.name, w.Body.String())
			} else if leg.handler && !bytes.Contains(w.Body.Bytes(), []byte(`refused 1+0`)) {
				b.Fatalf("leg %s: invalid region not refused by the handler: %.300s", leg.name, w.Body.String())
			}

			b.ReportAllocs()
			for b.Loop() {
				serveOne(httpSrv, body, header)
			}
		})
	}
}

// BenchmarkInputValidationArrayHook measures a validated tools/call whose
// arguments are an array, with a before-call hook, to a tool that reads its
// arguments at once and to one that opted in to ArgumentsOnDemand, for which
// validation decodes them for itself alone.
func BenchmarkInputValidationArrayHook(b *testing.B) {
	for _, onDemand := range []bool{false, true} {
		name := "eager"
		if onDemand {
			name = "ondemand"
		}
		b.Run(name, func(b *testing.B) {
			hooks := &Hooks{}
			hooks.AddBeforeCallTool(func(context.Context, any, *mcp.CallToolRequest) {})
			srv := NewMCPServer("bench", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation(), WithHooks(hooks))
			// argumentsNil is what the handler last saw of Params.Arguments.
			var argumentsNil bool
			srv.AddTools(ServerTool{
				Tool: mcp.NewToolWithRawSchema("echo", "", json.RawMessage(`{"type":["array","object"],"items":{"type":"integer"}}`)),
				Handler: func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					argumentsNil = request.Params.Arguments == nil
					return mcp.NewToolResultText("ok"), nil
				},
				ArgumentsOnDemand: onDemand,
			})
			// One call outside the loop proves the leg is validated, and its
			// handler is given Params.Arguments nil under the opt-in alone.
			const arguments = `[1,2,3,4,5,6,7,8]`
			if out := onDemandCall(b, srv, arguments); !strings.Contains(out, `"text":"ok"`) || strings.Contains(out, `"isError":true`) || argumentsNil != onDemand {
				b.Fatalf("%s: Params.Arguments nil %v, %s", name, argumentsNil, out)
			}
			if out := onDemandCall(b, srv, `[1,"x"]`); !strings.Contains(out, `"isError":true`) {
				b.Fatalf("%s: invalid arguments not refused: %s", name, out)
			}
			ctx := b.Context()
			message := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":` + arguments + `}}`)
			b.ReportAllocs()
			for b.Loop() {
				srv.HandleMessage(ctx, message)
			}
		})
	}
}

// BenchmarkInputValidationRegister measures registering a tool on a server
// that validates input, for bench/compare's complex schema and a flat 8 KB
// one. compile registers, each iteration, the other of two tools whose
// schemas differ only in a description, so every registration compiles its
// schema; unchanged registers the same tool again, which finds its compiled
// schema cached and only hashes it.
func BenchmarkInputValidationRegister(b *testing.B) {
	for _, leg := range []struct {
		name      string
		schema    json.RawMessage
		arguments string
	}{
		{"complex", complexRawSchema(), string(complexArguments())},
		{"flat-8k", bigRawSchema(8192, false), smallArguments},
	} {
		// described is the leg's tool with a description in its schema, which
		// changes the schema's digest and leaves what it accepts as it was.
		described := func(description string) ServerTool {
			schema := json.RawMessage(`{"description":"` + description + `",` + string(leg.schema[1:]))
			return ServerTool{Tool: mcp.NewToolWithRawSchema("echo", "echoes region", schema), Handler: okHandler}
		}

		// register registers tool on srv and proves the schema it compiled
		// accepts the leg's arguments and refuses a region that is a number.
		register := func(b *testing.B, srv *MCPServer, tool ServerTool) {
			srv.SetTools(tool)
			if out := onDemandCall(b, srv, leg.arguments); !strings.Contains(out, `"text":"ok"`) || strings.Contains(out, `"isError":true`) {
				b.Fatalf("leg %s: %s", leg.name, out)
			}
			if out := onDemandCall(b, srv, `{"region":5}`); !strings.Contains(out, `"isError":true`) {
				b.Fatalf("leg %s: invalid region not refused: %s", leg.name, out)
			}
		}

		b.Run(leg.name, func(b *testing.B) {
			b.Run("compile", func(b *testing.B) {
				srv := NewMCPServer("bench", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation())
				tools := [2]ServerTool{described("a"), described("b")}
				for _, tool := range tools {
					register(b, srv, tool)
				}

				// Each iteration registers the tool the one before replaced, whose
				// digest is not the cached one's, so every registration compiles.
				b.ReportAllocs()
				next := 0
				for b.Loop() {
					srv.SetTools(tools[next])
					next ^= 1
				}
			})
			b.Run("unchanged", func(b *testing.B) {
				srv := NewMCPServer("bench", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation())
				tool := ServerTool{Tool: mcp.NewToolWithRawSchema("echo", "echoes region", leg.schema), Handler: okHandler}
				register(b, srv, tool)

				// This measures registering an unchanged tool again: the schema's
				// digest matches the cached one's, so nothing is compiled.
				b.ReportAllocs()
				for b.Loop() {
					srv.SetTools(tool)
				}
			})
		})
	}
}

// BenchmarkInputValidationFailing drives ServeHTTP with one modern tools/call
// per iteration whose arguments fail input validation: bench/compare's complex
// call with 15 of its item ids written as strings, where the schema wants
// integers. hook=none is the default refusal; hook=refuse has an input
// validation handler, which reads the failure it is given and refuses with a
// result of its own. The other legs are shapes that reach further:
//   - exclusive: an exclusive bound fails, which asks whether the 8 KB schema
//     holds a draft-4 schema;
//   - longkey: 10 failures under a 1 MiB key of characters the pointer escapes,
//     their lines cut as they are written;
//   - propertyNames: a name fails under a $ref and a properties keyword, whose
//     place in the arguments is derived.
func BenchmarkInputValidationFailing(b *testing.B) {
	for _, leg := range []struct {
		name   string
		option ServerOption
		answer string
	}{
		{"hook=none", WithInputSchemaValidation(), "input schema validation failed: "},
		{"hook=refuse", refusingInputValidation(), "refused 10+5"},
	} {
		b.Run(leg.name, func(b *testing.B) {
			srv := NewMCPServer("bench", "1.0.0", WithToolCapabilities(false), leg.option)
			srv.AddTool(mcp.NewToolWithRawSchema("echo", "echoes region", complexRawSchema()),
				func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return mcp.NewToolResultText(request.GetString("region", "") + complexResultFiller), nil
				})
			httpSrv := NewStreamableHTTPServer(srv)
			body, header := modernToolCall(b, failingComplexArguments(b))

			// One call outside the loop proves the leg refuses with its own
			// answer, and that the arguments it fails are the complex call's, which
			// the same server answers when they are valid.
			if w := serveOne(httpSrv, body, header); !bytes.Contains(w.Body.Bytes(), []byte(`"isError":true`)) || !bytes.Contains(w.Body.Bytes(), []byte(leg.answer)) {
				b.Fatalf("leg %s: not refused as %q: %.300s", leg.name, leg.answer, w.Body.String())
			}
			goodBody, goodHeader := modernToolCall(b, complexArguments())
			if w := serveOne(httpSrv, goodBody, goodHeader); w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"text":"us-east-1`)) {
				b.Fatalf("leg %s: valid call: status %d body %.300s", leg.name, w.Code, w.Body.String())
			}

			b.ReportAllocs()
			for b.Loop() {
				serveOne(httpSrv, body, header)
			}
		})
	}

	// A hook that names what it was given proves which path a leg reached.
	naming := WithInputValidationHandler(func(_ context.Context, _ mcp.Tool, _ CallArguments, failure InputValidationFailure) (*mcp.CallToolResult, bool) {
		first := failure.Violations[0]
		return mcp.NewToolResultError(fmt.Sprintf("refused %d+%d %s at %.8s of %.8s: %.32s", len(failure.Violations), failure.Omitted, first.Keyword, first.Pointer, first.Property, first.SchemaLocation)), true
	})
	key := strings.Repeat("~", 1<<20)
	failures := make([]any, 10)
	for i := range failures {
		failures[i] = "x"
	}
	for _, leg := range []struct {
		name, schema string
		arguments    map[string]any
		option       ServerOption
		answer       string
		proof        func(*MCPServer) bool
	}{
		{
			name: "exclusive/hook=refuse", option: naming,
			schema:    strings.Replace(string(complexRawSchema()), `"properties":{"region":{"type":"string"}`, `"properties":{"n":{"exclusiveMaximum":5},"region":{"type":"string"}`, 1),
			arguments: map[string]any{"region": "us-east-1", "items": []any{}, "n": 9},
			answer:    "refused 1+0 exclusiveMaximum at /n of : /properties/n/exclusiveMaximum",
			proof:     func(srv *MCPServer) bool { return srv.toolInputSchemas["echo"].draft4.Load() == draft4Absent },
		},
		{
			name: "longkey/hook=none", option: WithInputSchemaValidation(),
			schema:    `{"type":"object","additionalProperties":{"type":"array","items":{"type":"integer"}}}`,
			arguments: map[string]any{key: failures},
			answer:    `/~0~0~0~0`,
		},
		{
			name: "longkey/hook=refuse", option: naming,
			schema:    `{"type":"object","additionalProperties":{"type":"array","items":{"type":"integer"}}}`,
			arguments: map[string]any{key: failures},
			answer:    "refused 10+0 type at /~0~0~0~ of : /additionalProperties/items/type",
		},
		{
			name: "propertyNames/hook=refuse", option: naming,
			schema: `{"$ref":"#/$defs/box","$defs":{"box":{"type":"object","properties":{"inner":{"type":"object","propertyNames":{"maxLength":2}}}}}}`,
			arguments: map[string]any{"inner": map[string]any{
				"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10, "eleven": 11,
			}},
			answer: "refused 10+2 maxLength at /inner of eight: /$defs/box/properties/inner/prop",
		},
	} {
		b.Run(leg.name, func(b *testing.B) {
			srv := NewMCPServer("bench", "1.0.0", WithToolCapabilities(false), leg.option)
			srv.AddTool(mcp.NewToolWithRawSchema("echo", "", json.RawMessage(leg.schema)), okHandler)
			httpSrv := NewStreamableHTTPServer(srv)
			body, header := modernToolCall(b, leg.arguments)

			// One call outside the loop proves the leg reaches its path: its
			// answer names what the hook was given, or the cut pointer, and the
			// default text stays within its lines.
			w := serveOne(httpSrv, body, header)
			if !bytes.Contains(w.Body.Bytes(), []byte(`"isError":true`)) || !bytes.Contains(w.Body.Bytes(), []byte(leg.answer)) {
				b.Fatalf("leg %s: not refused as %q: %.300s", leg.name, leg.answer, w.Body.String())
			}
			if w.Body.Len() > 16<<10 {
				b.Fatalf("leg %s: %d bytes refused", leg.name, w.Body.Len())
			}
			if leg.proof != nil && !leg.proof(srv) {
				b.Fatalf("leg %s: the path was not reached", leg.name)
			}

			b.ReportAllocs()
			for b.Loop() {
				serveOne(httpSrv, body, header)
			}
		})
	}
}

// failingComplexArguments are complexArguments with the ids of its first 15
// items written as strings.
func failingComplexArguments(tb testing.TB) json.RawMessage {
	tb.Helper()
	args := []byte(complexArguments())
	for i := range 15 {
		old := fmt.Appendf(nil, `"id":%d,`, i)
		if !bytes.Contains(args, old) {
			tb.Fatalf("no item id %d to write as a string", i)
		}
		args = bytes.Replace(args, old, []byte(`"id":"bad",`), 1)
	}
	if n := bytes.Count(args, []byte(`"id":"bad",`)); n != 15 {
		tb.Fatalf("%d ids written as strings, want 15", n)
	}
	return json.RawMessage(args)
}

// refusingInputValidation is an input validation handler that reads the
// failure it is given and refuses with a result of its own.
func refusingInputValidation() ServerOption {
	return WithInputValidationHandler(func(_ context.Context, _ mcp.Tool, _ CallArguments, failure InputValidationFailure) (*mcp.CallToolResult, bool) {
		return mcp.NewToolResultError(fmt.Sprintf("refused %d+%d", len(failure.Violations), failure.Omitted)), true
	})
}
