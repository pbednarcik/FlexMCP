package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// bigRawSchema builds an input schema of about size bytes; annotated adds one
// x-mcp-header property.
func bigRawSchema(size int, annotated bool) json.RawMessage {
	var b strings.Builder
	b.WriteString(`{"type":"object","properties":{`)
	if annotated {
		b.WriteString(`"region":{"type":"string","x-mcp-header":"Region"},`)
	} else {
		b.WriteString(`"region":{"type":"string"},`)
	}
	for i := 0; b.Len() < size-40; i++ {
		fmt.Fprintf(&b, `"p%03d":{"type":"string","description":"property number %03d"},`, i, i)
	}
	b.WriteString(`"last":{"type":"string"}}}`)
	return json.RawMessage(b.String())
}

// BenchmarkParamHeadersValidation measures the header check a 2026-07-28
// tools/call pays per request, against an 8 KB schema.
func BenchmarkParamHeadersValidation(b *testing.B) {
	for _, tc := range []struct {
		name      string
		annotated bool
	}{{"annotated", true}, {"plain", false}} {
		b.Run(tc.name, func(b *testing.B) {
			srv := NewMCPServer("bench", "1.0.0", WithToolCapabilities(true))
			srv.AddTool(mcp.NewToolWithRawSchema("big", "a big tool", bigRawSchema(8192, tc.annotated)), nil)
			message := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"big","arguments":{"region":"us-east-1"}}}`)
			headers := http.Header{}
			headers.Set(mcp.HeaderProtocolVersion, mcp.ProtocolVersion20260728)
			headers.Set(mcp.HeaderMethod, string(mcp.MethodToolsCall))
			headers.Set(mcp.HeaderName, "big")
			if tc.annotated {
				headers.Set(mcp.HeaderParamPrefix+"Region", "us-east-1")
			}
			ctx := b.Context()
			if err := srv.validateStandardHeadersForMessage(ctx, headers, mcp.ProtocolVersion20260728, mcp.MethodToolsCall, message); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				_ = srv.validateStandardHeadersForMessage(ctx, headers, mcp.ProtocolVersion20260728, mcp.MethodToolsCall, message)
			}
		})
	}
}

// BenchmarkStreamableHTTPToolCallHeaders drives ServeHTTP with one modern
// tools/call per iteration to a tool that binds 0, 1 or 3 of its three
// arguments to Mcp-Param headers, each header sent with its argument's value.
func BenchmarkStreamableHTTPToolCallHeaders(b *testing.B) {
	names := []string{"region", "zone", "rack"}
	headers := []string{"Region", "Zone", "Rack"}
	for _, bound := range []int{0, 1, 3} {
		b.Run(fmt.Sprintf("bindings=%d", bound), func(b *testing.B) {
			var properties strings.Builder
			arguments := make(map[string]any, len(names))
			for i, name := range names {
				if i > 0 {
					properties.WriteByte(',')
				}
				annotation := ""
				if i < bound {
					annotation = `,"x-mcp-header":"` + headers[i] + `"`
				}
				fmt.Fprintf(&properties, `"%s":{"type":"string"%s}`, name, annotation)
				arguments[name] = "us-east-1"
			}
			schema := json.RawMessage(`{"type":"object","properties":{` + properties.String() + `},"required":["region"]}`)
			srv := NewMCPServer("bench", "1.0.0", WithToolCapabilities(false))
			srv.AddTool(mcp.NewToolWithRawSchema("echo", "echoes region", schema),
				func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return mcp.NewToolResultText(request.GetString("region", "")), nil
				})
			httpSrv := NewStreamableHTTPServer(srv)
			body, header := modernToolCall(b, arguments)
			for _, name := range headers[:bound] {
				header.Set(mcp.HeaderParamPrefix+name, "us-east-1")
			}

			// One call outside the loop proves the leg answers the region, and
			// a bound header that does not match its argument is refused.
			if w := serveOne(httpSrv, body, header); w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"text":"us-east-1"`)) {
				b.Fatalf("bindings %d: status %d body %.300s", bound, w.Code, w.Body.String())
			}
			if bound > 0 {
				mismatched := header.Clone()
				mismatched.Set(mcp.HeaderParamPrefix+headers[0], "eu-west-1")
				if w := serveOne(httpSrv, body, mismatched); !bytes.Contains(w.Body.Bytes(), []byte("does not match")) {
					b.Fatalf("bindings %d: mismatched header not refused: status %d body %.300s", bound, w.Code, w.Body.String())
				}
			}

			b.ReportAllocs()
			for b.Loop() {
				serveOne(httpSrv, body, header)
			}
		})
	}
}
