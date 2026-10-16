package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
)

type Annotations struct {
	// Describes who the intended customer of this object or data is.
	//
	// It can include multiple entries to indicate content useful for multiple
	// audiences (e.g., `["user", "assistant"]`).
	Audience []Role `json:"audience,omitempty"`

	// Describes how important this data is for operating the server.
	//
	// A value of 1 means "most important," and indicates that the data is
	// effectively required, while 0 means "least important," and indicates that
	// the data is entirely optional.
	// Priority ranges from 0.0 to 1.0 (1 = most important, 0 = least important).
	Priority *float64 `json:"priority,omitempty"`
	// ISO 8601 formatted timestamp (e.g., "2025-01-12T15:00:58Z")
	LastModified string `json:"lastModified,omitempty"`
}

// Annotated is the base for objects that include optional annotations for the
// client. The client can use annotations to inform how objects are used or
// displayed
type Annotated struct {
	Annotations *Annotations `json:"annotations,omitempty"`
}

type Content interface {
	isContent()
}

// TextContent represents text provided to or from an LLM.
// It must have Type set to "text".
type TextContent struct {
	Annotated
	// Meta is a metadata object that is reserved by MCP for storing additional information.
	Meta *Meta  `json:"_meta,omitempty"`
	Type string `json:"type"` // Must be "text"
	// The text content of the message.
	Text string `json:"text"`
}

func (TextContent) isContent() {}

// ImageContent represents an image provided to or from an LLM.
// It must have Type set to "image".
type ImageContent struct {
	Annotated
	// Meta is a metadata object that is reserved by MCP for storing additional information.
	Meta *Meta  `json:"_meta,omitempty"`
	Type string `json:"type"` // Must be "image"
	// The base64-encoded image data.
	Data string `json:"data"`
	// The MIME type of the image. Different providers may support different image types.
	MIMEType string `json:"mimeType"`
}

func (ImageContent) isContent() {}

// AudioContent represents the contents of audio, embedded into a prompt or tool call result.
// It must have Type set to "audio".
type AudioContent struct {
	Annotated
	// Meta is a metadata object that is reserved by MCP for storing additional information.
	Meta *Meta  `json:"_meta,omitempty"`
	Type string `json:"type"` // Must be "audio"
	// The base64-encoded audio data.
	Data string `json:"data"`
	// The MIME type of the audio. Different providers may support different audio types.
	MIMEType string `json:"mimeType"`
}

func (AudioContent) isContent() {}

// ResourceLink represents a link to a resource that the client can access.
type ResourceLink struct {
	Annotated
	// Meta is a metadata object that is reserved by MCP for storing additional information.
	Meta *Meta  `json:"_meta,omitempty"`
	Type string `json:"type"` // Must be "resource_link"
	// The URI of the resource.
	URI string `json:"uri"`
	// The name of the resource.
	Name string `json:"name"`
	// Title is an optional human-readable, UI-friendly display name for this resource.
	// If not provided, clients should fall back to Name.
	Title string `json:"title,omitempty"`
	// The description of the resource.
	Description string `json:"description"`
	// The MIME type of the resource.
	MIMEType string `json:"mimeType"`
	// Size is the size of the raw resource content, in bytes (i.e., before base64
	// encoding or any tokenization), if known. This can be used by hosts to
	// display file sizes and estimate context window usage.
	//
	// A pointer is used so that an explicit zero size remains distinguishable
	// from an unset value.
	Size *int64 `json:"size,omitempty"`
}

func (ResourceLink) isContent() {}

// EmbeddedResource represents the contents of a resource, embedded into a prompt or tool call result.
//
// It is up to the client how best to render embedded resources for the
// benefit of the LLM and/or the user.
type EmbeddedResource struct {
	Annotated
	// Meta is a metadata object that is reserved by MCP for storing additional information.
	Meta     *Meta            `json:"_meta,omitempty"`
	Type     string           `json:"type"`
	Resource ResourceContents `json:"resource"`
}

func (EmbeddedResource) isContent() {}

// UnmarshalJSON implements custom JSON unmarshaling for EmbeddedResource
// to handle the nested ResourceContents interface.
func (e *EmbeddedResource) UnmarshalJSON(data []byte) error {
	var raw struct {
		Annotated
		Meta     *Meta           `json:"_meta,omitempty"`
		Type     string          `json:"type"`
		Resource json.RawMessage `json:"resource"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	var resourceFields map[string]json.RawMessage
	if err := json.Unmarshal(raw.Resource, &resourceFields); err != nil {
		return fmt.Errorf("unmarshaling embedded resource: %w", err)
	}

	var resource ResourceContents
	if _, ok := resourceFields["text"]; ok {
		var textResource TextResourceContents
		if err := json.Unmarshal(raw.Resource, &textResource); err != nil {
			return fmt.Errorf("unmarshaling embedded text resource: %w", err)
		}
		if textResource.URI == "" {
			return fmt.Errorf("unmarshaling embedded text resource: %w", ErrEmbeddedResourceMissingURI)
		}
		resource = textResource
	} else if _, ok := resourceFields["blob"]; ok {
		var blobResource BlobResourceContents
		if err := json.Unmarshal(raw.Resource, &blobResource); err != nil {
			return fmt.Errorf("unmarshaling embedded blob resource: %w", err)
		}
		if blobResource.URI == "" {
			return fmt.Errorf("unmarshaling embedded blob resource: %w", ErrEmbeddedResourceMissingURI)
		}
		resource = blobResource
	} else {
		return fmt.Errorf("unmarshaling embedded resource: %w", ErrEmbeddedResourceMissingVariant)
	}

	e.Annotated = raw.Annotated
	e.Meta = raw.Meta
	e.Type = raw.Type
	e.Resource = resource
	return nil
}

// ToolUseContent represents a request from the assistant to call a tool within a sampling message.
// It must have Type set to "tool_use".
type ToolUseContent struct {
	Annotated
	// Meta is a metadata object that is reserved by MCP for storing additional information.
	Meta *Meta  `json:"_meta,omitempty"`
	Type string `json:"type"` // Must be "tool_use"
	// ID is a unique identifier for this tool use, used to match tool results to their corresponding tool uses.
	ID string `json:"id"`
	// Name is the name of the tool to call.
	Name string `json:"name"`
	// Input contains the arguments to pass to the tool, conforming to the tool's input schema.
	Input any `json:"input"`
}

func (ToolUseContent) isContent() {}

// ToolResultContent represents the result of a tool invocation within a sampling message.
// It must have Type set to "tool_result".
type ToolResultContent struct {
	Annotated
	// Meta is a metadata object that is reserved by MCP for storing additional information.
	Meta *Meta  `json:"_meta,omitempty"`
	Type string `json:"type"` // Must be "tool_result"
	// ToolUseID is the ID of the tool use this result corresponds to.
	// This MUST match the ID from a previous ToolUseContent.
	ToolUseID string `json:"toolUseId"`
	// Content is the unstructured result content of the tool use.
	Content []Content `json:"content"`
	// StructuredContent is an optional structured result of the tool use.
	// If the tool defined an outputSchema, this SHOULD conform to that schema.
	StructuredContent any `json:"structuredContent,omitempty"`
	// RawStructuredContent preserves the original JSON bytes for structuredContent when
	// unmarshaled from a wire message.
	RawStructuredContent json.RawMessage `json:"-"`
	// Whether the tool use resulted in an error.
	IsError bool `json:"isError,omitempty"`
}

func (ToolResultContent) isContent() {}

// MarshalJSON implements custom JSON marshaling for ToolResultContent.
// structuredContent is written from RawStructuredContent when it is set.
func (t ToolResultContent) MarshalJSON() ([]byte, error) {
	type alias ToolResultContent
	out := struct {
		alias
		StructuredContent any `json:"structuredContent,omitempty"`
	}{alias: alias(t), StructuredContent: t.StructuredContent}
	if len(t.RawStructuredContent) > 0 {
		out.StructuredContent = t.RawStructuredContent
	}
	return json.Marshal(out)
}

// toolResultContentJSON is a helper type for unmarshaling ToolResultContent.
type toolResultContentJSON struct {
	Annotated
	Meta              *Meta             `json:"_meta,omitempty"`
	Type              string            `json:"type"`
	ToolUseID         string            `json:"toolUseId"`
	Content           []json.RawMessage `json:"content"`
	StructuredContent json.RawMessage   `json:"structuredContent,omitempty"`
	IsError           bool              `json:"isError,omitempty"`
}

// UnmarshalJSON implements custom JSON unmarshaling for ToolResultContent
// to handle the nested Content interface slice.
func (t *ToolResultContent) UnmarshalJSON(data []byte) error {
	var raw toolResultContentJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	t.Annotated = raw.Annotated
	t.Meta = raw.Meta
	t.Type = raw.Type
	t.ToolUseID = raw.ToolUseID
	t.IsError = raw.IsError

	t.RawStructuredContent = nil
	t.StructuredContent = nil
	if len(raw.StructuredContent) > 0 {
		// The decoder gave raw.StructuredContent its own copy of the bytes.
		t.RawStructuredContent = raw.StructuredContent
		if err := json.Unmarshal(raw.StructuredContent, &t.StructuredContent); err != nil {
			return fmt.Errorf("unmarshaling tool result structured content: %w", err)
		}
	}

	t.Content = nil
	if raw.Content != nil {
		t.Content = make([]Content, 0, len(raw.Content))
		for _, rawContent := range raw.Content {
			c, err := UnmarshalContent(rawContent)
			if err != nil {
				return fmt.Errorf("unmarshaling tool result content: %w", err)
			}
			t.Content = append(t.Content, c)
		}
	}
	return nil
}

// MarshalContent marshals MCP content as JSON.
func MarshalContent(content Content) ([]byte, error) {
	return json.Marshal(content)
}

// UnmarshalContent implements custom JSON unmarshaling for Content interface
func UnmarshalContent(data []byte) (Content, error) {
	var raw struct {
		Type any `json:"type"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}

	contentType, ok := raw.Type.(string)
	if !ok {
		return nil, errors.New("missing or invalid type field")
	}

	switch contentType {
	case ContentTypeText:
		var content TextContent
		err := json.Unmarshal(data, &content)
		return content, err
	case ContentTypeImage:
		var content ImageContent
		err := json.Unmarshal(data, &content)
		return content, err
	case ContentTypeAudio:
		var content AudioContent
		err := json.Unmarshal(data, &content)
		return content, err
	case ContentTypeLink:
		var content ResourceLink
		err := json.Unmarshal(data, &content)
		return content, err
	case ContentTypeResource:
		var content EmbeddedResource
		err := json.Unmarshal(data, &content)
		return content, err
	case ContentTypeToolUse:
		var content ToolUseContent
		err := json.Unmarshal(data, &content)
		return content, err
	case ContentTypeToolResult:
		var content ToolResultContent
		err := json.Unmarshal(data, &content)
		return content, err
	default:
		return nil, fmt.Errorf("unknown content type: %s", contentType)
	}
}
