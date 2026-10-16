package mcp

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"os"
)

var errToolSchemaConflict = errors.New("provide either InputSchema or RawInputSchema, not both")

// ListToolsRequest is sent from the client to request a list of tools the
// server has.
type ListToolsRequest struct {
	PaginatedRequest
	Header http.Header `json:"-"`
}

// ListToolsResult is the server's response to a tools/list request from the
// client.
type ListToolsResult struct {
	PaginatedResult
	Tools []Tool `json:"tools"`
}

// CallToolResult is the server's response to a tool call.
//
// Any errors that originate from the tool SHOULD be reported inside the result
// object, with `isError` set to true, _not_ as an MCP protocol-level error
// response. Otherwise, the LLM would not be able to see that an error occurred
// and self-correct.
//
// However, any errors in _finding_ the tool, an error indicating that the
// server does not support tool calls, or any other exceptional conditions,
// should be reported as an MCP error response.
type CallToolResult struct {
	Result
	MultiRoundTripResult
	Content []Content `json:"content"` // Can be TextContent, ImageContent, AudioContent, or EmbeddedResource
	// Structured content returned as a JSON object in the structuredContent field of a result.
	// For backwards compatibility, a tool that returns structured content SHOULD also return
	// functionally equivalent unstructured content.
	StructuredContent any `json:"structuredContent,omitempty"`
	// RawStructuredContent preserves the original JSON bytes for structuredContent when
	// unmarshaled from a wire message.
	RawStructuredContent json.RawMessage `json:"-"`
	// Whether the tool call ended in an error.
	//
	// If not set, this is assumed to be false (the call was successful).
	IsError bool `json:"isError,omitempty"`
	// Task is the task the call was accepted as, when ResultType is
	// [ResultTypeTask] (the Tasks extension): the result is then the task
	// and carries no content. On the wire its fields stand at the top level
	// of the result.
	Task *ExtensionTask `json:"-"`
}

// CallToolRequest is used by the client to invoke a tool provided by the server.
type CallToolRequest struct {
	Request
	Header http.Header    `json:"-"` // HTTP headers from the original request
	Params CallToolParams `json:"params"`
}

type CallToolParams struct {
	Name      string      `json:"name"`
	Arguments any         `json:"arguments,omitempty"`
	Meta      *Meta       `json:"_meta,omitempty"`
	Task      *TaskParams `json:"task,omitempty"`
	MultiRoundTripParams
	// RawArguments preserves the original JSON bytes for arguments when unmarshaled
	// from a wire message. This avoids precision loss for integers above 2^53.
	RawArguments json.RawMessage `json:"-"`
}

// UnmarshalJSON preserves the original arguments JSON while also populating Arguments.
func (p *CallToolParams) UnmarshalJSON(data []byte) error {
	type params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      *Meta           `json:"_meta,omitempty"`
		Task      *TaskParams     `json:"task,omitempty"`
		MultiRoundTripParams
	}

	var raw params
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	p.Name = raw.Name
	p.Meta = raw.Meta
	p.Task = raw.Task
	p.MultiRoundTripParams = raw.MultiRoundTripParams
	// A value decoded into twice must not keep the previous request's
	// arguments when the new one omits them.
	p.Arguments = nil
	p.RawArguments = nil

	if len(raw.Arguments) == 0 {
		return nil
	}

	// The decoder gave raw.Arguments its own copy of the bytes.
	p.RawArguments = raw.Arguments
	return json.Unmarshal(raw.Arguments, &p.Arguments)
}

// MarshalJSON re-emits preserved raw arguments when available.
func (p CallToolParams) MarshalJSON() ([]byte, error) {
	if len(p.RawArguments) > 0 {
		type params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments,omitempty"`
			Meta      *Meta           `json:"_meta,omitempty"`
			Task      *TaskParams     `json:"task,omitempty"`
			MultiRoundTripParams
		}
		return json.Marshal(params{
			Name:                 p.Name,
			Arguments:            p.RawArguments,
			Meta:                 p.Meta,
			Task:                 p.Task,
			MultiRoundTripParams: p.MultiRoundTripParams,
		})
	}

	type alias CallToolParams
	return json.Marshal(alias(p))
}

// MarshalJSON writes the wire form of a CallToolResult: content is always
// present, structured content comes from the raw bytes when they are kept,
// and the keys are written in sorted order. resultType is required from
// protocol version 2026-07-28 onward and omitted for earlier clients; the
// multi round-trip fields are present only while the server is asking the
// client for more input (SEP-2322).
func (r CallToolResult) MarshalJSON() ([]byte, error) {
	if r.Task != nil {
		// A result accepted as a task is the task, with no content.
		return json.Marshal(struct {
			Meta           *Meta      `json:"_meta,omitempty"`
			CreatedAt      string     `json:"createdAt"`
			LastUpdatedAt  string     `json:"lastUpdatedAt"`
			PollIntervalMs *int64     `json:"pollIntervalMs,omitempty"`
			ResultType     ResultType `json:"resultType"`
			Status         TaskStatus `json:"status"`
			StatusMessage  string     `json:"statusMessage,omitempty"`
			TaskId         string     `json:"taskId"`
			TTLMs          *int64     `json:"ttlMs"`
		}{r.Meta, r.Task.CreatedAt, r.Task.LastUpdatedAt, r.Task.PollIntervalMs, ResultTypeTask, r.Task.Status, r.Task.StatusMessage, r.Task.TaskId, r.Task.TTLMs})
	}
	content := r.Content
	if content == nil {
		content = []Content{}
	}
	var structured any
	if len(r.RawStructuredContent) > 0 {
		structured = r.RawStructuredContent
	} else if r.StructuredContent != nil {
		structured = r.StructuredContent
	}
	return json.Marshal(struct {
		Meta              *Meta         `json:"_meta,omitempty"`
		Content           []Content     `json:"content"`
		InputRequests     InputRequests `json:"inputRequests,omitempty"`
		IsError           bool          `json:"isError,omitempty"`
		RequestState      string        `json:"requestState,omitempty"`
		ResultType        ResultType    `json:"resultType,omitempty"`
		StructuredContent any           `json:"structuredContent,omitempty"`
	}{r.Meta, content, r.InputRequests, r.IsError, r.RequestState, r.ResultType, structured})
}

// MarshalJSONTo writes the result into enc as MarshalJSON writes it. When
// enc encodes as json.Marshal does, as the server's encoders do, the result
// is written straight in rather than encoded on its own and copied in; a
// member that cannot be encoded then fails the write with the encoder left
// within the object, as encoding/json/v2's own types leave it. json.Marshal
// and json.Encoder write nothing of a value that failed.
func (r *CallToolResult) MarshalJSONTo(enc *jsontext.Encoder) error {
	if r.Task != nil || !encodesAsMarshal(enc) {
		encoded, err := r.MarshalJSON()
		if err != nil {
			return err
		}
		return enc.WriteValue(encoded)
	}
	// The members are read before the content is written, as MarshalJSON's
	// copy of the result is taken before it writes: content that changes the
	// result while it is encoded does not change what is written.
	meta, content, inputs := r.Meta, &r.Content, r.InputRequests
	if r.Content == nil {
		content = &noContent
	}
	isError, state, resultType := r.IsError, r.RequestState, r.ResultType
	var structured any
	if len(r.RawStructuredContent) > 0 {
		structured = r.RawStructuredContent
	} else {
		structured = r.StructuredContent
	}

	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	if meta != nil {
		if err := writeMember(enc, "_meta", meta); err != nil {
			return err
		}
	}
	if err := writeMember(enc, "content", content); err != nil {
		return err
	}
	if len(inputs) > 0 {
		if err := writeMember(enc, "inputRequests", inputs); err != nil {
			return err
		}
	}
	if isError {
		if err := writeTokens(enc, "isError", jsontext.True); err != nil {
			return err
		}
	}
	if state != "" {
		if err := writeTokens(enc, "requestState", jsontext.String(state)); err != nil {
			return err
		}
	}
	if resultType != "" {
		if err := writeTokens(enc, "resultType", jsontext.String(string(resultType))); err != nil {
			return err
		}
	}
	if structured != nil {
		if err := writeMember(enc, "structuredContent", structured); err != nil {
			return err
		}
	}
	return enc.WriteToken(jsontext.EndObject)
}

// noContent is the content of a result that has none: an empty array.
var noContent = []Content{}

// writeTokens writes a member named name whose value is the token value.
func writeTokens(enc *jsontext.Encoder, name string, value jsontext.Token) error {
	if err := enc.WriteToken(jsontext.String(name)); err != nil {
		return err
	}
	return enc.WriteToken(value)
}

// writeMember writes a member named name whose value is v, by enc's options.
func writeMember(enc *jsontext.Encoder, name string, v any) error {
	if err := enc.WriteToken(jsontext.String(name)); err != nil {
		return err
	}
	return jsonv2.MarshalEncode(enc, v)
}

// encodesAsMarshal reports whether enc encodes as json.Marshal does: every
// option at v1's value, compact, with no marshalers of its own. A value
// written into such an encoder is written as json.Marshal would write it.
func encodesAsMarshal(enc *jsontext.Encoder) bool {
	opts := enc.Options()
	for _, option := range v1Options {
		if on, _ := jsonv2.GetOption(opts, option); !on {
			return false
		}
	}
	for _, option := range notV1Options {
		if on, _ := jsonv2.GetOption(opts, option); on {
			return false
		}
	}
	marshalers, _ := jsonv2.GetOption(opts, jsonv2.WithMarshalers)
	return marshalers == nil
}

var (
	// v1Options are the options that json.Marshal turns on and that bear on
	// what it writes.
	v1Options = []func(bool) jsonv2.Options{
		jsontext.AllowDuplicateNames,
		jsontext.AllowInvalidUTF8,
		jsontext.EscapeForHTML,
		jsontext.EscapeForJS,
		jsontext.PreserveRawStrings,
		jsonv2.Deterministic,
		jsonv2.FormatNilMapAsNull,
		jsonv2.FormatNilSliceAsNull,
		json.CallMethodsWithLegacySemantics,
		json.FormatByteArrayAsArray,
		json.FormatBytesWithLegacySemantics,
		json.FormatDurationAsNano,
		json.OmitEmptyWithLegacySemantics,
		json.StringifyWithLegacySemantics,
	}
	// notV1Options are the options that json.Marshal leaves off and that
	// bear on what it writes.
	notV1Options = []func(bool) jsonv2.Options{
		jsontext.Multiline,
		jsontext.SpaceAfterColon,
		jsontext.SpaceAfterComma,
		jsontext.CanonicalizeRawInts,
		jsontext.CanonicalizeRawFloats,
		jsontext.ReorderRawObjects,
		jsonv2.StringifyNumbers,
		jsonv2.OmitZeroStructFields,
	}
)

// UnmarshalJSON implements custom JSON unmarshaling for CallToolResult
func (r *CallToolResult) UnmarshalJSON(data []byte) error {
	type result struct {
		Meta              *Meta             `json:"_meta,omitempty"`
		ResultType        ResultType        `json:"resultType,omitempty"`
		Content           []json.RawMessage `json:"content"`
		StructuredContent json.RawMessage   `json:"structuredContent,omitempty"`
		IsError           bool              `json:"isError,omitempty"`
		InputRequests     InputRequests     `json:"inputRequests,omitempty"`
		RequestState      string            `json:"requestState,omitempty"`
	}

	var raw result
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	r.Meta = raw.Meta
	r.ResultType = raw.ResultType
	r.IsError = raw.IsError
	r.InputRequests = raw.InputRequests
	r.RequestState = raw.RequestState

	r.Task = nil
	if raw.ResultType == ResultTypeTask {
		var task ExtensionTask
		if err := json.Unmarshal(data, &task); err != nil {
			return err
		}
		r.Task = &task
	}

	r.Content = nil
	if raw.Content != nil {
		r.Content = make([]Content, len(raw.Content))
		for i, item := range raw.Content {
			content, err := UnmarshalContent(item)
			if err != nil {
				return err
			}
			r.Content[i] = content
		}
	}

	r.RawStructuredContent = nil
	r.StructuredContent = nil
	if len(raw.StructuredContent) > 0 {
		// The decoder gave raw.StructuredContent its own copy of the bytes.
		r.RawStructuredContent = raw.StructuredContent
		if err := json.Unmarshal(raw.StructuredContent, &r.StructuredContent); err != nil {
			return err
		}
	}

	return nil
}

// ToolListChangedNotification is an optional notification from the server to
// the client, informing it that the list of tools it offers has changed. This may
// be issued by servers without any previous subscription from the client.
type ToolListChangedNotification struct {
	Notification
}

// TaskSupport indicates how a tool supports task augmentation.
type TaskSupport string

const (
	// TaskSupportForbidden means the tool cannot be invoked as a task (default).
	TaskSupportForbidden TaskSupport = "forbidden"
	// TaskSupportOptional means the tool can be invoked as a task or normally.
	TaskSupportOptional TaskSupport = "optional"
	// TaskSupportRequired means the tool must be invoked as a task.
	TaskSupportRequired TaskSupport = "required"
)

// ToolExecution describes execution behavior for a tool.
type ToolExecution struct {
	// TaskSupport indicates whether the tool supports task augmentation.
	TaskSupport TaskSupport `json:"taskSupport,omitempty"`
}

// Tool represents the definition for a tool the client can call.
type Tool struct {
	// Meta is a metadata object that is reserved by MCP for storing additional information.
	Meta *Meta `json:"_meta,omitempty"`
	// The name of the tool.
	Name string `json:"name"`
	// Title is an optional human-readable, UI-friendly display name for the tool.
	// If not provided, clients should use Annotations.Title (if set) and fall back to Name.
	Title string `json:"title,omitempty"`
	// A human-readable description of the tool.
	Description string `json:"description,omitempty"`
	// A JSON Schema object defining the expected parameters for the tool.
	InputSchema ToolInputSchema `json:"inputSchema"`
	// Alternative to InputSchema - allows arbitrary JSON Schema to be provided
	RawInputSchema json.RawMessage `json:"-"` // Hide this from JSON marshaling
	// A JSON Schema object defining the expected output returned by the tool .
	OutputSchema ToolOutputSchema `json:"outputSchema,omitzero"`
	// Optional JSON Schema defining expected output structure
	RawOutputSchema json.RawMessage `json:"-"` // Hide this from JSON marshaling
	// Optional properties describing tool behavior
	Annotations ToolAnnotation `json:"annotations"`
	// Support for deferred loading
	DeferLoading bool `json:"defer_loading,omitempty"`
	// Icons provides visual identifiers for the tool
	Icons []Icon `json:"icons,omitempty"`
	// Execution describes execution behavior for the tool
	Execution *ToolExecution `json:"execution,omitempty"`
}

// GetName returns the name of the tool.
func (t Tool) GetName() string {
	return t.Name
}

// MarshalJSON implements the json.Marshaler interface for Tool.
// It handles marshaling either InputSchema or RawInputSchema based on which is set.
func (t Tool) MarshalJSON() ([]byte, error) {
	// Create a map to build the JSON structure
	m := make(map[string]any, 5)

	// Add the name and description
	m["name"] = t.Name
	if t.Title != "" {
		m["title"] = t.Title
	}
	if t.Description != "" {
		m["description"] = t.Description
	}

	// Determine which input schema to use
	if t.RawInputSchema != nil {
		if t.InputSchema.Type != "" || t.InputSchema.Types != nil || len(t.InputSchema.AdditionalFields) > 0 {
			return nil, fmt.Errorf("tool %s has both InputSchema and RawInputSchema set: %w", t.Name, errToolSchemaConflict)
		}
		m["inputSchema"] = t.RawInputSchema
	} else {
		// Use the structured InputSchema
		m["inputSchema"] = t.InputSchema
	}

	// Add output schema if present
	if t.RawOutputSchema != nil {
		if t.OutputSchema.Type != "" || t.OutputSchema.Types != nil || len(t.OutputSchema.AdditionalFields) > 0 {
			return nil, fmt.Errorf("tool %s has both OutputSchema and RawOutputSchema set: %w", t.Name, errToolSchemaConflict)
		}
		m["outputSchema"] = t.RawOutputSchema
	} else if t.OutputSchema.Type != "" || t.OutputSchema.Types != nil || len(t.OutputSchema.AdditionalFields) > 0 { // If no output schema is specified, do not return anything
		m["outputSchema"] = t.OutputSchema
	}

	m["annotations"] = t.Annotations

	if t.DeferLoading {
		m["defer_loading"] = t.DeferLoading
	}

	// Marshal Meta if present
	if t.Meta != nil {
		m["_meta"] = t.Meta
	}

	if t.Icons != nil {
		m["icons"] = t.Icons
	}

	if t.Execution != nil {
		m["execution"] = t.Execution
	}

	return json.Marshal(m)
}

type ToolAnnotation struct {
	// Human-readable title for the tool
	Title string `json:"title,omitempty"`
	// If true, the tool does not modify its environment
	ReadOnlyHint *bool `json:"readOnlyHint,omitempty"`
	// If true, the tool may perform destructive updates
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
	// If true, repeated calls with same args have no additional effect
	IdempotentHint *bool `json:"idempotentHint,omitempty"`
	// If true, tool interacts with external entities
	OpenWorldHint *bool `json:"openWorldHint,omitempty"`
}

// ToolOption is a function that configures a Tool.
// It provides a flexible way to set various properties of a Tool using the functional options pattern.
type ToolOption func(*Tool)

//
// Core Tool Functions
//

// NewTool creates a new Tool with the given name and options.
// The tool will have an object-type input schema with configurable properties.
// Options are applied in order, allowing for flexible tool configuration.
func NewTool(name string, opts ...ToolOption) Tool {
	tool := Tool{
		Name: name,
		InputSchema: ToolInputSchema{
			Type:       "object",
			Properties: make(map[string]any),
			Required:   nil, // Will be omitted from JSON if empty
		},
		Annotations: ToolAnnotation{
			Title:           "",
			ReadOnlyHint:    new(false),
			DestructiveHint: new(true),
			IdempotentHint:  new(false),
			OpenWorldHint:   new(true),
		},
	}

	for _, opt := range opts {
		opt(&tool)
	}

	return tool
}

// NewToolWithRawSchema creates a new Tool with the given name and a raw JSON
// Schema. This allows for arbitrary JSON Schema to be used for the tool's input
// schema.
//
// NOTE a [Tool] built in such a way is incompatible with the [ToolOption] and
// runtime errors will result from supplying a [ToolOption] to a [Tool] built
// with this function.
func NewToolWithRawSchema(name, description string, schema json.RawMessage) Tool {
	tool := Tool{
		Name:           name,
		Description:    description,
		RawInputSchema: schema,
	}

	return tool
}

// WithDescription adds a description to the Tool.
// The description should provide a clear, human-readable explanation of what the tool does.
func WithDescription(description string) ToolOption {
	return func(t *Tool) {
		t.Description = description
	}
}

// WithToolTitle sets the optional human-readable display title for the Tool.
// Per the MCP spec, clients should prefer Title over Annotations.Title and Name for display.
func WithToolTitle(title string) ToolOption {
	return func(t *Tool) {
		t.Title = title
	}
}

// WithDeferLoading sets the defer_loading flag for the tool.
// This is used to implement dynamic tool loading/searching patterns.
func WithDeferLoading(deferLoading bool) ToolOption {
	return func(t *Tool) {
		t.DeferLoading = deferLoading
	}
}

// WithInputSchema creates a ToolOption that sets the input schema for a tool.
// It accepts any Go type, usually a struct, and automatically generates a JSON schema from it.
func WithInputSchema[T any]() ToolOption {
	return func(t *Tool) {
		schema, err := schemaFor[T]()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		mcpSchema, err := json.Marshal(schema)
		if err != nil {
			return
		}

		t.InputSchema.Type = ""
		t.InputSchema.Types = nil
		t.InputSchema.AdditionalFields = nil
		t.RawInputSchema = json.RawMessage(mcpSchema)
	}
}

// WithToolIcons adds icons to the Tool.
// Icons provide visual identifiers for the tool.
func WithToolIcons(icons ...Icon) ToolOption {
	return func(t *Tool) {
		t.Icons = icons
	}
}

// WithTaskSupport sets the task support mode for the tool.
// It configures whether the tool can be invoked as a task (asynchronously).
// Valid values are TaskSupportForbidden (default), TaskSupportOptional, or TaskSupportRequired.
func WithTaskSupport(support TaskSupport) ToolOption {
	return func(t *Tool) {
		if t.Execution == nil {
			t.Execution = &ToolExecution{}
		}
		t.Execution.TaskSupport = support
	}
}

// WithRawInputSchema sets a raw JSON schema for the tool's input.
// Use this when you need full control over the schema or when working with
// complex schemas that can't be generated from Go types. The jsonschema library
// can handle complex schemas and provides nice extension points, so be sure to
// check that out before using this.
func WithRawInputSchema(schema json.RawMessage) ToolOption {
	return func(t *Tool) {
		// The raw schema replaces the typed one NewTool starts with, as
		// WithInputSchema does; both set would refuse the tool (upstream
		// #1082).
		if schema != nil {
			t.InputSchema.Type = ""
			t.InputSchema.Types = nil
			t.InputSchema.AdditionalFields = nil
		}
		t.RawInputSchema = schema
	}
}

// WithOutputSchema creates a ToolOption that sets the output schema for a tool.
// It accepts any Go type, usually a struct, and automatically generates a JSON schema from it.
func WithOutputSchema[T any]() ToolOption {
	return func(t *Tool) {
		schema, err := schemaFor[T]()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}

		mcpSchema, err := json.Marshal(schema)
		if err != nil {
			return
		}

		// Decoded into a fresh schema: decoding into the one set before
		// would merge the two (upstream #1055).
		var outputSchema ToolOutputSchema
		if err := json.Unmarshal(mcpSchema, &outputSchema); err != nil {
			return
		}
		t.OutputSchema = outputSchema

		// Always set the type to "object" as of the current MCP spec
		// https://modelcontextprotocol.io/specification/2025-06-18/server/tools#output-schema
		t.OutputSchema.Type = "object"
		// Decoding the generated schema recorded struct field order. Clear it
		// so these tools keep emitting properties sorted by name.
		t.OutputSchema.PropertyOrder = nil
	}
}

// WithRawOutputSchema sets a raw JSON schema for the tool's output.
// Use this when you need full control over the schema or when working with
// complex schemas that can't be generated from Go types. The jsonschema library
// can handle complex schemas and provides nice extension points, so be sure to
// check that out before using this.
func WithRawOutputSchema(schema json.RawMessage) ToolOption {
	return func(t *Tool) {
		t.RawOutputSchema = schema
	}
}

// WithToolAnnotation adds optional hints about the Tool.
func WithToolAnnotation(annotation ToolAnnotation) ToolOption {
	return func(t *Tool) {
		t.Annotations = annotation
	}
}

// WithTitleAnnotation sets the Title field of the Tool's Annotations.
// It provides a human-readable title for the tool.
func WithTitleAnnotation(title string) ToolOption {
	return func(t *Tool) {
		t.Annotations.Title = title
	}
}

// WithReadOnlyHintAnnotation sets the ReadOnlyHint field of the Tool's Annotations.
// If true, it indicates the tool does not modify its environment.
func WithReadOnlyHintAnnotation(value bool) ToolOption {
	return func(t *Tool) {
		t.Annotations.ReadOnlyHint = &value
	}
}

// WithDestructiveHintAnnotation sets the DestructiveHint field of the Tool's Annotations.
// If true, it indicates the tool may perform destructive updates.
func WithDestructiveHintAnnotation(value bool) ToolOption {
	return func(t *Tool) {
		t.Annotations.DestructiveHint = &value
	}
}

// WithIdempotentHintAnnotation sets the IdempotentHint field of the Tool's Annotations.
// If true, it indicates repeated calls with the same arguments have no additional effect.
func WithIdempotentHintAnnotation(value bool) ToolOption {
	return func(t *Tool) {
		t.Annotations.IdempotentHint = &value
	}
}

// WithOpenWorldHintAnnotation sets the OpenWorldHint field of the Tool's Annotations.
// If true, it indicates the tool interacts with external entities.
func WithOpenWorldHintAnnotation(value bool) ToolOption {
	return func(t *Tool) {
		t.Annotations.OpenWorldHint = &value
	}
}

// WithSchemaAdditionalProperties sets the additionalProperties field on the tool's input schema.
// It accepts false (disallow extra properties), true (allow any), or a schema map
// to validate additional properties against.
func WithSchemaAdditionalProperties(schema any) ToolOption {
	return func(t *Tool) {
		t.InputSchema.AdditionalProperties = schema
	}
}
