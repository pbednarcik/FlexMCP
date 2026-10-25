package compare

import (
	"context"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pbednarcik/flexmcp/bench/compare/harness"
)

// newHandler sets the official SDK up as its docs set up a production
// server: the generic AddTool (it decodes and validates the arguments; the
// raw Server.AddTool leaves both to the handler), stateless (the only mode
// that serves 2026-07-28), JSON answers, and a nil Logger, which logs
// nothing. go-sdk validates input on every payload.
func newHandler(p harness.Payload) http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "bench", Version: "1.0.0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: harness.ToolName, Description: "echoes region", InputSchema: harness.Schema(p)},
		func(_ context.Context, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			region, _ := args["region"].(string)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: harness.ResultText(region, p)}}}, nil, nil
		})
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
}

func TestLegsAnswer(t *testing.T) { harness.Verify(t, newHandler) }

func BenchmarkCompare(b *testing.B) { harness.Bench(b, newHandler) }
