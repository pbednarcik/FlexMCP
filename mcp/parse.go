package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
)

func ExtractString(data map[string]any, key string) string {
	if value, ok := data[key]; ok {
		if str, ok := value.(string); ok {
			return str
		}
	}
	return ""
}

// ParseAnnotations parses priority, audience, and lastModified fields from the provided map
// and returns an Annotations struct populated with any valid values found.
// If data is nil, ParseAnnotations returns nil. Priority is set when a numeric value can be
// parsed and is stored as a *float64. Audience is populated from string values and includes
// only RoleUser and RoleAssistant entries. LastModified is set when the value is a string.
func ParseAnnotations(data map[string]any) *Annotations {
	if data == nil {
		return nil
	}
	annotations := &Annotations{}
	if value, ok := data["priority"]; ok {
		if value != nil {
			if priority, err := toFloat64E(value); err == nil {
				annotations.Priority = &priority
			}
		}
	}

	if value, ok := data["audience"]; ok {
		for _, a := range toStringSlice(value) {
			a := Role(a)
			if a == RoleUser || a == RoleAssistant {
				annotations.Audience = append(annotations.Audience, a)
			}
		}
	}

	if value, ok := data["lastModified"]; ok {
		if str, ok := value.(string); ok {
			annotations.LastModified = str
		}
	}
	return annotations

}

func ExtractMap(data map[string]any, key string) map[string]any {
	if value, ok := data[key]; ok {
		if m, ok := value.(map[string]any); ok {
			return m
		}
	}
	return nil
}

// ParseContent parses a generic map into a strongly-typed Content value.
// It extracts annotations and _meta fields from the map and sets them on
// the returned content type. The map is taken over: its _meta map becomes
// the content's metadata as it is, so a caller that goes on using the map
// should pass a copy.
func ParseContent(contentMap map[string]any) (Content, error) {
	contentType := ExtractString(contentMap, "type")

	var annotations *Annotations
	if annotationsMap := ExtractMap(contentMap, "annotations"); annotationsMap != nil {
		annotations = ParseAnnotations(annotationsMap)
	}

	var meta *Meta
	if metaMap := ExtractMap(contentMap, "_meta"); metaMap != nil {
		meta = metaFromOwnedMap(metaMap)
	}

	switch contentType {
	case ContentTypeText:
		text := ExtractString(contentMap, "text")
		c := NewTextContent(text)
		c.Annotations = annotations
		c.Meta = meta
		return c, nil

	case ContentTypeImage:
		data := ExtractString(contentMap, "data")
		mimeType := ExtractString(contentMap, "mimeType")
		if data == "" || mimeType == "" {
			return nil, errors.New("image data or mimeType is missing")
		}
		c := NewImageContent(data, mimeType)
		c.Annotations = annotations
		c.Meta = meta
		return c, nil

	case ContentTypeAudio:
		data := ExtractString(contentMap, "data")
		mimeType := ExtractString(contentMap, "mimeType")
		if data == "" || mimeType == "" {
			return nil, errors.New("audio data or mimeType is missing")
		}
		c := NewAudioContent(data, mimeType)
		c.Annotations = annotations
		c.Meta = meta
		return c, nil

	case ContentTypeLink:
		uri := ExtractString(contentMap, "uri")
		name := ExtractString(contentMap, "name")
		description := ExtractString(contentMap, "description")
		mimeType := ExtractString(contentMap, "mimeType")
		if uri == "" || name == "" {
			return nil, errors.New("resource_link uri or name is missing")
		}
		c := NewResourceLink(uri, name, description, mimeType)
		c.Title = ExtractString(contentMap, "title")
		if value, ok := contentMap["size"]; ok && value != nil {
			if size, err := toInt64E(value); err == nil && size >= 0 {
				c.Size = &size
			}
		}
		c.Annotations = annotations
		c.Meta = meta
		return c, nil

	case ContentTypeResource:
		resourceMap := ExtractMap(contentMap, "resource")
		if resourceMap == nil {
			return nil, errors.New("resource is missing")
		}

		resourceContents, err := ParseResourceContents(resourceMap)
		if err != nil {
			return nil, err
		}

		c := NewEmbeddedResource(resourceContents)
		c.Annotations = annotations
		c.Meta = meta
		return c, nil

	case ContentTypeToolUse:
		id := ExtractString(contentMap, "id")
		if id == "" {
			return nil, errors.New("tool_use id is missing")
		}
		name := ExtractString(contentMap, "name")
		if name == "" {
			return nil, errors.New("tool_use name is missing")
		}
		input := contentMap["input"]
		c := NewToolUseContent(id, name, input)
		c.Annotations = annotations
		c.Meta = meta
		return c, nil

	case ContentTypeToolResult:
		toolUseID := ExtractString(contentMap, "toolUseId")
		if toolUseID == "" {
			return nil, errors.New("tool_result toolUseId is missing")
		}
		isError, _ := contentMap["isError"].(bool)
		var contentItems []Content
		if rawContent, ok := contentMap["content"].([]any); ok {
			for i, item := range rawContent {
				itemMap, ok := item.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("tool_result content[%d]: expected object, got %T", i, item)
				}
				parsed, err := ParseContent(itemMap)
				if err != nil {
					return nil, fmt.Errorf("parsing tool result content[%d]: %w", i, err)
				}
				contentItems = append(contentItems, parsed)
			}
		}
		c := NewToolResultContent(toolUseID, contentItems, isError)
		c.StructuredContent = contentMap["structuredContent"]
		c.Annotations = annotations
		c.Meta = meta
		return c, nil
	}

	return nil, fmt.Errorf("unsupported content type: %s", contentType)
}

// ParseSamplingContent parses the content of a SamplingMessage that was
// decoded into generic JSON values. A single content block is returned as a
// Content and an array of blocks as a []Content. Any other value, such as
// content that is already typed, is returned unchanged.
func ParseSamplingContent(content any) (any, error) {
	switch c := content.(type) {
	case map[string]any:
		return ParseContent(c)
	case []any:
		blocks := make([]Content, 0, len(c))
		for i, item := range c {
			itemMap, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("content[%d]: expected object, got %T", i, item)
			}
			block, err := ParseContent(itemMap)
			if err != nil {
				return nil, fmt.Errorf("content[%d]: %w", i, err)
			}
			blocks = append(blocks, block)
		}
		return blocks, nil
	default:
		return content, nil
	}
}

// resultEnvelope holds the fields a result carries outside its payload: the
// SEP-2322 round-trip fields and the SEP-2549 cache hints.
type resultEnvelope struct {
	ResultType    ResultType    `json:"resultType"`
	InputRequests InputRequests `json:"inputRequests"`
	RequestState  string        `json:"requestState"`
	TTLMs         *int64        `json:"ttlMs"`
	CacheScope    CacheScope    `json:"cacheScope"`
}

// parseResultEnvelope decodes the envelope of a result whose payload holds
// interface values and therefore has to be walked by hand.
func parseResultEnvelope(rawMessage json.RawMessage) (resultEnvelope, error) {
	var envelope resultEnvelope
	if err := json.Unmarshal(rawMessage, &envelope); err != nil {
		return resultEnvelope{}, fmt.Errorf("failed to unmarshal response: %w", err)
	}
	return envelope, nil
}

func ParseGetPromptResult(rawMessage *json.RawMessage) (*GetPromptResult, error) {
	if rawMessage == nil {
		return nil, errors.New("response is nil")
	}

	var jsonContent map[string]any
	if err := json.Unmarshal(*rawMessage, &jsonContent); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	envelope, err := parseResultEnvelope(*rawMessage)
	if err != nil {
		return nil, err
	}

	result := GetPromptResult{
		ResultType:    envelope.ResultType,
		InputRequests: envelope.InputRequests,
		RequestState:  envelope.RequestState,
	}

	meta, ok := jsonContent["_meta"]
	if ok {
		if metaMap, ok := meta.(map[string]any); ok {
			result.Meta = metaFromOwnedMap(metaMap)
		}
	}

	description, ok := jsonContent["description"]
	if ok {
		if descriptionStr, ok := description.(string); ok {
			result.Description = descriptionStr
		}
	}

	// A null payload is treated like an absent one: an input_required result
	// carries no messages, and so does a zero-valued GetPromptResult.
	messages, ok := jsonContent["messages"]
	if ok && messages != nil {
		messagesArr, ok := messages.([]any)
		if !ok {
			return nil, errors.New("messages is not an array")
		}

		// An explicit empty array stays an empty array (upstream #1060).
		result.Messages = make([]PromptMessage, 0, len(messagesArr))
		for _, message := range messagesArr {
			messageMap, ok := message.(map[string]any)
			if !ok {
				return nil, errors.New("message is not an object")
			}

			// Extract role
			roleStr := ExtractString(messageMap, "role")
			if roleStr == "" || (roleStr != string(RoleAssistant) && roleStr != string(RoleUser)) {
				return nil, fmt.Errorf("unsupported role: %s", roleStr)
			}

			// Extract content
			contentMap, ok := messageMap["content"].(map[string]any)
			if !ok {
				return nil, errors.New("content is not an object")
			}

			// Process content
			content, err := ParseContent(contentMap)
			if err != nil {
				return nil, err
			}

			// Append processed message
			result.Messages = append(result.Messages, NewPromptMessage(Role(roleStr), content))

		}
	}

	return &result, nil
}

func ParseCallToolResult(rawMessage *json.RawMessage) (*CallToolResult, error) {
	if rawMessage == nil {
		return nil, errors.New("response is nil")
	}

	var probe struct {
		Content    json.RawMessage `json:"content"`
		ResultType ResultType      `json:"resultType"`
	}
	if err := json.Unmarshal(*rawMessage, &probe); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}
	// Only a complete result carries content: one asking for more input
	// (SEP-2322) holds the input requests and the request state, and one
	// accepted as a task (the Tasks extension) holds the task.
	if probe.Content == nil && probe.ResultType.IsComplete() {
		return nil, errors.New("content is missing")
	}

	var result CallToolResult
	if err := json.Unmarshal(*rawMessage, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}
	// A task the client cannot name is no handle to poll with.
	if result.Task != nil && result.Task.TaskId == "" {
		return nil, errors.New("task result without a taskId")
	}

	return &result, nil
}

func ParseResourceContents(contentMap map[string]any) (ResourceContents, error) {
	uri := ExtractString(contentMap, "uri")
	if uri == "" {
		return nil, errors.New("resource uri is missing")
	}

	mimeType := ExtractString(contentMap, "mimeType")

	meta := ExtractMap(contentMap, "_meta")

	if _, present := contentMap["_meta"]; present && meta == nil {
		return nil, errors.New("_meta must be an object")
	}

	// Select the variant on the presence of a string "text" or "blob" field,
	// not on its emptiness. An empty resource is still a resource, and both
	// fields marshal unconditionally, so treating "" as absent rejected
	// payloads this library itself produces.
	if text, ok := contentMap["text"].(string); ok {
		return TextResourceContents{
			Meta:     meta,
			URI:      uri,
			MIMEType: mimeType,
			Text:     text,
		}, nil
	}

	if blob, ok := contentMap["blob"].(string); ok {
		return BlobResourceContents{
			Meta:     meta,
			URI:      uri,
			MIMEType: mimeType,
			Blob:     blob,
		}, nil
	}

	return nil, errors.New("unsupported resource type")
}

func ParseReadResourceResult(rawMessage *json.RawMessage) (*ReadResourceResult, error) {
	if rawMessage == nil {
		return nil, errors.New("response is nil")
	}

	var jsonContent map[string]any
	if err := json.Unmarshal(*rawMessage, &jsonContent); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	envelope, err := parseResultEnvelope(*rawMessage)
	if err != nil {
		return nil, err
	}

	var result ReadResourceResult
	result.ResultType = envelope.ResultType
	result.InputRequests = envelope.InputRequests
	result.RequestState = envelope.RequestState
	result.TTLMs = envelope.TTLMs
	result.CacheScope = envelope.CacheScope

	meta, ok := jsonContent["_meta"]
	if ok {
		if metaMap, ok := meta.(map[string]any); ok {
			result.Meta = metaFromOwnedMap(metaMap)
		}
	}

	contents, ok := jsonContent["contents"]
	if !ok || contents == nil {
		// An input_required result asks for more input instead of returning
		// contents, so the payload is legitimately absent.
		if result.NeedsInput() {
			return &result, nil
		}
		return nil, errors.New("contents is missing")
	}

	contentArr, ok := contents.([]any)
	if !ok {
		return nil, errors.New("contents is not an array")
	}

	// An explicit empty array stays an empty array (upstream #1060).
	result.Contents = make([]ResourceContents, 0, len(contentArr))
	for _, content := range contentArr {
		// Extract content
		contentMap, ok := content.(map[string]any)
		if !ok {
			return nil, errors.New("content is not an object")
		}

		// Process content
		content, err := ParseResourceContents(contentMap)
		if err != nil {
			return nil, err
		}

		result.Contents = append(result.Contents, content)
	}

	return &result, nil
}

// GetTextFromContent extracts text from a Content interface that might be a TextContent struct
// or a map[string]any that was unmarshaled from JSON. This is useful when dealing with content
// that comes from different transport layers that may handle JSON differently.
//
// This function uses fallback behavior for non-text content - it returns a string representation
// via fmt.Sprintf for any content that cannot be extracted as text. This is a lossy operation
// intended for convenience in logging and display scenarios.
//
// For strict type validation, use ParseContent() instead, which returns an error for invalid content.
func GetTextFromContent(content any) string {
	switch c := content.(type) {
	case TextContent:
		return c.Text
	case map[string]any:
		// Handle JSON unmarshaled content
		if contentType, exists := c["type"]; exists && contentType == "text" {
			if text, exists := c["text"].(string); exists {
				return text
			}
		}
		return fmt.Sprintf("%v", content)
	case string:
		return c
	default:
		return fmt.Sprintf("%v", content)
	}
}

// ParseCancelTaskResult parses a JSON message and converts it to a
// CancelTaskResult. The task parsers decode into the typed result
// (upstream #1071): resultType and the integer fields arrive as sent, and
// a field of the wrong type fails the parse rather than vanish.
func ParseCancelTaskResult(rawMessage *json.RawMessage) (*CancelTaskResult, error) {
	if rawMessage == nil {
		return nil, errors.New("response is nil")
	}
	var result CancelTaskResult
	if err := json.Unmarshal(*rawMessage, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}
	return &result, nil
}

// ParseListTasksResult parses a JSON message and converts it to a
// ListTasksResult; see ParseCancelTaskResult.
func ParseListTasksResult(rawMessage *json.RawMessage) (*ListTasksResult, error) {
	if rawMessage == nil {
		return nil, errors.New("response is nil")
	}
	var result ListTasksResult
	if err := json.Unmarshal(*rawMessage, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}
	return &result, nil
}

// ParseTaskResultResult parses a JSON message and converts it to a TaskResultResult.
func ParseTaskResultResult(rawMessage *json.RawMessage) (*TaskResultResult, error) {
	if rawMessage == nil {
		return nil, errors.New("response is nil")
	}

	// Content holds interface values, so it is decoded element by element -
	// the same shape CallToolResult.UnmarshalJSON handles.
	var raw struct {
		Meta              *Meta             `json:"_meta"`
		ResultType        ResultType        `json:"resultType"`
		Content           []json.RawMessage `json:"content"`
		StructuredContent json.RawMessage   `json:"structuredContent"`
		IsError           bool              `json:"isError"`
	}
	if err := json.Unmarshal(*rawMessage, &raw); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	resultResult := TaskResultResult{
		Meta:       raw.Meta,
		ResultType: raw.ResultType,
		IsError:    raw.IsError,
	}

	// The structured content keeps its bytes so a re-encoded result writes
	// its numbers as received; StructuredContent holds the decoded value.
	if len(raw.StructuredContent) > 0 {
		resultResult.RawStructuredContent = raw.StructuredContent
		if err := json.Unmarshal(raw.StructuredContent, &resultResult.StructuredContent); err != nil {
			return nil, fmt.Errorf("unmarshal task structured content: %w", err)
		}
	}

	for _, item := range raw.Content {
		content, err := UnmarshalContent(item)
		if err != nil {
			return nil, fmt.Errorf("unmarshal task result content: %w", err)
		}
		resultResult.Content = append(resultResult.Content, content)
	}

	return &resultResult, nil
}

// ParseGetTaskResult parses a JSON message and converts it to a
// GetTaskResult; see ParseCancelTaskResult.
func ParseGetTaskResult(rawMessage *json.RawMessage) (*GetTaskResult, error) {
	if rawMessage == nil {
		return nil, errors.New("response is nil")
	}
	var result GetTaskResult
	if err := json.Unmarshal(*rawMessage, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}
	return &result, nil
}
