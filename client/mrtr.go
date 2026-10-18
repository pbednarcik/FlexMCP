package client

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// Multi round-trip requests (SEP-2322) replace the server-initiated
// elicitation/create, sampling/createMessage, and roots/list requests that
// previously required a held-open bidirectional stream.
//
// When a server needs something from the user mid-call it returns a result
// whose resultType is "input_required", carrying the requests it needs
// answered and an opaque requestState. The client fulfils the requests with
// the handlers it already has registered, then retries the original call with
// the answers attached.
//
// The retry loop is transparent: callers of CallTool, GetPrompt, and
// ReadResource see only the final result. Set [WithMaxInputRoundTrips] to
// bound it.

const (
	// defaultMaxInputRoundTrips bounds how many requests one call sends, the
	// first included, while the server keeps asking for input.
	defaultMaxInputRoundTrips = 10

	// maxLoadSheddingRoundTrips bounds the consecutive requests sent while
	// the server asks for no input at all. An empty inputRequests map is a
	// load-shedding signal - "retry later" - not a request for information.
	maxLoadSheddingRoundTrips = 3
)

// ErrMaxInputRoundTripsExceeded is returned when a server keeps asking for
// more input past the configured limit.
var ErrMaxInputRoundTripsExceeded = errors.New("multi round-trip: exceeded maximum retries")

// maxRoundTrips returns the configured retry bound.
func (c *Client) maxRoundTrips() int {
	if c.maxInputRoundTrips > 0 {
		return c.maxInputRoundTrips
	}
	return defaultMaxInputRoundTrips
}

// multiRoundTrip drives the SEP-2322 retry loop for a single request.
//
// initial carries the multi round-trip params the caller supplied, so a
// caller resuming an exchange by hand keeps its request state and answers on
// the first attempt. send issues the request with the given multi round-trip
// params and returns the decoded result. The loop terminates as soon as a
// result comes back that does not ask for more input.
func multiRoundTrip[T any](
	ctx context.Context,
	c *Client,
	initial mcp.MultiRoundTripParams,
	send func(context.Context, mcp.MultiRoundTripParams) (*T, error),
	needsInput func(*T) (mcp.InputRequests, string, bool),
) (*T, error) {
	params := initial
	loadShedding := 0

	for attempt := 1; ; attempt++ {
		result, err := send(ctx, params)
		if err != nil {
			return nil, err
		}

		requests, state, pending := needsInput(result)
		if !pending {
			return result, nil
		}

		// An empty request map means the server is shedding load rather than
		// asking for information.
		if len(requests) == 0 {
			loadShedding++
			if loadShedding >= maxLoadSheddingRoundTrips {
				return nil, fmt.Errorf(
					"%w: server asked to retry %d times without requesting input",
					ErrMaxInputRoundTripsExceeded, loadShedding)
			}
		} else {
			loadShedding = 0
		}

		if attempt >= c.maxRoundTrips() {
			return nil, fmt.Errorf("%w (%d)", ErrMaxInputRoundTripsExceeded, c.maxRoundTrips())
		}

		// A load-shedding retry asks for nothing new, so the answers already
		// gathered go out again; only a fresh set of requests replaces them.
		if len(requests) > 0 {
			responses, err := c.fulfillInputRequests(ctx, requests)
			if err != nil {
				return nil, err
			}
			params.InputResponses = responses
		}
		params.RequestState = state
	}
}

// fulfillInputRequests answers a server's input requests using the handlers
// registered on the client, one at a time in key order: the user is asked
// one thing at a time however many a server asks, a handler need not be safe
// to call concurrently, and a server's request count is bounded by the time
// the user gives it, not by goroutines. The first failure ends the call.
func (c *Client) fulfillInputRequests(
	ctx context.Context,
	requests mcp.InputRequests,
) (mcp.InputResponses, error) {
	if len(requests) == 0 {
		return nil, nil
	}

	responses := make(mcp.InputResponses, len(requests))
	for _, id := range slices.Sorted(maps.Keys(requests)) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		response, err := c.fulfillInputRequestSafely(ctx, requests[id])
		if err != nil {
			return nil, fmt.Errorf("multi round-trip: fulfilling input request %q: %w", id, err)
		}
		responses[id] = response
	}
	return responses, nil
}

// fulfillInputRequestSafely runs a handler as the transports run one: a
// panic is that request's error, not the process's end.
func (c *Client) fulfillInputRequestSafely(ctx context.Context, request mcp.InputRequest) (response mcp.InputResponse, err error) {
	defer func() {
		if r := recover(); r != nil {
			response, err = mcp.InputResponse{}, fmt.Errorf("handler panic: %v", r)
		}
	}()
	return c.fulfillInputRequest(ctx, request)
}

// fulfillInputRequest dispatches a single input request to the matching
// handler.
func (c *Client) fulfillInputRequest(
	ctx context.Context,
	request mcp.InputRequest,
) (mcp.InputResponse, error) {
	switch request.Method {
	case mcp.MethodElicitationCreate:
		if c.elicitationHandler == nil {
			return mcp.InputResponse{}, errors.New("no elicitation handler configured")
		}
		if request.Elicitation == nil {
			return mcp.InputResponse{}, errors.New("elicitation input request has no params")
		}
		if err := c.checkElicitation(*request.Elicitation); err != nil {
			return mcp.InputResponse{}, err
		}
		result, err := c.elicitationHandler.Elicit(ctx, mcp.ElicitationRequest{
			Method: string(mcp.MethodElicitationCreate),
			Params: *request.Elicitation,
		})
		if err != nil {
			// The handler could not get an answer: the user did not give one,
			// which is a cancel, unless the call itself was ended.
			if ctx.Err() != nil {
				return mcp.InputResponse{}, err
			}
			return mcp.NewElicitationInputResponse(mcp.ElicitationResult{Action: mcp.ElicitationResponseActionCancel}), nil
		}
		if result == nil {
			return mcp.InputResponse{}, errors.New("elicitation handler returned no result")
		}
		return mcp.NewElicitationInputResponse(*result), nil

	case mcp.MethodSamplingCreateMessage:
		if c.samplingHandler == nil {
			return mcp.InputResponse{}, errors.New("no sampling handler configured")
		}
		if request.Sampling == nil {
			return mcp.InputResponse{}, errors.New("sampling input request has no params")
		}
		params, err := typedSamplingParams(*request.Sampling)
		if err != nil {
			return mcp.InputResponse{}, err
		}
		result, err := c.samplingHandler.CreateMessage(ctx, mcp.CreateMessageRequest{
			Method:              string(mcp.MethodSamplingCreateMessage),
			CreateMessageParams: params,
		})
		if err != nil {
			return mcp.InputResponse{}, err
		}
		if result == nil {
			return mcp.InputResponse{}, errors.New("sampling handler returned no result")
		}
		return mcp.NewSamplingInputResponse(*result), nil

	case mcp.MethodListRoots:
		if c.rootsHandler == nil {
			return mcp.InputResponse{}, errors.New("no roots handler configured")
		}
		result, err := c.rootsHandler.ListRoots(ctx, mcp.ListRootsRequest{
			Method: string(mcp.MethodListRoots),
		})
		if err != nil {
			return mcp.InputResponse{}, err
		}
		if result == nil {
			return mcp.InputResponse{}, errors.New("roots handler returned no result")
		}
		result, err = checkRoots(result)
		if err != nil {
			return mcp.InputResponse{}, err
		}
		return mcp.NewRootsInputResponse(*result), nil

	default:
		return mcp.InputResponse{}, fmt.Errorf("unsupported input request method %q", request.Method)
	}
}

// checkElicitation refuses an elicitation that is not valid, or asks for a
// mode this client did not declare (ClientFeatures.md rows 1-2, 4, 7): the
// handler sees only what the client said it can answer. Form mode, which an
// elicitation without a mode is in, is declared by a form sub-object or by
// an elicitation object with neither sub-object.
func (c *Client) checkElicitation(params mcp.ElicitationParams) error {
	if err := params.Validate(); err != nil {
		return fmt.Errorf("invalid elicitation params: %w", err)
	}
	declared := c.effectiveCapabilities(c.currentEra()).Elicitation
	if params.Mode == mcp.ElicitationModeURL {
		if declared == nil || declared.URL == nil {
			return errors.New("elicitation in url mode was not declared by this client")
		}
		return nil
	}
	if declared == nil || (declared.Form == nil && declared.URL != nil) {
		return errors.New("elicitation in form mode was not declared by this client")
	}
	return nil
}

// typedSamplingParams returns params with every message's content typed, as
// the handshake-era path hands it to the handler: a transport decodes a
// content block as a map.
func typedSamplingParams(params mcp.CreateMessageParams) (mcp.CreateMessageParams, error) {
	messages := make([]mcp.SamplingMessage, len(params.Messages))
	for i, message := range params.Messages {
		content, err := mcp.ParseSamplingContent(message.Content)
		if err != nil {
			return params, fmt.Errorf("failed to parse content for message %d: %w", i, err)
		}
		message.Content = content
		messages[i] = message
	}
	params.Messages = messages
	return params, nil
}

// callToolOnce issues a single tools/call request.
//
// A header mismatch about an Mcp-Param-* header means the tool's definition
// may have changed since it was listed: the cached definition is dropped,
// the tools are listed again, and the call is retried once with headers
// mirrored from the new definition.
func (c *Client) callToolOnce(
	ctx context.Context,
	request mcp.CallToolRequest,
	roundTrip mcp.MultiRoundTripParams,
	header http.Header,
) (*mcp.CallToolResult, error) {
	params := request.Params
	params.MultiRoundTripParams = roundTrip

	response, err := c.sendRequest(ctx, string(mcp.MethodToolsCall), params, header)
	if mismatch, ok := errors.AsType[mcp.HeaderMismatchError](err); ok && isParamHeader(mismatch.Header) {
		c.forgetTool(params.Name)
		if _, listErr := c.ListTools(ctx, mcp.ListToolsRequest{}); listErr != nil {
			return nil, fmt.Errorf("%w (listing the tools again failed: %w)", err, listErr)
		}
		response, err = c.sendRequest(ctx, string(mcp.MethodToolsCall), params, header)
	}
	if err != nil {
		return nil, err
	}
	result, err := mcp.ParseCallToolResult(response)
	if err != nil {
		return nil, err
	}
	return result, c.checkToolResultType(result.ResultType)
}

// getPromptOnce issues a single prompts/get request.
func (c *Client) getPromptOnce(
	ctx context.Context,
	request mcp.GetPromptRequest,
	roundTrip mcp.MultiRoundTripParams,
	header http.Header,
) (*mcp.GetPromptResult, error) {
	params := request.Params
	params.MultiRoundTripParams = roundTrip

	response, err := c.sendRequest(ctx, string(mcp.MethodPromptsGet), params, header)
	if err != nil {
		return nil, err
	}
	result, err := mcp.ParseGetPromptResult(response)
	if err != nil {
		return nil, err
	}
	return result, c.checkInputResultType(result.ResultType)
}

// readResourceOnce issues a single resources/read request.
func (c *Client) readResourceOnce(
	ctx context.Context,
	request mcp.ReadResourceRequest,
	roundTrip mcp.MultiRoundTripParams,
	header http.Header,
) (*mcp.ReadResourceResult, error) {
	params := request.Params
	params.MultiRoundTripParams = roundTrip

	response, err := c.sendRequest(ctx, string(mcp.MethodResourcesRead), params, header)
	if err != nil {
		return nil, err
	}
	result, err := mcp.ParseReadResourceResult(response)
	if err != nil {
		return nil, err
	}
	return result, c.checkInputResultType(result.ResultType)
}

// callToolNeedsInput reports whether a tools/call result asks for more input.
func callToolNeedsInput(result *mcp.CallToolResult) (mcp.InputRequests, string, bool) {
	if !result.NeedsInput() {
		return nil, "", false
	}
	return result.InputRequests, result.RequestState, true
}

// getPromptNeedsInput reports whether a prompts/get result asks for more input.
func getPromptNeedsInput(result *mcp.GetPromptResult) (mcp.InputRequests, string, bool) {
	if !result.NeedsInput() {
		return nil, "", false
	}
	return result.InputRequests, result.RequestState, true
}

// readResourceNeedsInput reports whether a resources/read result asks for more
// input.
func readResourceNeedsInput(result *mcp.ReadResourceResult) (mcp.InputRequests, string, bool) {
	if !result.NeedsInput() {
		return nil, "", false
	}
	return result.InputRequests, result.RequestState, true
}

// isParamHeader reports whether name is an Mcp-Param-* header; header names
// compare case-insensitively, however the server spelled the one it refused.
func isParamHeader(name string) bool {
	prefix := mcp.HeaderParamPrefix
	return len(name) >= len(prefix) && strings.EqualFold(name[:len(prefix)], prefix)
}
