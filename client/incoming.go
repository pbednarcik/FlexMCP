package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/pbednarcik/flexmcp/client/transport"
	"github.com/pbednarcik/flexmcp/mcp"
)

// requestError is a server request the client could not serve: its text as
// written, and its kind, mcp.ErrMethodNotFound or mcp.ErrInvalidParams,
// which the transport answers with the matching JSON-RPC code.
type requestError struct {
	kind error
	err  error
}

func (e requestError) Error() string   { return e.err.Error() }
func (e requestError) Unwrap() []error { return []error{e.kind, e.err} }

// requestFailed is a requestError of kind, its text formatted as fmt.Errorf
// formats it.
func requestFailed(kind error, format string, args ...any) error {
	return requestError{kind: kind, err: fmt.Errorf(format, args...)}
}

// handleIncomingRequest processes incoming requests from the server.
// This is the main entry point for server-to-client requests like sampling and elicitation.
func (c *Client) handleIncomingRequest(ctx context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	switch request.Method {
	case string(mcp.MethodSamplingCreateMessage):
		return c.handleSamplingRequestTransport(ctx, request)
	case string(mcp.MethodElicitationCreate):
		return c.handleElicitationRequestTransport(ctx, request)
	case string(mcp.MethodPing):
		return c.handlePingRequestTransport(ctx, request)
	case string(mcp.MethodListRoots):
		return c.handleListRootsRequestTransport(ctx, request)
	default:
		return nil, requestFailed(mcp.ErrMethodNotFound, "unsupported request method: %s", request.Method)
	}
}

// handleSamplingRequestTransport handles sampling requests at the transport level.
func (c *Client) handleSamplingRequestTransport(ctx context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	if c.samplingHandler == nil {
		return nil, requestFailed(mcp.ErrMethodNotFound, "no sampling handler configured")
	}

	// Parse the request parameters
	var params mcp.CreateMessageParams
	if request.Params != nil {
		paramsBytes, err := json.Marshal(request.Params)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal params: %w", err)
		}
		if err := json.Unmarshal(paramsBytes, &params); err != nil {
			return nil, requestFailed(mcp.ErrInvalidParams, "failed to unmarshal params: %w", err)
		}
	}

	// Fix content parsing - HTTP transport unmarshals TextContent as map[string]any
	// Use the helper function to properly handle content from different transports
	for i := range params.Messages {
		content, err := mcp.ParseSamplingContent(params.Messages[i].Content)
		if err != nil {
			return nil, requestFailed(mcp.ErrInvalidParams, "failed to parse content for message %d: %w", i, err)
		}
		params.Messages[i].Content = content
	}

	// Create the MCP request
	mcpRequest := mcp.CreateMessageRequest{
		Method:              string(mcp.MethodSamplingCreateMessage),
		CreateMessageParams: params,
	}

	// Call the sampling handler
	result, err := c.samplingHandler.CreateMessage(ctx, mcpRequest)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("sampling handler returned no result")
	}

	// Marshal the result
	resultBytes, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal result: %w", err)
	}

	// Create the transport response
	response := transport.NewJSONRPCResultResponse(request.ID, json.RawMessage(resultBytes))

	return response, nil
}

// checkRoots returns result with a non-nil Roots, so that a client with no
// roots answers with "roots": [] rather than null, which the schema doesn't
// allow, and refuses a root that is not a file:// URI, the only kind the
// protocol defines.
func checkRoots(result *mcp.ListRootsResult) (*mcp.ListRootsResult, error) {
	for _, root := range result.Roots {
		if !strings.HasPrefix(root.URI, "file://") {
			return nil, fmt.Errorf("root %q is not a file:// URI", root.URI)
		}
	}
	if result.Roots != nil {
		return result, nil
	}
	withRoots := *result
	withRoots.Roots = []mcp.Root{}
	return &withRoots, nil
}

// handleListRootsRequestTransport handles list roots requests at the transport level.
func (c *Client) handleListRootsRequestTransport(ctx context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	if c.rootsHandler == nil {
		return nil, requestFailed(mcp.ErrMethodNotFound, "no roots handler configured")
	}

	// Create the MCP request
	mcpRequest := mcp.ListRootsRequest{
		Method: string(mcp.MethodListRoots),
	}

	// Call the list roots handler
	result, err := c.rootsHandler.ListRoots(ctx, mcpRequest)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("roots handler returned no result")
	}
	result, err = checkRoots(result)
	if err != nil {
		return nil, err
	}

	// Marshal the result
	resultBytes, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal result: %w", err)
	}

	// Create the transport response
	response := transport.NewJSONRPCResultResponse(request.ID, json.RawMessage(resultBytes))

	return response, nil
}

// handleElicitationRequestTransport handles elicitation requests at the transport level.
func (c *Client) handleElicitationRequestTransport(ctx context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	if c.elicitationHandler == nil {
		return nil, requestFailed(mcp.ErrMethodNotFound, "no elicitation handler configured")
	}

	// Parse the request parameters
	var params mcp.ElicitationParams
	if request.Params != nil {
		paramsBytes, err := json.Marshal(request.Params)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal params: %w", err)
		}
		if err := json.Unmarshal(paramsBytes, &params); err != nil {
			return nil, requestFailed(mcp.ErrInvalidParams, "failed to unmarshal params: %w", err)
		}
	}

	// As for a multi round-trip input request: valid, and in a mode this
	// client declared.
	if err := c.checkElicitation(params); err != nil {
		return nil, requestFailed(mcp.ErrInvalidParams, "%w", err)
	}

	// Create the MCP request
	mcpRequest := mcp.ElicitationRequest{
		Method: string(mcp.MethodElicitationCreate),
		Params: params,
	}

	// Call the elicitation handler
	result, err := c.elicitationHandler.Elicit(ctx, mcpRequest)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("elicitation handler returned no result")
	}

	// Marshal the result
	resultBytes, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal result: %w", err)
	}

	// Create the transport response
	response := transport.NewJSONRPCResultResponse(request.ID, resultBytes)

	return response, nil
}

func (c *Client) handlePingRequestTransport(ctx context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	b, _ := json.Marshal(&mcp.EmptyResult{})
	return transport.NewJSONRPCResultResponse(request.ID, b), nil
}
