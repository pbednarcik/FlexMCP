package server

import "net/http/httptest"

// NewTestStreamableHTTPServer creates a test Streamable HTTP server for internal package tests.
// It lives in a _test.go file so production builds do not link net/http/httptest.
// External packages should use servertest.NewTestStreamableHTTPServer.
func NewTestStreamableHTTPServer(srv *MCPServer, opts ...StreamableHTTPOption) *httptest.Server {
	s := NewStreamableHTTPServer(srv, opts...)
	return httptest.NewServer(s)
}
