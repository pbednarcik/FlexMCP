package client

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
)

// ElicitationHandler defines the interface for handling elicitation requests from servers.
// Clients can implement this interface to request additional information from users.
type ElicitationHandler interface {
	// Elicit handles an elicitation request from the server and returns the user's response.
	// The implementation should:
	// 1. Present the request message to the user, naming the server that asks
	// 2. In URL mode, show the full URL and its host and get the user's
	//    consent before opening it; never prefetch it, and open it where
	//    neither the client nor the model can read the page
	// 3. Let the user review and edit the form answers, and validate them
	//    against the requested schema (form mode)
	// 4. Allow the user to accept, decline, or cancel
	// 5. Return the appropriate response
	Elicit(ctx context.Context, request mcp.ElicitationRequest) (*mcp.ElicitationResult, error)
}

// WithElicitationHandler sets the elicitation handler for the client.
// When set, the client declares the elicitation capability, unless the
// capabilities it was given declare it already: in the initialize handshake
// on a legacy connection, and in every request's _meta on protocol version
// 2026-07-28.
func WithElicitationHandler(handler ElicitationHandler) ClientOption {
	return func(c *Client) {
		c.elicitationHandler = handler
	}
}
