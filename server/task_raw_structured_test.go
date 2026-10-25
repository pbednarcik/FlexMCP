package server

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// TestTaskResultPreservesRawStructuredContent checks that tasks/result
// forwards a stored tool result's structured content with its numbers exactly
// as the tool produced them, through encoding/json and encoding/json/v2.
func TestTaskResultPreservesRawStructuredContent(t *testing.T) {
	s := NewMCPServer("test", "1")
	entry, err := s.createTask(t.Context(), "task", "tool", nil, nil)
	require.NoError(t, err)
	var stored mcp.CallToolResult
	require.NoError(t, json.Unmarshal([]byte(`{"content":[],"structuredContent":{"id":9007199254740993}}`), &stored))
	s.completeTask(entry, &stored, nil)
	request := mcp.TaskResultRequest{}
	request.Params.TaskId = "task"
	result, requestErr := s.handleTaskResult(t.Context(), 1, request)
	require.Nil(t, requestErr)
	for name, marshal := range map[string]func(any) ([]byte, error){
		"v1": json.Marshal,
		"v2": func(v any) ([]byte, error) { return jsonv2.Marshal(v) },
	} {
		t.Run(name, func(t *testing.T) {
			encoded, err := marshal(*result)
			require.NoError(t, err)
			require.Contains(t, string(encoded), `"id":9007199254740993`)
		})
	}
}
