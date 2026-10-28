package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rawResults are results whose structuredContent, as it is written, is raw:
// a result carrying RawStructuredContent is written from it, whatever
// StructuredContent holds, so raw is what output validation must judge.
// refused says whether the written structured content breaks raw_output's
// schema, which wants an object with a string value.
var rawResults = map[string]struct {
	raw     string
	decoded any
	refused bool
}{
	"raw only, invalid":                  {raw: `{"value":1}`, refused: true},
	"invalid raw over a valid decoding":  {raw: `{"value":1}`, decoded: map[string]any{"value": "ok"}, refused: true},
	"valid raw over an invalid decoding": {raw: `{"value":"ok"}`, decoded: map[string]any{"value": 1}, refused: false},
	"raw only, valid":                    {raw: `{"value":"ok"}`, refused: false},
}

// rawResult is the rawResults entry name as a result.
func rawResult(name string) *mcp.CallToolResult {
	tc := rawResults[name]
	result := mcp.NewToolResultText("raw output")
	result.RawStructuredContent = json.RawMessage(tc.raw)
	result.StructuredContent = tc.decoded
	return result
}

// The synchronous tools/call and the 2025-11-25 task-augmented one validate
// the structured content that is written: the raw bytes, when there are any.
func TestOutputValidationJudgesTheWrittenStructuredContent(t *testing.T) {
	for name, tc := range rawResults {
		for _, task := range []bool{false, true} {
			label := "synchronous"
			if task {
				label = "task-augmented"
			}
			t.Run(label+"/"+name, func(t *testing.T) {
				srv := NewMCPServer("test", "1.0.0", WithOutputSchemaValidation(), WithTaskCapabilities(true, true, true))
				srv.AddTool(withTasks(rawOutputSchemaTool()), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return rawResult(name), nil
				})
				assert.Equal(t, tc.refused, outputRefused(t, srv, task))
			})
		}
	}
}

// tasks/get validates a completed task's result against its origin's schema
// as the synchronous call does: the raw bytes, when there are any.
func TestExtensionGetTaskJudgesTheWrittenStructuredContent(t *testing.T) {
	for name, tc := range rawResults {
		t.Run(name, func(t *testing.T) {
			f := newTaskFixture(t, false, WithOutputSchemaValidation())
			f.addTool(json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`))
			id := f.handOver(t, t.Context(), "raw-job")
			f.tasks.set("raw-job", func(s *TaskState) {
				s.Status = mcp.TaskStatusCompleted
				s.Result = rawResult(name)
			})
			result := completeResult(t, sendTaskMethod(t, t.Context(), f.srv, mcp.MethodTasksGet, map[string]any{"taskId": id}, declaringTasks()))
			inner, ok := result["result"].(map[string]any)
			require.True(t, ok, "expected a result, got %v", result)
			assert.Equal(t, tc.refused, inner["isError"] == true, "%v", inner)
		})
	}
}
