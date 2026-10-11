package server

import (
	"bytes"
	"context"
	"io"
	"log"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// BenchmarkStdioListen measures the stdio read loop: Listen over a stdin
// holding requests back to back, answers to io.Discard, until EOF. One
// iteration is the whole input, so the per-request cost is the figure divided
// by the request count.
func BenchmarkStdioListen(b *testing.B) {
	const requests = 1000
	mcpServer := NewMCPServer("bench", "1.0.0", WithToolCapabilities(false))
	mcpServer.AddTool(mcp.NewTool("echo"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	var input bytes.Buffer
	for range requests {
		input.WriteString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
	}
	stdioServer := NewStdioServer(mcpServer)
	stdioServer.SetErrorLogger(log.New(io.Discard, "", 0))

	b.ReportAllocs()
	for b.Loop() {
		if err := stdioServer.Listen(b.Context(), bytes.NewReader(input.Bytes()), io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}
