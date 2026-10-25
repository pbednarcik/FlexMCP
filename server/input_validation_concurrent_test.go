package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// Validation errors are rendered through one shared message.Printer; each of
// its calls takes its own state from a pool, so concurrent calls with invalid
// arguments each get their own rendering, whole and unmixed. Run with -race.
func TestInputValidationRendersErrorsConcurrently(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithInputSchemaValidation())
	tool := mcp.NewToolWithRawSchema("bounded", "a bounded limit", json.RawMessage(
		`{"type":"object","properties":{"limit":{"type":"number","maximum":5}},"required":["limit"]}`))
	srv.AddTool(tool, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ran"), nil
	})

	const workers, calls, distinct = 32, 25, 16
	call := func(n int) string {
		message := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"bounded","arguments":{"limit":%d}}}`, 6+n)
		b, err := json.Marshal(srv.HandleMessage(t.Context(), json.RawMessage(message)))
		if err != nil {
			return err.Error()
		}
		return string(b)
	}

	// Each limit is rendered once alone first: the answer every concurrent
	// call with that limit must give.
	want := make([]string, distinct)
	for n := range want {
		want[n] = call(n)
		require.Contains(t, want[n], `"isError":true`)
		require.Contains(t, want[n], fmt.Sprintf("input schema validation failed: /limit: maximum: got %d, want 5\"", 6+n))
	}

	got := make([][]string, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			got[w] = make([]string, calls)
			for c := range calls {
				got[w][c] = call((w + c) % distinct)
			}
		})
	}
	wg.Wait()
	for w := range workers {
		for c := range calls {
			assert.Equal(t, want[(w+c)%distinct], got[w][c], "worker %d call %d", w, c)
		}
	}
}
