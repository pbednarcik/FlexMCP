package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// BenchmarkStreamableHTTPToolCallOnDemand drives ServeHTTP with one modern
// tools/call per iteration to a tool that reads its arguments at once and to
// the same tool opted in to ServerTool.ArgumentsOnDemand: bench/compare's
// complex call to a typed handler and to one that reads forty of its
// arguments through the getters, and its small call to a typed handler.
func BenchmarkStreamableHTTPToolCallOnDemand(b *testing.B) {
	type smallTypedArgs struct {
		Region string `json:"region"`
	}
	fillers := make([]string, 40)
	for i := range fillers {
		fillers[i] = fmt.Sprintf("p%03d", i)
	}
	for _, leg := range []struct {
		name    string
		complex bool
		getters bool
	}{
		{name: "complex-typed", complex: true},
		{name: "complex-getters", complex: true, getters: true},
		{name: "small-typed"},
	} {
		for _, onDemand := range []bool{false, true} {
			mode := "eager"
			if onDemand {
				mode = "ondemand"
			}
			b.Run(leg.name+"/"+mode, func(b *testing.B) {
				// argumentsNil is what the handler last saw of Params.Arguments.
				var argumentsNil bool
				var handler ToolHandlerFunc
				switch {
				case leg.getters:
					handler = func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
						argumentsNil = request.Params.Arguments == nil
						n := 0
						for _, name := range fillers {
							n += len(request.GetString(name, ""))
						}
						if n == 0 {
							return nil, errors.New("no filler arguments")
						}
						return mcp.NewToolResultText(request.GetString("region", "") + complexResultFiller), nil
					}
				case leg.complex:
					handler = mcp.NewTypedToolHandler(func(_ context.Context, request mcp.CallToolRequest, a complexTypedArgs) (*mcp.CallToolResult, error) {
						argumentsNil = request.Params.Arguments == nil
						return mcp.NewToolResultText(a.Region + complexResultFiller), nil
					})
				default:
					handler = mcp.NewTypedToolHandler(func(_ context.Context, request mcp.CallToolRequest, a smallTypedArgs) (*mcp.CallToolResult, error) {
						argumentsNil = request.Params.Arguments == nil
						return mcp.NewToolResultText(a.Region), nil
					})
				}
				schema, arguments := json.RawMessage(smallSchema), json.RawMessage(smallArguments)
				if leg.complex {
					schema, arguments = complexRawSchema(), complexArguments()
				}
				srv := NewMCPServer("bench", "1.0.0", WithToolCapabilities(false))
				srv.AddTools(ServerTool{
					Tool:              mcp.NewToolWithRawSchema("echo", "echoes region", schema),
					Handler:           handler,
					ArgumentsOnDemand: onDemand,
				})
				httpSrv := NewStreamableHTTPServer(srv)
				body, header := modernToolCall(b, arguments)

				// One call outside the loop proves the leg answers the region,
				// its handler given Params.Arguments nil under the opt-in alone.
				w := serveOne(httpSrv, body, header)
				if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"text":"us-east-1`)) || argumentsNil != onDemand {
					b.Fatalf("leg %s/%s: status %d, Params.Arguments nil %v, body %.300s", leg.name, mode, w.Code, argumentsNil, w.Body.String())
				}

				b.ReportAllocs()
				for b.Loop() {
					serveOne(httpSrv, body, header)
				}
			})
		}
	}
}
