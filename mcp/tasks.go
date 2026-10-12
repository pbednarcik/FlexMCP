package mcp

import (
	"time"
)

// TaskOption is a function that configures a Task.
// It provides a flexible way to set various properties of a Task using the functional options pattern.
type TaskOption func(*Task)

//
// Core Task Functions
//

// NewTask creates a new Task with the given ID and options.
// The task will be configured based on the provided options.
// Options are applied in order, allowing for flexible task configuration.
func NewTask(taskId string, opts ...TaskOption) Task {
	now := time.Now().UTC().Format(time.RFC3339)
	task := Task{
		TaskId:        taskId,
		Status:        TaskStatusWorking,
		CreatedAt:     now,
		LastUpdatedAt: now,
	}

	for _, opt := range opts {
		opt(&task)
	}

	return task
}

// WithTaskStatus sets the status of the task.
func WithTaskStatus(status TaskStatus) TaskOption {
	return func(t *Task) {
		t.Status = status
	}
}

// WithTaskStatusMessage sets a human-readable status message for the task.
func WithTaskStatusMessage(message string) TaskOption {
	return func(t *Task) {
		t.StatusMessage = message
	}
}

// WithTaskTTL sets the time-to-live for the task in milliseconds.
// After this duration from creation, the task may be deleted.
func WithTaskTTL(ttlMs int64) TaskOption {
	return func(t *Task) {
		t.TTL = &ttlMs
	}
}

// WithTaskPollInterval sets the suggested polling interval in milliseconds.
func WithTaskPollInterval(intervalMs int64) TaskOption {
	return func(t *Task) {
		t.PollInterval = &intervalMs
	}
}

// WithTaskCreatedAt sets a specific creation timestamp for the task.
// By default, NewTask uses the current time.
func WithTaskCreatedAt(createdAt string) TaskOption {
	return func(t *Task) {
		t.CreatedAt = createdAt
	}
}

//
// Task Helper Functions
//

// NewTaskParams creates TaskParams with the given TTL.
func NewTaskParams(ttlMs *int64) TaskParams {
	return TaskParams{
		TTL: ttlMs,
	}
}

// NewCreateTaskResult creates a CreateTaskResult with the given task.
func NewCreateTaskResult(task Task) CreateTaskResult {
	return CreateTaskResult{
		Task: task,
	}
}

// NewGetTaskResult creates a GetTaskResult from a Task.
func NewGetTaskResult(task Task) GetTaskResult {
	return GetTaskResult{
		Task: task,
	}
}

// NewListTasksResult creates a ListTasksResult with the given tasks.
func NewListTasksResult(tasks []Task) ListTasksResult {
	return ListTasksResult{
		Tasks: tasks,
	}
}

// NewCancelTaskResult creates a CancelTaskResult from a Task.
func NewCancelTaskResult(task Task) CancelTaskResult {
	return CancelTaskResult{
		Task: task,
	}
}

// NewTaskStatusNotification creates a notification for a task status change.
func NewTaskStatusNotification(task Task) TaskStatusNotification {
	return TaskStatusNotification{
		Method: MethodNotificationTasksStatus,
		Params: TaskStatusNotificationParams{
			Task: task,
		},
	}
}

//
// Task Capability Helper Functions
//

// NewTasksCapability creates a TasksCapability with all operations enabled.
func NewTasksCapability() *TasksCapability {
	return &TasksCapability{
		List:   &struct{}{},
		Cancel: &struct{}{},
		Requests: &TaskRequestsCapability{
			Tools: &struct {
				Call *struct{} `json:"call,omitempty"`
			}{
				Call: &struct{}{},
			},
		},
	}
}

// NewTasksCapabilityWithToolsOnly creates a TasksCapability with only tool call support.
// List and Cancel operations are not enabled with this capability.
func NewTasksCapabilityWithToolsOnly() *TasksCapability {
	return &TasksCapability{
		Requests: &TaskRequestsCapability{
			Tools: &struct {
				Call *struct{} `json:"call,omitempty"`
			}{
				Call: &struct{}{},
			},
		},
	}
}

//
// Related Task Metadata Functions
//

// ExtensionTasks is the identifier of the Tasks extension of protocol
// version 2026-07-28: the key under capabilities.extensions by which a
// client declares, on every request, that a tools/call may be answered with
// a task, and a server that it serves tasks.
const ExtensionTasks = "io.modelcontextprotocol/tasks"

// ExtensionTask is a task of the Tasks extension of protocol version
// 2026-07-28 (see ExtensionTasks), the handle a client polls with: a
// tools/call answered with resultType "task" carries these fields at the top
// level of its result, and no content. [Task] is the 2025-11-25 shape, whose
// fields are named ttl and pollInterval.
type ExtensionTask struct {
	// TaskId identifies the task.
	TaskId string `json:"taskId"`
	// Status is the task's current state.
	Status TaskStatus `json:"status"`
	// StatusMessage optionally describes the current state.
	StatusMessage string `json:"statusMessage,omitempty"`
	// CreatedAt is when the task was created, an ISO 8601 timestamp.
	CreatedAt string `json:"createdAt"`
	// LastUpdatedAt is when the task last changed, an ISO 8601 timestamp.
	LastUpdatedAt string `json:"lastUpdatedAt"`
	// TTLMs is how long, in milliseconds from creation, the task is kept;
	// nil is unlimited.
	TTLMs *int64 `json:"ttlMs"`
	// PollIntervalMs is the suggested time, in milliseconds, between polls.
	PollIntervalMs *int64 `json:"pollIntervalMs,omitempty"`
}

// RelatedTaskMetaKey is the metadata key for associating a message with a task.
const RelatedTaskMetaKey = "io.modelcontextprotocol/related-task"

// RelatedTaskMeta creates the metadata for associating a message with a task.
// The returned map contains a "taskId" field with the provided task ID.
func RelatedTaskMeta(taskID string) map[string]any {
	return map[string]any{
		"taskId": taskID,
	}
}

// WithRelatedTask returns a Meta with the related task ID set.
// This is useful for associating task results with their originating task.
func WithRelatedTask(taskID string) *Meta {
	return &Meta{
		AdditionalFields: map[string]any{
			RelatedTaskMetaKey: RelatedTaskMeta(taskID),
		},
	}
}

//
// Model Immediate Response Metadata Functions
//

// ModelImmediateResponseMetaKey is the metadata key for providing an immediate response to the model.
// Servers can use this optional key in the _meta field of CreateTaskResult to provide
// a string that should be passed as an immediate tool result to the model while the task
// continues executing asynchronously in the background.
const ModelImmediateResponseMetaKey = "io.modelcontextprotocol/model-immediate-response"

// WithModelImmediateResponse creates Meta with an immediate response message for the model.
// This allows the model to continue processing while the task executes asynchronously.
// The message parameter is a human-readable string that will be shown to the model.
//
// Example:
//
//	return &mcp.CreateTaskResult{
//	    Task: task,
//	    Result: mcp.Result{
//	        Meta: mcp.WithModelImmediateResponse("Processing your request. This may take a few minutes."),
//	    },
//	}
func WithModelImmediateResponse(message string) *Meta {
	return &Meta{
		AdditionalFields: map[string]any{
			ModelImmediateResponseMetaKey: message,
		},
	}
}
