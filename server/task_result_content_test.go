package server

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// TestTaskResultAnswersAnEmptyContentArray checks that tasks/result of a tool
// call, plain or task tool, whose result has no content answers
// "content":[], as tools/call would have.
func TestTaskResultAnswersAnEmptyContentArray(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content []mcp.Content
	}{{"nil", nil}, {"empty", []mcp.Content{}}} {
		for _, kind := range []string{"tool", "task_tool"} {
			t.Run(kind+"/"+tt.name, func(t *testing.T) {
				s := NewMCPServer("test", "1", WithTaskCapabilities(true, true, true))
				entry, err := s.createTask(t.Context(), "task", "tool", nil, nil)
				require.NoError(t, err)
				structured := map[string]any{"ok": true}
				if kind == "task_tool" {
					s.completeTask(entry, &mcp.CreateTaskResult{Content: tt.content, StructuredContent: structured}, nil)
				} else {
					s.completeTask(entry, &mcp.CallToolResult{Content: tt.content, StructuredContent: structured}, nil)
				}

				response := s.HandleMessage(t.Context(), []byte(
					`{"jsonrpc":"2.0","id":1,"method":"tasks/result","params":{"taskId":"task"}}`))
				data, err := json.Marshal(response)
				require.NoError(t, err)
				var wire struct {
					Result map[string]json.RawMessage `json:"result"`
				}
				require.NoError(t, json.Unmarshal(data, &wire))
				require.NotNil(t, wire.Result, string(data))
				require.Contains(t, wire.Result, "content", string(data))
				assert.JSONEq(t, `[]`, string(wire.Result["content"]))
				assert.JSONEq(t, `{"ok":true}`, string(wire.Result["structuredContent"]))
			})
		}
	}
}
