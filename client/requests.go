package client

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pbednarcik/flexmcp/mcp"
)

// Ping sends a ping request to verify the server is responsive.
//
// Deprecated: the ping RPC was removed in protocol version 2026-07-28
// (SEP-2575); liveness is a transport concern there. On a modern connection
// this method succeeds without sending anything.
func (c *Client) Ping(ctx context.Context) error {
	if c.isModern() {
		return nil
	}
	_, err := c.sendRequest(ctx, string(mcp.MethodPing), nil, nil)
	return err
}

// ReadResource reads the contents of a resource from the server.
func (c *Client) ReadResource(
	ctx context.Context,
	request mcp.ReadResourceRequest,
) (*mcp.ReadResourceResult, error) {
	request.Params.Meta = c.injectMeta(ctx, request.Params.Meta)
	header := outboundHeader(request.Header, request.Method)
	return multiRoundTrip(ctx, c, request.Params.MultiRoundTripParams,
		func(ctx context.Context, roundTrip mcp.MultiRoundTripParams) (*mcp.ReadResourceResult, error) {
			return c.readResourceOnce(ctx, request, roundTrip, header)
		},
		readResourceNeedsInput,
	)
}

// GetPrompt gets a prompt and renders it with the provided arguments.
func (c *Client) GetPrompt(
	ctx context.Context,
	request mcp.GetPromptRequest,
) (*mcp.GetPromptResult, error) {
	request.Params.Meta = c.injectMeta(ctx, request.Params.Meta)
	header := outboundHeader(request.Header, request.Method)
	return multiRoundTrip(ctx, c, request.Params.MultiRoundTripParams,
		func(ctx context.Context, roundTrip mcp.MultiRoundTripParams) (*mcp.GetPromptResult, error) {
			return c.getPromptOnce(ctx, request, roundTrip, header)
		},
		getPromptNeedsInput,
	)
}

// CallTool invokes a tool on the server.
func (c *Client) CallTool(
	ctx context.Context,
	request mcp.CallToolRequest,
) (*mcp.CallToolResult, error) {
	request.Params.Meta = c.injectMeta(ctx, request.Params.Meta)
	header := outboundHeader(request.Header, request.Method)
	return multiRoundTrip(ctx, c, request.Params.MultiRoundTripParams,
		func(ctx context.Context, roundTrip mcp.MultiRoundTripParams) (*mcp.CallToolResult, error) {
			return c.callToolOnce(ctx, request, roundTrip, header)
		},
		callToolNeedsInput,
	)
}

// SetLevel sets the minimum severity of log messages the server should send.
//
// Protocol version 2026-07-28 removed the logging/setLevel RPC: the level is
// declared per request, in _meta (SEP-2575). On a modern connection this
// method records the level locally, and every subsequent request carries it.
//
// Deprecated: the Logging feature is deprecated as of protocol version
// 2026-07-28 (SEP-2577). Log to stderr or use OpenTelemetry instead.
func (c *Client) SetLevel(
	ctx context.Context,
	request mcp.SetLevelRequest,
) error {
	if c.isModern() {
		c.logLevelMu.Lock()
		c.logLevel = request.Params.Level
		c.logLevelMu.Unlock()
		return nil
	}
	_, err := c.sendRequest(ctx, "logging/setLevel", request.Params, outboundHeader(request.Header, request.Method))
	return err
}

// Complete requests completion suggestions from the server.
func (c *Client) Complete(
	ctx context.Context,
	request mcp.CompleteRequest,
) (*mcp.CompleteResult, error) {
	response, err := c.sendRequest(ctx, "completion/complete", request.Params, outboundHeader(request.Header, request.Method))
	if err != nil {
		return nil, err
	}

	var result mcp.CompleteResult
	if err := json.Unmarshal(*response, &result); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}
	if err := c.checkResultType(result.ResultType); err != nil {
		return nil, err
	}

	return &result, nil
}

// RootListChanges sends a roots list-changed notification to the server.
//
// Protocol version 2026-07-28 removed the notification along with the
// roots.listChanged capability: a server asks for the roots it needs through
// a multi round-trip request. On a modern connection this method sends
// nothing and returns nil.
//
// Deprecated: the Roots feature is deprecated as of protocol version
// 2026-07-28 (SEP-2577). Pass paths as tool arguments or resource URIs.
func (c *Client) RootListChanges(
	ctx context.Context,
) error {
	if c.isModern() {
		return nil
	}
	// Send root list changes notification
	notification := mcp.JSONRPCNotification{
		JSONRPC: mcp.JSONRPC_VERSION,
		Method:  mcp.MethodNotificationRootsListChanged,
	}

	err := c.transport.SendNotification(ctx, notification)
	if err != nil {
		return fmt.Errorf(
			"failed to send root list change notification: %w",
			err,
		)
	}
	return nil
}
