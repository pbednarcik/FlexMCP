package client

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pbednarcik/flexmcp/client/transport"
	"github.com/pbednarcik/flexmcp/server"
)

// GetEndpoint on a client that is not SSE-based answers nil, not a panic.
func TestGetEndpointOnANonSSEClientIsNil(t *testing.T) {
	c, err := NewInProcessClient(server.NewMCPServer("test-server", "1.0.0"))
	assert.NoError(t, err)
	assert.Nil(t, GetEndpoint(c))
}

// GetStderr reports no reader when the stdio transport has none to give.
func TestGetStderrWithoutAStderrStreamReportsNone(t *testing.T) {
	c := NewClient(transport.NewIO(strings.NewReader(""), nopWriteCloser{}, nil))
	reader, ok := GetStderr(c)
	assert.False(t, ok)
	assert.Nil(t, reader)
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }
