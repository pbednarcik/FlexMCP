package mcp

/* Sampling */

const (
	// MethodSamplingCreateMessage allows servers to request LLM completions from clients
	MethodSamplingCreateMessage MCPMethod = "sampling/createMessage"
)

// CreateMessageRequest is a request from the server to sample an LLM via the
// client. The client has full discretion over which model to select. The client
// should also inform the user before beginning sampling, to allow them to inspect
// the request (human in the loop) and decide whether to approve it.
type CreateMessageRequest struct {
	Request
	CreateMessageParams `json:"params"`
}

type CreateMessageParams struct {
	Messages         []SamplingMessage `json:"messages"`
	ModelPreferences *ModelPreferences `json:"modelPreferences,omitempty"`
	SystemPrompt     string            `json:"systemPrompt,omitempty"`
	IncludeContext   string            `json:"includeContext,omitempty"`
	Temperature      float64           `json:"temperature,omitempty"`
	MaxTokens        int               `json:"maxTokens"`
	StopSequences    []string          `json:"stopSequences,omitempty"`
	Metadata         any               `json:"metadata,omitempty"`
	// Tools the model may use during generation.
	//
	// Per the 2025-11-25 spec, the client MUST return an error if this field
	// is provided but ClientCapabilities.Sampling.Tools is not declared.
	Tools []Tool `json:"tools,omitempty"`
	// ToolChoice controls how the model uses tools during generation.
	//
	// Per the 2025-11-25 spec, the client MUST return an error if this field
	// is provided but ClientCapabilities.Sampling.Tools is not declared.
	// When omitted the client defaults to {Mode: ToolChoiceModeAuto}.
	ToolChoice *ToolChoice `json:"toolChoice,omitempty"`
}

// ToolChoiceMode controls tool selection behaviour during sampling.
type ToolChoiceMode string

const (
	// ToolChoiceModeAuto lets the model decide whether to use tools. This is
	// the default when ToolChoice is omitted.
	ToolChoiceModeAuto ToolChoiceMode = "auto"
	// ToolChoiceModeRequired forces the model to call at least one tool.
	ToolChoiceModeRequired ToolChoiceMode = "required"
	// ToolChoiceModeNone disables tool use for this sampling request.
	ToolChoiceModeNone ToolChoiceMode = "none"
)

// ToolChoice controls tool selection behaviour for a sampling request as
// defined by the 2025-11-25 protocol revision.
type ToolChoice struct {
	// Mode controls tool selection. Empty is treated as ToolChoiceModeAuto by
	// the client.
	Mode ToolChoiceMode `json:"mode,omitempty"`
}

// CreateMessageResult is the client's response to a sampling/create_message
// request from the server. The client should inform the user before returning the
// sampled message, to allow them to inspect the response (human in the loop) and
// decide whether to allow the server to see it.
type CreateMessageResult struct {
	Result
	SamplingMessage
	// The name of the model that generated the message.
	Model string `json:"model"`
	// The reason why sampling stopped, if known.
	StopReason string `json:"stopReason,omitempty"`
}

// SamplingMessage describes a message issued to or received from an LLM API.
type SamplingMessage struct {
	Role    Role `json:"role"`
	Content any  `json:"content"` // Can be TextContent, ImageContent, AudioContent, ToolUseContent or ToolResultContent, or a []Content of them
}

// ModelPreferences represents the server's preferences for model selection,
// requested of the client during sampling.
//
// Because LLMs can vary along multiple dimensions, choosing the "best" modelis
// rarely straightforward.  Different models excel in different areas—some are
// faster but less capable, others are more capable but more expensive, and so
// on. This interface allows servers to express their priorities across multiple
// dimensions to help clients make an appropriate selection for their use case.
//
// These preferences are always advisory. The client MAY ignore them. It is also
// up to the client to decide how to interpret these preferences and how to
// balance them against other considerations.
type ModelPreferences struct {
	// Optional hints to use for model selection.
	//
	// If multiple hints are specified, the client MUST evaluate them in order
	// (such that the first match is taken).
	//
	// The client SHOULD prioritize these hints over the numeric priorities, but
	// MAY still use the priorities to select from ambiguous matches.
	Hints []ModelHint `json:"hints,omitempty"`

	// How much to prioritize cost when selecting a model. A value of 0 means cost
	// is not important, while a value of 1 means cost is the most important
	// factor.
	CostPriority float64 `json:"costPriority,omitempty"`

	// How much to prioritize sampling speed (latency) when selecting a model. A
	// value of 0 means speed is not important, while a value of 1 means speed is
	// the most important factor.
	SpeedPriority float64 `json:"speedPriority,omitempty"`

	// How much to prioritize intelligence and capabilities when selecting a
	// model. A value of 0 means intelligence is not important, while a value of 1
	// means intelligence is the most important factor.
	IntelligencePriority float64 `json:"intelligencePriority,omitempty"`
}

// ModelHint represents hints to use for model selection.
//
// Keys not declared here are currently left unspecified by the spec and are up
// to the client to interpret.
type ModelHint struct {
	// A hint for a model name.
	//
	// The client SHOULD treat this as a substring of a model name; for example:
	//  - `claude-3-5-sonnet` should match `claude-3-5-sonnet-20241022`
	//  - `sonnet` should match `claude-3-5-sonnet-20241022`, `claude-3-sonnet-20240229`, etc.
	//  - `claude` should match any Claude model
	//
	// The client MAY also map the string to a different provider's model name or
	// a different model family, as long as it fills a similar niche; for example:
	//  - `gemini-1.5-flash` could match `claude-3-haiku-20240307`
	Name string `json:"name,omitempty"`
}
