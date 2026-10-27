package mcp

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var _ ResultMetadata = (*ExtensionTaskStateResult)(nil)

func TestTasksMethodsCarryTheTaskIdInMcpName(t *testing.T) {
	assert.Equal(t, MCPMethod("tasks/update"), MethodTasksUpdate)

	for _, method := range []MCPMethod{MethodTasksGet, MethodTasksCancel, MethodTasksUpdate} {
		t.Run(string(method), func(t *testing.T) {
			assert.True(t, MethodRequiresNameHeader(method))

			name, ok := ExtractHeaderName(method, json.RawMessage(`{"taskId":"abc"}`))
			assert.True(t, ok)
			assert.Equal(t, "abc", name)
		})
	}
}

func TestHeaderMismatchOnATaskMethodDoesNotEchoTheId(t *testing.T) {
	const headerID, bodyID = "header-task-id-7f3a", "body-task-id-91c2"

	for _, method := range []MCPMethod{MethodTasksGet, MethodTasksCancel, MethodTasksUpdate} {
		t.Run(string(method), func(t *testing.T) {
			header := http.Header{}
			header.Set(HeaderMethod, string(method))
			header.Set(HeaderName, headerID)

			err := ValidateStandardHeaders(header.Get, ProtocolVersion20260728, method,
				json.RawMessage(`{"taskId":"`+bodyID+`"}`))
			require.Error(t, err)
			assert.True(t, IsHeaderMismatch(err), "want a header mismatch, got %v", err)
			assert.NotContains(t, err.Error(), headerID)
			assert.NotContains(t, err.Error(), bodyID)
		})
	}
}

func TestExtensionTaskStateResultWire(t *testing.T) {
	created := "2026-01-01T00:00:00Z"
	updated := "2026-01-01T00:00:05Z"
	ttl := int64(60000)

	tests := []struct {
		name   string
		result ExtensionTaskStateResult
		want   string
	}{
		{
			name: "a completed state carries the result",
			result: ExtensionTaskStateResult{
				Result:        Result{ResultType: ResultTypeComplete},
				ExtensionTask: ExtensionTask{TaskId: "t1", Status: TaskStatusCompleted, CreatedAt: created, LastUpdatedAt: updated, TTLMs: &ttl},
				TaskResult: &CallToolResult{
					ResultType: ResultTypeComplete,
					Content:    []Content{NewTextContent("done")},
				},
			},
			want: `{"resultType":"complete","taskId":"t1","status":"completed","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:05Z","ttlMs":60000,` +
				`"result":{"resultType":"complete","content":[{"type":"text","text":"done"}]}}`,
		},
		{
			name: "a failed state carries the error",
			result: ExtensionTaskStateResult{
				Result:        Result{ResultType: ResultTypeComplete},
				ExtensionTask: ExtensionTask{TaskId: "t2", Status: TaskStatusFailed, CreatedAt: created, LastUpdatedAt: updated, TTLMs: &ttl},
				Error:         &JSONRPCErrorDetails{Code: INTERNAL_ERROR, Message: "boom"},
			},
			want: `{"resultType":"complete","taskId":"t2","status":"failed","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:05Z","ttlMs":60000,` +
				`"error":{"code":-32603,"message":"boom"}}`,
		},
		{
			name: "an input_required state carries the input requests",
			result: ExtensionTaskStateResult{
				Result:        Result{ResultType: ResultTypeComplete},
				ExtensionTask: ExtensionTask{TaskId: "t3", Status: TaskStatusInputRequired, CreatedAt: created, LastUpdatedAt: updated, TTLMs: &ttl},
				InputRequests: InputRequests{"roots": NewRootsInputRequest()},
			},
			want: `{"resultType":"complete","taskId":"t3","status":"input_required","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:05Z","ttlMs":60000,` +
				`"inputRequests":{"roots":{"method":"roots/list"}}}`,
		},
		{
			name: "ttlMs is present as null when unlimited",
			result: ExtensionTaskStateResult{
				Result:        Result{ResultType: ResultTypeComplete},
				ExtensionTask: ExtensionTask{TaskId: "t4", Status: TaskStatusWorking, CreatedAt: created, LastUpdatedAt: updated},
			},
			want: `{"resultType":"complete","taskId":"t4","status":"working","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:05Z","ttlMs":null}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := json.Marshal(tt.result)
			require.NoError(t, err)
			assert.JSONEq(t, tt.want, string(encoded))
		})
	}
}

func TestParseExtensionTaskStateResultToleratesAnInnerResultWithoutResultType(t *testing.T) {
	raw := json.RawMessage(`{"resultType":"complete","taskId":"t1","status":"completed",` +
		`"createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:05Z","ttlMs":null,` +
		`"result":{"content":[{"type":"text","text":"done"}]}}`)

	state, err := ParseExtensionTaskStateResult(&raw)
	require.NoError(t, err)
	assert.Equal(t, ResultTypeComplete, state.ResultType)
	assert.Equal(t, "t1", state.TaskId)
	assert.Equal(t, TaskStatusCompleted, state.Status)
	assert.Nil(t, state.TTLMs)
	require.NotNil(t, state.TaskResult)
	assert.Equal(t, ResultType(""), state.TaskResult.ResultType)
	require.Len(t, state.TaskResult.Content, 1)
	assert.Equal(t, "done", state.TaskResult.Content[0].(TextContent).Text)

	t.Run("a failed state keeps its error", func(t *testing.T) {
		raw := json.RawMessage(`{"resultType":"complete","taskId":"t2","status":"failed",` +
			`"createdAt":"c","lastUpdatedAt":"u","ttlMs":1,"error":{"code":-32603,"message":"boom"}}`)
		state, err := ParseExtensionTaskStateResult(&raw)
		require.NoError(t, err)
		require.NotNil(t, state.Error)
		assert.Equal(t, INTERNAL_ERROR, state.Error.Code)
		assert.Equal(t, "boom", state.Error.Message)
		assert.Nil(t, state.TaskResult)
	})

	t.Run("an input_required state keeps its requests", func(t *testing.T) {
		raw := json.RawMessage(`{"resultType":"complete","taskId":"t3","status":"input_required",` +
			`"createdAt":"c","lastUpdatedAt":"u","ttlMs":null,"inputRequests":{"roots":{"method":"roots/list"}}}`)
		state, err := ParseExtensionTaskStateResult(&raw)
		require.NoError(t, err)
		require.Contains(t, state.InputRequests, "roots")
		assert.Equal(t, MethodListRoots, state.InputRequests["roots"].Method)
	})

	t.Run("a nil message and a malformed one are refused", func(t *testing.T) {
		_, err := ParseExtensionTaskStateResult(nil)
		assert.Error(t, err)

		bad := json.RawMessage(`{"taskId":5}`)
		_, err = ParseExtensionTaskStateResult(&bad)
		assert.Error(t, err)
	})
}
