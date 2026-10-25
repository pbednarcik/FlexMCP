package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
)

// BenchmarkStreamableHTTPToolCallValidated drives ServeHTTP with one modern
// tools/call per iteration to a tool whose input is validated: bench/compare's
// small and complex calls, the complex one with an item's id written with an
// exponent, and with a before-call hook, after which validation reads the
// arguments the hook left.
func BenchmarkStreamableHTTPToolCallValidated(b *testing.B) {
	for _, leg := range []struct {
		name     string
		complex  bool
		exponent bool
		hook     bool
	}{
		{name: "small"},
		{name: "complex", complex: true},
		{name: "complex-exponent", complex: true, exponent: true},
		{name: "complex-hook", complex: true, hook: true},
	} {
		b.Run(leg.name, func(b *testing.B) {
			options := []ServerOption{WithToolCapabilities(false), WithInputSchemaValidation()}
			if leg.hook {
				hooks := &Hooks{}
				hooks.AddBeforeCallTool(func(context.Context, any, *mcp.CallToolRequest) {})
				options = append(options, WithHooks(hooks))
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
			srv.AddTools(ServerTool{
				Tool:              mcp.NewToolWithRawSchema("echo", "", json.RawMessage(`{"type":["array","object"],"items":{"type":"integer"}}`)),
				Handler:           okHandler,
				ArgumentsOnDemand: onDemand,
			})
			const arguments = `[1,2,3,4,5,6,7,8]`
			if out := onDemandCall(b, srv, arguments); !strings.Contains(out, `"text":"ok"`) || strings.Contains(out, `"isError":true`) {
				b.Fatalf("%s: %s", name, out)
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
