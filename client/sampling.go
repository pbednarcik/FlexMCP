package client

import (
	"context"

	"github.com/pbednarcik/flexmcp/mcp"
)

// SamplingHandler defines the interface for handling sampling requests from servers.
// Clients can implement this interface to provide LLM sampling capabilities to servers.
//
// Sampling is Deprecated in protocol version 2026-07-28: new servers call
// their LLM provider directly.
type SamplingHandler interface {
	// CreateMessage handles a sampling request from the server and returns the generated message.
	// The implementation should:
	// 1. Validate the request parameters
	// 2. Let a human deny the request (human-in-the-loop), and rate-limit
	// 3. Select an appropriate model based on preferences
	// 4. Generate the response using the selected model, capping the
	//    iterations of a tool-use loop and handling sensitive data with care
	// 5. Return the result with model information and stop reason
	CreateMessage(ctx context.Context, request mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error)
}

// WithSamplingHandler sets the sampling handler for the client.
// When set, the client declares the sampling capability, unless the
// capabilities it was given declare it already: in the initialize handshake
// on a legacy connection, and in every request's _meta on protocol version
// 2026-07-28.
func WithSamplingHandler(handler SamplingHandler) ClientOption {
	return func(c *Client) {
		c.samplingHandler = handler
	}
}
