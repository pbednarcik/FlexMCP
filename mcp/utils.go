package mcp

import (
	"encoding/json"
	"fmt"
)

// ClientRequest types
var (
	_ ClientRequest = (*PingRequest)(nil)
	_ ClientRequest = (*InitializeRequest)(nil)
	_ ClientRequest = (*CompleteRequest)(nil)
	_ ClientRequest = (*SetLevelRequest)(nil)
	_ ClientRequest = (*GetPromptRequest)(nil)
	_ ClientRequest = (*ListPromptsRequest)(nil)
	_ ClientRequest = (*ListResourcesRequest)(nil)
	_ ClientRequest = (*ReadResourceRequest)(nil)
	_ ClientRequest = (*SubscribeRequest)(nil)
	_ ClientRequest = (*UnsubscribeRequest)(nil)
	_ ClientRequest = (*CallToolRequest)(nil)
	_ ClientRequest = (*ListToolsRequest)(nil)
)

// ClientNotification types
var (
	_ ClientNotification = (*CancelledNotification)(nil)
	_ ClientNotification = (*ProgressNotification)(nil)
	_ ClientNotification = (*InitializedNotification)(nil)
)

// ClientResult types
var (
	_ ClientResult = (*EmptyResult)(nil)
	_ ClientResult = (*CreateMessageResult)(nil)
	_ ClientResult = (*ListRootsResult)(nil)
)

// ServerRequest types
var (
	_ ServerRequest = (*PingRequest)(nil)
	_ ServerRequest = (*CreateMessageRequest)(nil)
	_ ServerRequest = (*ListRootsRequest)(nil)
)

// ServerNotification types
var (
	_ ServerNotification = (*CancelledNotification)(nil)
	_ ServerNotification = (*ProgressNotification)(nil)
	_ ServerNotification = (*LoggingMessageNotification)(nil)
	_ ServerNotification = (*ResourceUpdatedNotification)(nil)
	_ ServerNotification = (*ResourceListChangedNotification)(nil)
	_ ServerNotification = (*ToolListChangedNotification)(nil)
	_ ServerNotification = (*PromptListChangedNotification)(nil)
)

// ServerResult types
var (
	_ ServerResult = (*EmptyResult)(nil)
	_ ServerResult = (*InitializeResult)(nil)
	_ ServerResult = (*CompleteResult)(nil)
	_ ServerResult = (*GetPromptResult)(nil)
	_ ServerResult = (*ListPromptsResult)(nil)
	_ ServerResult = (*ListResourcesResult)(nil)
	_ ServerResult = (*ReadResourceResult)(nil)
	_ ServerResult = (*CallToolResult)(nil)
	_ ServerResult = (*ListToolsResult)(nil)
)

// Helper functions for type assertions

// asType attempts to cast the given interface to the given type
func asType[T any](content any) (*T, bool) {
	tc, ok := content.(T)
	if !ok {
		return nil, false
	}
	return &tc, true
}

// AsTextContent attempts to cast the given interface to TextContent
func AsTextContent(content any) (*TextContent, bool) {
	return asType[TextContent](content)
}

// AsImageContent attempts to cast the given interface to ImageContent
func AsImageContent(content any) (*ImageContent, bool) {
	return asType[ImageContent](content)
}

// AsAudioContent attempts to cast the given interface to AudioContent
func AsAudioContent(content any) (*AudioContent, bool) {
	return asType[AudioContent](content)
}

// AsEmbeddedResource attempts to cast the given interface to EmbeddedResource
func AsEmbeddedResource(content any) (*EmbeddedResource, bool) {
	return asType[EmbeddedResource](content)
}

// AsToolUseContent attempts to cast the given interface to ToolUseContent
func AsToolUseContent(content any) (*ToolUseContent, bool) {
	return asType[ToolUseContent](content)
}

// AsToolResultContent attempts to cast the given interface to ToolResultContent
func AsToolResultContent(content any) (*ToolResultContent, bool) {
	return asType[ToolResultContent](content)
}

// AsTextResourceContents attempts to cast the given interface to TextResourceContents
func AsTextResourceContents(content any) (*TextResourceContents, bool) {
	return asType[TextResourceContents](content)
}

// AsBlobResourceContents attempts to cast the given interface to BlobResourceContents
func AsBlobResourceContents(content any) (*BlobResourceContents, bool) {
	return asType[BlobResourceContents](content)
}

// Helper function for JSON-RPC

// NewJSONRPCResponse creates a new JSONRPCResponse with the given id and result.
// NOTE: This function expects a Result struct, but JSONRPCResponse.Result is typed as `any`.
// The Result struct wraps the actual result data with optional metadata.
// For direct result assignment, use NewJSONRPCResultResponse instead.
func NewJSONRPCResponse(id RequestId, result Result) JSONRPCResponse {
	return JSONRPCResponse{
		JSONRPC: JSONRPC_VERSION,
		ID:      id,
		Result:  result,
	}
}

// NewJSONRPCResultResponse creates a new JSONRPCResponse with the given id and result.
// This function accepts any type for the result, matching the JSONRPCResponse.Result field type.
func NewJSONRPCResultResponse(id RequestId, result any) JSONRPCResponse {
	return JSONRPCResponse{
		JSONRPC: JSONRPC_VERSION,
		ID:      id,
		Result:  result,
	}
}

// NewJSONRPCErrorDetails creates a new JSONRPCErrorDetails with the given code, message, and data.
func NewJSONRPCErrorDetails(code int, message string, data any) JSONRPCErrorDetails {
	return JSONRPCErrorDetails{
		Code:    code,
		Message: message,
		Data:    data,
	}
}

// NewJSONRPCError creates a new JSONRPCResponse with the given id, code, and message
func NewJSONRPCError(
	id RequestId,
	code int,
	message string,
	data any,
) JSONRPCError {
	return JSONRPCError{
		JSONRPC: JSONRPC_VERSION,
		ID:      id,
		Error:   NewJSONRPCErrorDetails(code, message, data),
	}
}

// NewProgressNotification creates a progress notification.
func NewProgressNotification(
	token ProgressToken,
	progress float64,
	total *float64,
	message *string,
) ProgressNotification {
	notification := ProgressNotification{
		Method: string(MethodNotificationProgress),
		Params: struct {
			ProgressToken ProgressToken `json:"progressToken"`
			Progress      float64       `json:"progress"`
			Total         float64       `json:"total,omitempty"`
			Message       string        `json:"message,omitempty"`
		}{
			ProgressToken: token,
			Progress:      progress,
		},
	}
	if total != nil {
		notification.Params.Total = *total
	}
	if message != nil {
		notification.Params.Message = *message
	}
	return notification
}

// NewLoggingMessageNotification creates a logging message notification.
func NewLoggingMessageNotification(
	level LoggingLevel,
	logger string,
	data any,
) LoggingMessageNotification {
	return LoggingMessageNotification{
		Method: string(MethodNotificationMessage),
		Params: struct {
			Level  LoggingLevel `json:"level"`
			Logger string       `json:"logger,omitempty"`
			Data   any          `json:"data"`
		}{
			Level:  level,
			Logger: logger,
			Data:   data,
		},
	}
}

// NewPromptMessage creates a prompt message.
func NewPromptMessage(role Role, content Content) PromptMessage {
	return PromptMessage{
		Role:    role,
		Content: content,
	}
}

// NewTextContent creates text content.
func NewTextContent(text string) TextContent {
	return TextContent{
		Type: ContentTypeText,
		Text: text,
	}
}

// NewImageContent creates image content.
func NewImageContent(data, mimeType string) ImageContent {
	return ImageContent{
		Type:     ContentTypeImage,
		Data:     data,
		MIMEType: mimeType,
	}
}

// NewAudioContent creates audio content.
func NewAudioContent(data, mimeType string) AudioContent {
	return AudioContent{
		Type:     ContentTypeAudio,
		Data:     data,
		MIMEType: mimeType,
	}
}

// NewResourceLink creates a resource link.
func NewResourceLink(uri, name, description, mimeType string) ResourceLink {
	return ResourceLink{
		Type:        ContentTypeLink,
		URI:         uri,
		Name:        name,
		Description: description,
		MIMEType:    mimeType,
	}
}

// NewEmbeddedResource creates an embedded resource.
func NewEmbeddedResource(resource ResourceContents) EmbeddedResource {
	return EmbeddedResource{
		Type:     ContentTypeResource,
		Resource: resource,
	}
}

// NewToolUseContent creates a new ToolUseContent with the given id, tool name, and input arguments.
func NewToolUseContent(id, name string, input any) ToolUseContent {
	return ToolUseContent{
		Type:  ContentTypeToolUse,
		ID:    id,
		Name:  name,
		Input: input,
	}
}

// NewToolResultContent creates a new ToolResultContent with the given tool use ID, content, and error flag.
func NewToolResultContent(toolUseID string, content []Content, isError bool) ToolResultContent {
	return ToolResultContent{
		Type:      ContentTypeToolResult,
		ToolUseID: toolUseID,
		Content:   content,
		IsError:   isError,
	}
}

// NewToolResultText creates a new CallToolResult with a text content
func NewToolResultText(text string) *CallToolResult {
	return &CallToolResult{
		Content: []Content{
			TextContent{
				Type: ContentTypeText,
				Text: text,
			},
		},
	}
}

// NewToolResultJSON creates a new CallToolResult with a JSON content.
func NewToolResultJSON[T any](data T) (*CallToolResult, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("unable to marshal JSON: %w", err)
	}

	return &CallToolResult{
		Content: []Content{
			TextContent{
				Type: ContentTypeText,
				Text: string(b),
			},
		},
		StructuredContent: data,
	}, nil
}

// NewToolResultStructured creates a new CallToolResult with structured content.
// It includes both the structured content and a text representation for backward compatibility.
func NewToolResultStructured(structured any, fallbackText string) *CallToolResult {
	return &CallToolResult{
		Content: []Content{
			TextContent{
				Type: "text",
				Text: fallbackText,
			},
		},
		StructuredContent: structured,
	}
}

// NewToolResultStructuredOnly creates a new CallToolResult with structured
// content and creates a JSON string fallback for backwards compatibility.
// This is useful when you want to provide structured data without any specific text fallback.
func NewToolResultStructuredOnly(structured any) *CallToolResult {
	// Convert to JSON string for backward compatibility. Content that cannot
	// be encoded would make the whole result unencodable, and the client
	// would get no answer: it is a tool error instead (upstream #1069).
	jsonBytes, err := json.Marshal(structured)
	if err != nil {
		return NewToolResultError(fmt.Sprintf("Error serializing structured content: %v", err))
	}

	return &CallToolResult{
		Content: []Content{
			TextContent{
				Type: "text",
				Text: string(jsonBytes),
			},
		},
		StructuredContent: structured,
	}
}

// NewToolResultImage creates a new CallToolResult with both text and image content
func NewToolResultImage(text, imageData, mimeType string) *CallToolResult {
	return &CallToolResult{
		Content: []Content{
			TextContent{
				Type: ContentTypeText,
				Text: text,
			},
			ImageContent{
				Type:     ContentTypeImage,
				Data:     imageData,
				MIMEType: mimeType,
			},
		},
	}
}

// NewToolResultAudio creates a new CallToolResult with both text and audio content
func NewToolResultAudio(text, audioData, mimeType string) *CallToolResult {
	return &CallToolResult{
		Content: []Content{
			TextContent{
				Type: ContentTypeText,
				Text: text,
			},
			AudioContent{
				Type:     ContentTypeAudio,
				Data:     audioData,
				MIMEType: mimeType,
			},
		},
	}
}

// NewToolResultResource creates a new CallToolResult with an embedded resource
func NewToolResultResource(
	text string,
	resource ResourceContents,
) *CallToolResult {
	return &CallToolResult{
		Content: []Content{
			TextContent{
				Type: ContentTypeText,
				Text: text,
			},
			EmbeddedResource{
				Type:     ContentTypeResource,
				Resource: resource,
			},
		},
	}
}

// NewToolResultError creates a new CallToolResult with an error message.
// Any errors that originate from the tool SHOULD be reported inside the result object.
func NewToolResultError(text string) *CallToolResult {
	return &CallToolResult{
		Content: []Content{
			TextContent{
				Type: ContentTypeText,
				Text: text,
			},
		},
		IsError: true,
	}
}

// NewToolResultErrorFromErr creates a new CallToolResult with an error message.
// If an error is provided, its details will be appended to the text message.
// Any errors that originate from the tool SHOULD be reported inside the result object.
func NewToolResultErrorFromErr(text string, err error) *CallToolResult {
	if err != nil {
		text = fmt.Sprintf("%s: %v", text, err)
	}
	return &CallToolResult{
		Content: []Content{
			TextContent{
				Type: ContentTypeText,
				Text: text,
			},
		},
		IsError: true,
	}
}

// NewToolResultErrorf creates a new CallToolResult with an error message.
// The error message is formatted using the fmt package.
// Any errors that originate from the tool SHOULD be reported inside the result object.
func NewToolResultErrorf(format string, a ...any) *CallToolResult {
	return &CallToolResult{
		Content: []Content{
			TextContent{
				Type: ContentTypeText,
				Text: fmt.Sprintf(format, a...),
			},
		},
		IsError: true,
	}
}

// optionalCursor is the cursor a list result carries: none for the last
// page, which the helpers below take as an empty nextCursor.
func optionalCursor(c Cursor) *Cursor {
	if c == "" {
		return nil
	}
	return &c
}

// NewListResourcesResult creates a new ListResourcesResult; an empty
// nextCursor marks the last page.
func NewListResourcesResult(
	resources []Resource,
	nextCursor Cursor,
) *ListResourcesResult {
	return &ListResourcesResult{
		NextCursor: optionalCursor(nextCursor),
		Resources:  resources,
	}
}

// NewListResourceTemplatesResult creates a new ListResourceTemplatesResult;
// an empty nextCursor marks the last page.
func NewListResourceTemplatesResult(
	templates []ResourceTemplate,
	nextCursor Cursor,
) *ListResourceTemplatesResult {
	return &ListResourceTemplatesResult{
		NextCursor:        optionalCursor(nextCursor),
		ResourceTemplates: templates,
	}
}

// NewReadResourceResult creates a new ReadResourceResult with text content
func NewReadResourceResult(text string) *ReadResourceResult {
	return &ReadResourceResult{
		Contents: []ResourceContents{
			TextResourceContents{
				Text: text,
			},
		},
	}
}

// NewListPromptsResult creates a new ListPromptsResult; an empty nextCursor
// marks the last page.
func NewListPromptsResult(
	prompts []Prompt,
	nextCursor Cursor,
) *ListPromptsResult {
	return &ListPromptsResult{
		NextCursor: optionalCursor(nextCursor),
		Prompts:    prompts,
	}
}

// NewGetPromptResult creates a new GetPromptResult
func NewGetPromptResult(
	description string,
	messages []PromptMessage,
) *GetPromptResult {
	return &GetPromptResult{
		Description: description,
		Messages:    messages,
	}
}

// NewListToolsResult creates a new ListToolsResult; an empty nextCursor
// marks the last page.
func NewListToolsResult(tools []Tool, nextCursor Cursor) *ListToolsResult {
	return &ListToolsResult{
		NextCursor: optionalCursor(nextCursor),
		Tools:      tools,
	}
}

// NewInitializeResult creates a new InitializeResult
func NewInitializeResult(
	protocolVersion string,
	capabilities ServerCapabilities,
	serverInfo Implementation,
	instructions string,
) *InitializeResult {
	return &InitializeResult{
		ProtocolVersion: protocolVersion,
		Capabilities:    capabilities,
		ServerInfo:      serverInfo,
		Instructions:    instructions,
	}
}

// FormatNumberResult formats a number as a tool result.
func FormatNumberResult(value float64) *CallToolResult {
	return NewToolResultText(fmt.Sprintf("%.2f", value))
}

// ToBoolPtr returns a pointer to the given boolean value
//
//go:fix inline
func ToBoolPtr(b bool) *bool {
	return new(b)
}

// ToInt64Ptr returns a pointer to the given int64 value
//
//go:fix inline
func ToInt64Ptr(i int64) *int64 {
	return new(i)
}
