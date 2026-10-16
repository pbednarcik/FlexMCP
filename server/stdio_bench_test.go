package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"strings"
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

// BenchmarkStdioListenLines measures the read loop's cost per byte of input:
// 200 pings whose params carry a string of the given size, back to back,
// until EOF. Above 4 KiB a line no longer fits in the reader's buffer.
func BenchmarkStdioListenLines(b *testing.B) {
	const requests = 200
	for _, size := range []int{512, 16 << 10, 256 << 10} {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			arg := strings.Repeat("y", size)
			var input bytes.Buffer
			for i := range requests {
				fmt.Fprintf(&input, `{"jsonrpc":"2.0","id":%d,"method":"ping","params":{"a":%q}}`+"\n", i+1, arg)
			}
			stdioServer := NewStdioServer(NewMCPServer("bench", "1.0.0"))
			stdioServer.SetErrorLogger(log.New(io.Discard, "", 0))
			var out bytes.Buffer
			if err := stdioServer.Listen(b.Context(), bytes.NewReader(input.Bytes()), &out); err != nil {
				b.Fatal(err)
			}
			if got := bytes.Count(out.Bytes(), []byte(`"result":{}`)); got != requests {
				b.Fatalf("answered %d of %d", got, requests)
			}
			b.SetBytes(int64(input.Len()))
			b.ReportAllocs()
			for b.Loop() {
				if err := stdioServer.Listen(b.Context(), bytes.NewReader(input.Bytes()), io.Discard); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
