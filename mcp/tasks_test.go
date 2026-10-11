package mcp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelatedTaskMeta(t *testing.T) {
	taskID := "task-123"
	meta := RelatedTaskMeta(taskID)

	assert.NotNil(t, meta)
	assert.Equal(t, taskID, meta["taskId"])
	assert.Len(t, meta, 1)
}

func TestWithRelatedTask(t *testing.T) {
	taskID := "task-456"
	meta := WithRelatedTask(taskID)

	assert.NotNil(t, meta)
	assert.NotNil(t, meta.AdditionalFields)

	// Check that the related task metadata is properly nested
	relatedTask, ok := meta.AdditionalFields[RelatedTaskMetaKey]
	assert.True(t, ok, "RelatedTaskMetaKey should exist in AdditionalFields")

	relatedTaskMap, ok := relatedTask.(map[string]any)
	assert.True(t, ok, "Related task metadata should be a map[string]any")
	assert.Equal(t, taskID, relatedTaskMap["taskId"])
}

func TestRelatedTaskMetaKey(t *testing.T) {
	// Verify the constant matches the spec
	assert.Equal(t, "io.modelcontextprotocol/related-task", RelatedTaskMetaKey)
}

func TestWithModelImmediateResponse(t *testing.T) {
	message := "Processing your request. This may take a few minutes."
	meta := WithModelImmediateResponse(message)

	assert.NotNil(t, meta)
	assert.NotNil(t, meta.AdditionalFields)

	// Check that the immediate response message is properly set
	immediateResponse, ok := meta.AdditionalFields[ModelImmediateResponseMetaKey]
	assert.True(t, ok, "ModelImmediateResponseMetaKey should exist in AdditionalFields")

	responseMsg, ok := immediateResponse.(string)
	assert.True(t, ok, "Immediate response should be a string")
	assert.Equal(t, message, responseMsg)
}

func TestModelImmediateResponseMetaKey(t *testing.T) {
	// Verify the constant matches the spec
	assert.Equal(t, "io.modelcontextprotocol/model-immediate-response", ModelImmediateResponseMetaKey)
}

func TestCreateTaskResultWithModelImmediateResponse(t *testing.T) {
	// Test that CreateTaskResult can include model immediate response metadata
	task := NewTask("task-789")
	message := "Your request is being processed in the background."

	result := CreateTaskResult{
		Task: task,
		Meta: WithModelImmediateResponse(message),
	}

	assert.Equal(t, task.TaskId, result.Task.TaskId)
	assert.NotNil(t, result.Meta)
	assert.NotNil(t, result.Meta.AdditionalFields)

	immediateResponse, ok := result.Meta.AdditionalFields[ModelImmediateResponseMetaKey]
	assert.True(t, ok)
	assert.Equal(t, message, immediateResponse)
}

func TestNewTaskDefaultsToWorkingNow(t *testing.T) {
	before := time.Now().UTC().Truncate(time.Second)
	task := NewTask("task-0")

	assert.Equal(t, "task-0", task.TaskId)
	assert.Equal(t, TaskStatusWorking, task.Status)
	assert.Equal(t, task.CreatedAt, task.LastUpdatedAt)
	assert.Nil(t, task.TTL)
	assert.Nil(t, task.PollInterval)

	createdAt, err := time.Parse(time.RFC3339, task.CreatedAt)
	require.NoError(t, err)
	assert.False(t, createdAt.Before(before))
}

func TestNewTaskOptionsSurviveTheWire(t *testing.T) {
	task := NewTask("task-1",
		WithTaskStatus(TaskStatusCompleted),
		WithTaskStatusMessage("done"),
		WithTaskTTL(60_000),
		WithTaskPollInterval(500),
		WithTaskCreatedAt("2026-01-02T03:04:05Z"),
	)
	assert.Equal(t, "task-1", Named(task).GetName(), "pagination keys tasks by id")

	result := NewGetTaskResult(task)
	result.Meta = WithRelatedTask("parent-task")

	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"_meta": {"io.modelcontextprotocol/related-task": {"taskId": "parent-task"}},
		"taskId": "task-1",
		"status": "completed",
		"statusMessage": "done",
		"createdAt": "2026-01-02T03:04:05Z",
		"lastUpdatedAt": "`+task.LastUpdatedAt+`",
		"ttl": 60000,
		"pollInterval": 500
	}`, string(encoded))

	raw := json.RawMessage(encoded)
	parsed, err := ParseGetTaskResult(&raw)
	require.NoError(t, err)
	assert.Equal(t, task, parsed.Task)
	require.NotNil(t, parsed.Meta)
	assert.Equal(t, map[string]any{"taskId": "parent-task"}, parsed.Meta.AdditionalFields[RelatedTaskMetaKey])
}

func TestParseListTasksResultRoundTrip(t *testing.T) {
	working := NewTask("a", WithTaskCreatedAt("2026-01-01T00:00:00Z"), WithTaskTTL(1000))
	failed := NewTask("b", WithTaskStatus(TaskStatusFailed), WithTaskStatusMessage("boom"))

	result := NewListTasksResult([]Task{working, failed})
	next := Cursor("page-2")
	result.NextCursor = &next
	result.Meta = WithModelImmediateResponse("listing")

	encoded, err := json.Marshal(result)
	require.NoError(t, err)

	raw := json.RawMessage(encoded)
	parsed, err := ParseListTasksResult(&raw)
	require.NoError(t, err)
	assert.Equal(t, []Task{working, failed}, parsed.Tasks)
	require.NotNil(t, parsed.NextCursor)
	assert.Equal(t, Cursor("page-2"), *parsed.NextCursor)
	require.NotNil(t, parsed.Meta)
	assert.Equal(t, "listing", parsed.Meta.AdditionalFields[ModelImmediateResponseMetaKey])
}

func TestParseCancelTaskResultRoundTrip(t *testing.T) {
	task := NewTask("task-3", WithTaskStatus(TaskStatusCancelled), WithTaskStatusMessage("cancelled by user"))
	result := NewCancelTaskResult(task)
	result.Meta = WithRelatedTask("task-3")

	encoded, err := json.Marshal(result)
	require.NoError(t, err)

	raw := json.RawMessage(encoded)
	parsed, err := ParseCancelTaskResult(&raw)
	require.NoError(t, err)
	assert.Equal(t, task, parsed.Task)
	assert.True(t, parsed.Status.IsTerminal())
	require.NotNil(t, parsed.Meta)
	assert.Equal(t, map[string]any{"taskId": "task-3"}, parsed.Meta.AdditionalFields[RelatedTaskMetaKey])
}

func TestParseTaskResultsTolerateFieldsOfTheWrongType(t *testing.T) {
	// A peer that sends a field with the wrong JSON type loses that field,
	// not the whole result.
	raw := json.RawMessage(`{
		"_meta": "not-an-object",
		"tasks": [
			{"taskId": 1, "status": "working", "ttl": "soon", "pollInterval": null},
			"not-a-task",
			{"taskId": "ok", "status": 2, "statusMessage": 3, "createdAt": 4, "lastUpdatedAt": 5}
		],
		"nextCursor": 7
	}`)

	parsed, err := ParseListTasksResult(&raw)
	require.NoError(t, err)
	assert.Nil(t, parsed.Meta)
	assert.Nil(t, parsed.NextCursor)
	assert.Equal(t, []Task{{Status: TaskStatusWorking}, {TaskId: "ok"}}, parsed.Tasks)
}

func TestParseTaskResultsRefuseBadPayloads(t *testing.T) {
	malformed := json.RawMessage(`{"taskId": `)
	parsers := []struct {
		name  string
		parse func(*json.RawMessage) error
	}{
		{"get", func(raw *json.RawMessage) error { _, err := ParseGetTaskResult(raw); return err }},
		{"list", func(raw *json.RawMessage) error { _, err := ParseListTasksResult(raw); return err }},
		{"cancel", func(raw *json.RawMessage) error { _, err := ParseCancelTaskResult(raw); return err }},
		{"result", func(raw *json.RawMessage) error { _, err := ParseTaskResultResult(raw); return err }},
	}

	for _, tt := range parsers {
		t.Run(tt.name, func(t *testing.T) {
			assert.EqualError(t, tt.parse(nil), "response is nil")
			assert.ErrorContains(t, tt.parse(&malformed), "failed to unmarshal response")
		})
	}
}

func TestTaskStatusNotificationWire(t *testing.T) {
	task := NewTask("task-4",
		WithTaskStatus(TaskStatusFailed),
		WithTaskStatusMessage("disk full"),
		WithTaskCreatedAt("2026-01-01T00:00:00Z"),
	)

	encoded, err := json.Marshal(NewTaskStatusNotification(task))
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"method": "notifications/tasks/status",
		"params": {
			"taskId": "task-4",
			"status": "failed",
			"statusMessage": "disk full",
			"createdAt": "2026-01-01T00:00:00Z",
			"lastUpdatedAt": "`+task.LastUpdatedAt+`",
			"ttl": null
		}
	}`, string(encoded))

	var decoded TaskStatusNotification
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, task, decoded.Params.Task)
}

func TestTasksCapabilityWire(t *testing.T) {
	tests := []struct {
		name       string
		capability *TasksCapability
		wire       string
	}{
		{"every operation", NewTasksCapability(), `{"list":{},"cancel":{},"requests":{"tools":{"call":{}}}}`},
		{"tool calls only", NewTasksCapabilityWithToolsOnly(), `{"requests":{"tools":{"call":{}}}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			encoded, err := json.Marshal(tt.capability)
			require.NoError(t, err)
			assert.JSONEq(t, tt.wire, string(encoded))

			var decoded TasksCapability
			require.NoError(t, json.Unmarshal(encoded, &decoded))
			assert.Equal(t, tt.capability, &decoded)
		})
	}
}

func TestTaskStatusIsTerminal(t *testing.T) {
	tests := []struct {
		status TaskStatus
		want   bool
	}{
		{TaskStatusWorking, false},
		{TaskStatusInputRequired, false},
		{TaskStatusCompleted, true},
		{TaskStatusFailed, true},
		{TaskStatusCancelled, true},
		{TaskStatus(""), false},
	}

	for _, tt := range tests {
		t.Run(string(tt.status), func(t *testing.T) {
			assert.Equal(t, tt.want, tt.status.IsTerminal())
		})
	}
}

func TestNewTaskParamsOmitsAbsentTTL(t *testing.T) {
	ttl := int64(5000)
	encoded, err := json.Marshal(NewTaskParams(&ttl))
	require.NoError(t, err)
	assert.JSONEq(t, `{"ttl":5000}`, string(encoded))

	encoded, err = json.Marshal(NewTaskParams(nil))
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(encoded))
}
