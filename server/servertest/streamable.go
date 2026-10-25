package servertest

import (
	"net/http/httptest"

	"github.com/pbednarcik/flexmcp/server"
)

// NewTestStreamableHTTPServer creates a test Streamable HTTP server. Replaces server.NewTestStreamableHTTPServer.
func NewTestStreamableHTTPServer(srv *server.MCPServer, opts ...server.StreamableHTTPOption) *httptest.Server {
	s := server.NewStreamableHTTPServer(srv, opts...)
	return httptest.NewServer(s)
}
