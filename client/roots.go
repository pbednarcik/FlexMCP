package client

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
)

// RootsHandler defines the interface for handling roots requests from servers.
// Clients can implement this interface to provide roots list to servers.
//
// Roots is Deprecated in protocol version 2026-07-28: new servers take
// directories as tool parameters, resource URIs or configuration instead.
type RootsHandler interface {
	// ListRoots handles a list root request from the server and returns the roots list.
	// The implementation should:
	// 1. Expose only roots the user permits, as file:// URIs (the client
	//    checks the scheme before sending), validated against path traversal
	// 2. Return the appropriate response
	ListRoots(ctx context.Context, request mcp.ListRootsRequest) (*mcp.ListRootsResult, error)
}

// WithRootsHandler sets the roots handler for the client.
// When set, the client declares the roots capability, unless the
// capabilities it was given declare it already: in the initialize handshake
// on a legacy connection, with listChanged, and in every request's _meta on
// protocol version 2026-07-28, without it, since that version removed the
// roots list_changed notification.
func WithRootsHandler(handler RootsHandler) ClientOption {
	return func(c *Client) {
		c.rootsHandler = handler
	}
}
