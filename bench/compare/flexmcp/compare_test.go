package compare

import (
	"context"
	"log/slog"
	"net/http"
	"testing"

	"github.com/pbednarcik/flexmcp/bench/compare/harness"
	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
)

// newHandler sets FlexMCP up as its docs set up a production server:
// sessionless (its only mode), JSON answers, logging off, input validation
// on for the Complex payload (opt-in in FlexMCP).
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
	return server.NewStreamableHTTPServer(srv, server.WithStreamableHTTPLogger(slog.New(slog.DiscardHandler)))
}

func TestLegsAnswer(t *testing.T) { harness.Verify(t, newHandler) }

func BenchmarkCompare(b *testing.B) { harness.Bench(b, newHandler) }
