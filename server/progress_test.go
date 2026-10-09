package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// sseData returns the JSON payload of every "data:" line on an SSE body, in
// arrival order, decoded to a map; the body is read to its end.
func sseData(t *testing.T, resp *http.Response) []map[string]any {
	t.Helper()
	var events []map[string]any
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		data, ok := strings.CutPrefix(scanner.Text(), "data: ")
		if !ok {
			continue
		}
		var event map[string]any
		require.NoError(t, json.Unmarshal([]byte(data), &event), "bad SSE data line: %s", data)
		events = append(events, event)
	}
	require.NoError(t, scanner.Err())
	return events
}

// TestModernToolCallStreamsProgressBeforeTheResult pins what Claude Code
// relies on when it sends a progressToken: a handler's progress
// notifications for that token are delivered on the same POST, the response
// becomes an SSE stream, the notifications arrive in the order sent, and the
// result is the last event.
func TestModernToolCallStreamsProgressBeforeTheResult(t *testing.T) {
	mcpServer := NewMCPServer("progress-test", "1.0.0", WithToolCapabilities(false))
	mcpServer.AddTool(mcp.NewTool("count"), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		token := request.Params.Meta.ProgressToken
		for _, step := range []float64{1, 2} {
			if err := mcpServer.SendNotificationToClient(ctx, "notifications/progress", map[string]any{
				"progressToken": token, "progress": step, "total": 2,
			}); err != nil {
				return nil, err
			}
		}
		return mcp.NewToolResultText("done"), nil
	})
	srv := NewTestStreamableHTTPServer(mcpServer)
	t.Cleanup(srv.Close)

	// The call is repeated because the notification forwarder and the
	// response path race: a lost notification shows only in some runs.
	for i := range 25 {
		meta := modernMeta()
		meta["progressToken"] = "p-1"
		resp := postModernWithMeta(t, srv.URL, mcp.MethodToolsCall, map[string]any{"name": "count"}, meta)

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"), "a call that emitted notifications answers as a stream")
		events := sseData(t, resp)
		require.Len(t, events, 3, "call %d: two progress notifications, then the result: %v", i, events)

		assert.Equal(t, "notifications/progress", events[0]["method"])
		assert.Equal(t, map[string]any{"progressToken": "p-1", "progress": float64(1), "total": float64(2)}, events[0]["params"])
		assert.Equal(t, "notifications/progress", events[1]["method"])
		assert.Equal(t, map[string]any{"progressToken": "p-1", "progress": float64(2), "total": float64(2)}, events[1]["params"])

		assert.Equal(t, float64(1), events[2]["id"], "the result answers the request's ID")
		result, ok := events[2]["result"].(map[string]any)
		require.True(t, ok, "the last event is the result, got %v", events[2])
		content := result["content"].([]any)
		require.Len(t, content, 1)
		assert.Equal(t, "done", content[0].(map[string]any)["text"])
	}
}
