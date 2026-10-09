package client

import (
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/server"
)

// NewInProcessClient connect directly to a mcp server object in the same process
func NewInProcessClient(server *server.MCPServer) (*Client, error) {
	inProcessTransport := transport.NewInProcessTransport(server)
	return NewClient(inProcessTransport), nil
}

// NewInProcessClientWithOptions connects directly to an MCP server in the
// same process and applies client options.
func NewInProcessClientWithOptions(mcpServer *server.MCPServer, options ...ClientOption) (*Client, error) {
	client := NewClient(nil, options...)
	client.transport = transport.NewInProcessTransport(mcpServer)
	return client, nil
}
