package client

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/client/transport"
	"github.com/pbednarcik/flexmcp/mcp"
)

// discoverErrorTransport fails server/discover with the errors in discover,
// one per attempt, answers it with a result once they run out, and answers
// initialize as a legacy server would. It records the methods it was sent.
type discoverErrorTransport struct {
	mu       sync.Mutex
	discover []error
	methods  []string
}

func (m *discoverErrorTransport) Start(context.Context) error { return nil }

func (m *discoverErrorTransport) SendRequest(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.methods = append(m.methods, request.Method)
	switch request.Method {
	case "server/discover":
		if len(m.discover) > 0 {
			err := m.discover[0]
			m.discover = m.discover[1:]
			return nil, err
		}
		return transport.NewJSONRPCResultResponse(request.ID, json.RawMessage(fmt.Sprintf(
			`{"supportedVersions":[%q],"capabilities":{}}`, mcp.LATEST_PROTOCOL_VERSION))), nil
	case "initialize":
		return transport.NewJSONRPCResultResponse(request.ID, json.RawMessage(fmt.Sprintf(
			`{"protocolVersion":%q,"capabilities":{},"serverInfo":{"name":"legacy","version":"1"}}`, mcp.LATEST_LEGACY_PROTOCOL_VERSION))), nil
	}
	return nil, fmt.Errorf("no mock response for method %s", request.Method)
}

func (m *discoverErrorTransport) SendNotification(context.Context, mcp.JSONRPCNotification) error {
	return nil
}

func (m *discoverErrorTransport) SetNotificationHandler(func(mcp.JSONRPCNotification)) {}

func (m *discoverErrorTransport) Close() error { return nil }

func (m *discoverErrorTransport) GetSessionId() string { return "" }

func (m *discoverErrorTransport) sent() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.methods...)
}

func initializeWith(t *testing.T, tr *discoverErrorTransport) (*Client, error) {
	t.Helper()
	c := NewClient(tr)
	_, err := c.Initialize(t.Context(), mcp.InitializeRequest{Params: mcp.InitializeParams{
		ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
		ClientInfo:      mcp.Implementation{Name: "test-client", Version: "1.0"},
	}})
	return c, err
}

// A transport may hand the negotiation a protocol error as a pointer: it
// negotiates the same as with the value. A header mismatch is a modern
// server's answer, returned rather than fallen back from, and a version
// rejection naming a shared modern version is renegotiated.
func TestNegotiationTreatsPointerProtocolErrorsAsValues(t *testing.T) {
	forms := func(value, pointer error) map[string]error {
		return map[string]error{"value": value, "pointer": pointer}
	}

	for form, mismatch := range forms(mcp.HeaderMismatchError{}, &mcp.HeaderMismatchError{}) {
		t.Run("header mismatch as a "+form, func(t *testing.T) {
			tr := &discoverErrorTransport{discover: []error{mismatch}}
			_, err := initializeWith(t, tr)
			require.Error(t, err)
			assert.True(t, mcp.IsHeaderMismatch(err), "%v", err)
			assert.NotContains(t, tr.sent(), "initialize", "fell back to the handshake")
		})
	}

	rejection := mcp.UnsupportedProtocolVersionError{Version: mcp.LATEST_PROTOCOL_VERSION, Supported: []string{mcp.LATEST_PROTOCOL_VERSION}}
	for form, unsupported := range forms(rejection, &rejection) {
		t.Run("version rejection as a "+form, func(t *testing.T) {
			tr := &discoverErrorTransport{discover: []error{unsupported}}
			c, err := initializeWith(t, tr)
			require.NoError(t, err)
			assert.Equal(t, mcp.LATEST_PROTOCOL_VERSION, c.ProtocolVersion())
			assert.Equal(t, []string{"server/discover", "server/discover"}, tr.sent())
		})
	}
}
