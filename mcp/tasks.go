package mcp

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"net/http"
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

// WithModelImmediateResponse creates Meta carrying [ModelImmediateResponseMetaKey]
// with message, a human-readable string meant for the model.
//
// Deprecated: the server builds the answer to a task-augmented tools/call
// itself and ignores the CreateTaskResult a handler returns, so this meta
// never reaches that answer; set on a handler's result it only appears in
// tasks/result, after the task ends. The 2025-11-25 tasks it belongs to are
// superseded by the 2026-07-28 Tasks extension.
func WithModelImmediateResponse(message string) *Meta {
	return &Meta{
		AdditionalFields: map[string]any{
			ModelImmediateResponseMetaKey: message,
		},
	}
}

/* Tasks */

// TasksCapability represents the task capabilities that a client or server may support.
// Tasks enable long-running, asynchronous operations with status polling.
type TasksCapability struct {
	// Whether the party supports the tasks/list operation.
	List *struct{} `json:"list,omitempty"`
	// Whether the party supports the tasks/cancel operation.
	Cancel *struct{} `json:"cancel,omitempty"`
	// Requests that can be augmented with task metadata.
	Requests *TaskRequestsCapability `json:"requests,omitempty"`
}

// TaskRequestsCapability indicates which request types support task augmentation.
type TaskRequestsCapability struct {
	// Tool-related capabilities.
	Tools *struct {
		// Whether tools/call can be augmented with task metadata.
		Call *struct{} `json:"call,omitempty"`
	} `json:"tools,omitempty"`
	// Sampling-related capabilities.
	Sampling *struct {
		// Whether sampling/createMessage can be augmented with task metadata.
		CreateMessage *struct{} `json:"createMessage,omitempty"`
	} `json:"sampling,omitempty"`
	// Elicitation-related capabilities.
	Elicitation *struct {
		// Whether elicitation/create can be augmented with task metadata.
		Create *struct{} `json:"create,omitempty"`
	} `json:"elicitation,omitempty"`
}

// TaskStatus represents the execution state of a task.
type TaskStatus string

const (
	// TaskStatusWorking indicates the request is currently being processed.
	TaskStatusWorking TaskStatus = "working"
	// TaskStatusInputRequired indicates the receiver needs input from the requestor.
	// NOTE: This status is defined by the spec but not yet implemented in this SDK.
	// The input_required flow requires integration with elicitation which is planned
	// for a future release.
	TaskStatusInputRequired TaskStatus = "input_required"
	// TaskStatusCompleted indicates the request completed successfully.
	TaskStatusCompleted TaskStatus = "completed"
	// TaskStatusFailed indicates the request did not complete successfully.
	TaskStatusFailed TaskStatus = "failed"
	// TaskStatusCancelled indicates the request was cancelled before completion.
	TaskStatusCancelled TaskStatus = "cancelled"
)

// IsTerminal returns true if the task status is terminal (completed, failed, or cancelled).
func (s TaskStatus) IsTerminal() bool {
	return s == TaskStatusCompleted || s == TaskStatusFailed || s == TaskStatusCancelled
}

// Task represents the execution state of a request.
type Task struct {
	// Unique identifier for the task.
	TaskId string `json:"taskId"`
	// Current state of the task execution.
	Status TaskStatus `json:"status"`
	// Optional human-readable message describing the current state.
	StatusMessage string `json:"statusMessage,omitempty"`
	// ISO 8601 timestamp when the task was created.
	CreatedAt string `json:"createdAt"`
	// ISO 8601 timestamp when the task was last updated.
	LastUpdatedAt string `json:"lastUpdatedAt"`
	// Time in milliseconds from creation before task may be deleted.
	// If null, the task has no expiration.
	TTL *int64 `json:"ttl"`
	// Suggested time in milliseconds between status checks.
	PollInterval *int64 `json:"pollInterval,omitempty"`
}

// GetName returns the task ID, implementing the Named interface for pagination.
func (t Task) GetName() string {
	return t.TaskId
}

// TaskParams represents the task metadata included when augmenting a request.
type TaskParams struct {
	// Requested duration in milliseconds to retain task from creation.
	TTL *int64 `json:"ttl,omitempty"`
}

// CreateTaskResult is returned immediately when a task-augmented request is accepted.
// It contains task metadata rather than the actual operation result.
type CreateTaskResult struct {
	Result
	Task              Task      `json:"task"`
	Content           []Content `json:"-"`
	StructuredContent any       `json:"-"`
	IsError           bool      `json:"-"`
}

// GetTaskRequest retrieves the current status of a task.
type GetTaskRequest struct {
	Request
	Header http.Header   `json:"-"`
	Params GetTaskParams `json:"params"`
}

type GetTaskParams struct {
	TaskId string `json:"taskId"`
}

// GetTaskResult returns the current state of a task.
type GetTaskResult struct {
	Result
	Task
}

// ListTasksRequest retrieves a paginated list of tasks.
type ListTasksRequest struct {
	PaginatedRequest
	Header http.Header `json:"-"`
}

// ListTasksResult returns a list of tasks.
type ListTasksResult struct {
	PaginatedResult
	Tasks []Task `json:"tasks"`
}

// TaskResultRequest retrieves the result of a completed task.
type TaskResultRequest struct {
	Request
	Header http.Header      `json:"-"`
	Params TaskResultParams `json:"params"`
}

type TaskResultParams struct {
	TaskId string `json:"taskId"`
}

// TaskResultResult contains the actual operation result.
// For task-augmented tool calls, this embeds the CallToolResult fields.
type TaskResultResult struct {
	Result
	// Tool call result fields (for task-augmented tool calls)
	Content           []Content `json:"content,omitempty"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	// RawStructuredContent is the structured content as JSON. When set,
	// MarshalJSON writes it in place of StructuredContent, so numbers that a
	// float64 cannot hold exactly pass through a parsed result unchanged.
	RawStructuredContent json.RawMessage `json:"-"`
	IsError              bool            `json:"isError,omitempty"`
}

// MarshalJSON implements custom JSON marshaling for TaskResultResult.
// structuredContent is written from RawStructuredContent when it is set.
func (r TaskResultResult) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.wire())
}

// MarshalJSONTo writes the result into enc by the encoder's own rules, as a
// TaskResultResult without methods would be written, with structuredContent
// from RawStructuredContent when it is set.
func (r TaskResultResult) MarshalJSONTo(enc *jsontext.Encoder) error {
	return jsonv2.MarshalEncode(enc, r.wire())
}

// taskResultFields is TaskResultResult without its methods.
type taskResultFields TaskResultResult

// taskResultWire is a TaskResultResult as it is written: its fields, with
// structuredContent taken from the raw bytes when there are any.
type taskResultWire struct {
	taskResultFields
	StructuredContent any `json:"structuredContent,omitempty"`
}

func (r TaskResultResult) wire() *taskResultWire {
	out := &taskResultWire{taskResultFields: taskResultFields(r), StructuredContent: r.StructuredContent}
	if len(r.RawStructuredContent) > 0 {
		out.StructuredContent = r.RawStructuredContent
	}
	return out
}

// CancelTaskRequest cancels an in-progress task.
type CancelTaskRequest struct {
	Request
	Header http.Header      `json:"-"`
	Params CancelTaskParams `json:"params"`
}

type CancelTaskParams struct {
	TaskId string `json:"taskId"`
}

// CancelTaskResult returns the cancelled task state.
type CancelTaskResult struct {
	Result
	Task
}

// ExtensionTaskRequest is the request of tasks/get and tasks/cancel in the
// Tasks extension of protocol version 2026-07-28.
type ExtensionTaskRequest struct {
	Request
	Header http.Header         `json:"-"`
	Params ExtensionTaskParams `json:"params"`
}

// ExtensionTaskParams are the params of an [ExtensionTaskRequest].
type ExtensionTaskParams struct {
	// TaskId names the task.
	TaskId string `json:"taskId"`
}

// UpdateExtensionTaskRequest is the tasks/update request of the Tasks
// extension: the client's answers to the input requests of a task.
type UpdateExtensionTaskRequest struct {
	Request
	Header http.Header               `json:"-"`
	Params UpdateExtensionTaskParams `json:"params"`
}

// UpdateExtensionTaskParams are the params of an [UpdateExtensionTaskRequest].
type UpdateExtensionTaskParams struct {
	// TaskId names the task.
	TaskId string `json:"taskId"`
	// InputResponses answers the task's input requests, by their keys.
	InputResponses InputResponses `json:"inputResponses"`
}

// ExtensionTaskStateResult is the answer to tasks/get in the Tasks extension:
// the task, and as its state requires the outcome of the call (TaskResult,
// when completed), the error it failed with (Error, when failed) or the input
// it waits for (InputRequests, when input_required).
type ExtensionTaskStateResult struct {
	Result
	ExtensionTask
	// TaskResult is the tool call's result, for a completed task.
	TaskResult *CallToolResult `json:"result,omitempty"`
	// Error is why the call failed, for a failed task.
	Error *JSONRPCErrorDetails `json:"error,omitempty"`
	// InputRequests are what an input_required task waits for.
	InputRequests InputRequests `json:"inputRequests,omitempty"`
}

// TaskStatusNotification is sent when a task's status changes.
type TaskStatusNotification struct {
	Notification
	Params TaskStatusNotificationParams `json:"params"`
}

type TaskStatusNotificationParams struct {
	Task
}
