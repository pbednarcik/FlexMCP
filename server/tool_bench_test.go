package server

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// BenchmarkToolResultSizeLimit measures the check WithToolResultSizeLimit
// makes on every tool result, for a text result of the given size that fits.
func BenchmarkToolResultSizeLimit(b *testing.B) {
	for _, size := range []int{512, 16 << 10, 256 << 10} {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			server := NewMCPServer("bench", "1.0.0", WithToolResultSizeLimit(2*size))
			result := mcp.NewToolResultText(strings.Repeat("x", size))
			if server.limitToolResult(result) != result {
				b.Fatal("the result was limited")
			}
			b.ReportAllocs()
			for b.Loop() {
				server.limitToolResult(result)
			}
		})
	}
}
