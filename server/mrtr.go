package server

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
)

// Multi round-trip requests (SEP-2322) are how a server asks a client for
// information mid-request from protocol version 2026-07-28 onward.
//
// A handler that needs a confirmation, a missing parameter, or an LLM
// completion returns a result whose ResultType is
// [mcp.ResultTypeInputRequired], carrying the requests it needs answered and
// an opaque RequestState. The client fulfils them and retries the original
// request with the answers in InputResponses and the state echoed back.
//
// A client on an earlier protocol version cannot answer such a result, and
// the server issues no server-initiated requests to ask it instead, so the
// request fails with [ErrInputRequiresModernClient].

// InputRequestBuilder accumulates the requests a handler needs answered before
// it can complete, and renders them as an [mcp.InputRequiredResult].
//
// The zero value is ready to use.
type InputRequestBuilder struct {
	requests mcp.InputRequests
	state    string
}

// NewInputRequestBuilder returns a builder for the input a handler needs.
// requestState is an opaque token the client echoes back on retry; use it to
// resume where the handler left off. It may be empty.
func NewInputRequestBuilder(requestState string) *InputRequestBuilder {
	return &InputRequestBuilder{state: requestState}
}

// Elicit asks the client to collect information from the user, keyed by id.
func (b *InputRequestBuilder) Elicit(id string, params mcp.ElicitationParams) *InputRequestBuilder {
	b.add(id, mcp.NewElicitationInputRequest(params))
	return b
}

// Sample asks the client to run an LLM completion, keyed by id.
func (b *InputRequestBuilder) Sample(id string, params mcp.CreateMessageParams) *InputRequestBuilder {
	b.add(id, mcp.NewSamplingInputRequest(params))
	return b
}

// Roots asks the client for its list of roots, keyed by id.
func (b *InputRequestBuilder) Roots(id string) *InputRequestBuilder {
	b.add(id, mcp.NewRootsInputRequest())
	return b
}

// RequestState sets the opaque token the client echoes back on retry.
func (b *InputRequestBuilder) RequestState(state string) *InputRequestBuilder {
	b.state = state
	return b
}

func (b *InputRequestBuilder) add(id string, request mcp.InputRequest) {
	if b.requests == nil {
		b.requests = make(mcp.InputRequests)
	}
	b.requests[id] = request
}

// ToolResult renders the accumulated requests as a tools/call result asking
// the client for more input.
func (b *InputRequestBuilder) ToolResult() *mcp.CallToolResult {
	return &mcp.CallToolResult{
		ResultType:           mcp.ResultTypeInputRequired,
		MultiRoundTripResult: b.multiRoundTripResult(),
	}
}

// PromptResult renders the accumulated requests as a prompts/get result asking
// the client for more input.
func (b *InputRequestBuilder) PromptResult() *mcp.GetPromptResult {
	return &mcp.GetPromptResult{
		ResultType:           mcp.ResultTypeInputRequired,
		MultiRoundTripResult: b.multiRoundTripResult(),
	}
}

// ResourceResult renders the accumulated requests as a resources/read result
// asking the client for more input.
//
// Resource handlers registered with AddResource return contents rather than a
// full result, so this is for servers that construct resources/read responses
// directly.
func (b *InputRequestBuilder) ResourceResult() *mcp.ReadResourceResult {
	result := &mcp.ReadResourceResult{MultiRoundTripResult: b.multiRoundTripResult()}
	result.ResultType = mcp.ResultTypeInputRequired
	return result
}

func (b *InputRequestBuilder) multiRoundTripResult() mcp.MultiRoundTripResult {
	return mcp.MultiRoundTripResult{
		InputRequests: b.requests,
		RequestState:  b.state,
	}
}

// InputResponse returns the client's answer to the input request recorded
// under id, and whether it was present.
//
// Use it at the top of a handler to detect a retry:
//
//	if answer, ok := server.InputResponse(request.Params.InputResponses, "confirm"); ok {
//	    // the user answered; finish the work
//	}
func InputResponse(responses mcp.InputResponses, id string) (mcp.InputResponse, bool) {
	response, ok := responses[id]
	return response, ok
}

// ElicitationResponse returns the elicitation result the client supplied under
// id, or nil when absent.
func ElicitationResponse(responses mcp.InputResponses, id string) *mcp.ElicitationResult {
	response, ok := responses[id]
	if !ok {
		return nil
	}
	if response.Elicitation == nil {
		if err := response.DecodeFor(mcp.MethodElicitationCreate); err != nil {
			return nil
		}
	}
	return response.Elicitation
}

// SamplingResponse returns the sampling result the client supplied under id,
// or nil when absent.
func SamplingResponse(responses mcp.InputResponses, id string) *mcp.CreateMessageResult {
	response, ok := responses[id]
	if !ok {
		return nil
	}
	if response.Sampling == nil {
		if err := response.DecodeFor(mcp.MethodSamplingCreateMessage); err != nil {
			return nil
		}
	}
	return response.Sampling
}

// RootsResponse returns the roots list the client supplied under id, or nil
// when absent.
func RootsResponse(responses mcp.InputResponses, id string) *mcp.ListRootsResult {
	response, ok := responses[id]
	if !ok {
		return nil
	}
	if response.Roots == nil {
		if err := response.DecodeFor(mcp.MethodListRoots); err != nil {
			return nil
		}
	}
	return response.Roots
}

// clientSupportsMultiRoundTrip reports whether the peer can handle an
// input_required result, which requires protocol version 2026-07-28 or later.
func clientSupportsMultiRoundTrip(ctx context.Context) bool {
	return mcp.IsModernProtocol(RequestProtocolVersion(ctx))
}

// resolveMultiRoundTrip passes an input_required result through to a client
// that understands the multi round-trip pattern, and refuses it for one that
// predates protocol version 2026-07-28: with ErrLoadShedding when the handler
// asked nothing and only wants a retry, ErrInputRequiresModernClient otherwise.
func resolveMultiRoundTrip[T any](
	ctx context.Context,
	result *T,
	needsInput func(*T) (mcp.InputRequests, string, bool),
) (*T, error) {
	if clientSupportsMultiRoundTrip(ctx) {
		return result, nil
	}
	requests, _, pending := needsInput(result)
	if !pending {
		return result, nil
	}
	if len(requests) == 0 {
		return nil, ErrLoadShedding
	}
	return nil, ErrInputRequiresModernClient
}

// callToolResultNeedsInput reports whether a tools/call result asks the client
// for more input.
func callToolResultNeedsInput(result *mcp.CallToolResult) (mcp.InputRequests, string, bool) {
	if !result.NeedsInput() {
		return nil, "", false
	}
	return result.InputRequests, result.RequestState, true
}

// getPromptResultNeedsInput reports whether a prompts/get result asks the
// client for more input.
func getPromptResultNeedsInput(result *mcp.GetPromptResult) (mcp.InputRequests, string, bool) {
	if !result.NeedsInput() {
		return nil, "", false
	}
	return result.InputRequests, result.RequestState, true
}
