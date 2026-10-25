package compare

import (
	"context"
	"log/slog"
	"net/http"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/pbednarcik/flexmcp/bench/compare/harness"
)

// newHandler sets upstream mcp-go up as its docs set up a production
// server: stateless, JSON answers, logging off, input validation on for the
// Complex payload (opt-in in mcp-go).
func newHandler(p harness.Payload) http.Handler {
	opts := []server.ServerOption{server.WithToolCapabilities(false)}
	if p == harness.Complex {
		opts = append(opts, server.WithInputSchemaValidation())
	}
	srv := server.NewMCPServer("bench", "1.0.0", opts...)
	srv.AddTool(mcp.NewToolWithRawSchema(harness.ToolName, "echoes region", harness.Schema(p)),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText(harness.ResultText(req.GetString("region", ""), p)), nil
		})
	return server.NewStreamableHTTPServer(srv,
		server.WithStateLess(true),
		server.WithStreamableHTTPLogger(slog.New(slog.DiscardHandler)))
}

func TestLegsAnswer(t *testing.T) { harness.Verify(t, newHandler) }

func BenchmarkCompare(b *testing.B) { harness.Bench(b, newHandler) }
