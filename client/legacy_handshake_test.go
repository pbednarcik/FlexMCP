package client

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// handshakeServer is a transport for a server on the handshake era: it
// answers initialize with version and keeps the params it was sent; every
// other request, server/discover included, is an unknown method.
type handshakeServer struct {
	version    string
	initParams json.RawMessage
}

func (s *handshakeServer) Start(context.Context) error { return nil }

func (s *handshakeServer) SendRequest(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	if request.Method != string(mcp.MethodInitialize) {
		return &transport.JSONRPCResponse{JSONRPC: "2.0", ID: request.ID, Error: &mcp.JSONRPCErrorDetails{Code: mcp.METHOD_NOT_FOUND, Message: "not found"}}, nil
	}
	s.initParams, _ = json.Marshal(request.Params)
	result, _ := json.Marshal(map[string]any{
		"protocolVersion": s.version,
		"capabilities":    map[string]any{},
		"serverInfo":      map[string]any{"name": "server", "version": "1"},
	})
	return &transport.JSONRPCResponse{JSONRPC: "2.0", ID: request.ID, Result: result}, nil
}

func (s *handshakeServer) SendNotification(context.Context, mcp.JSONRPCNotification) error {
	return nil
}
func (s *handshakeServer) SetNotificationHandler(func(mcp.JSONRPCNotification)) {}
func (s *handshakeServer) Close() error                                         { return nil }
func (s *handshakeServer) GetSessionId() string                                 { return "" }

// The handshake names the client as a modern request does: the caller's
// clientInfo, or this SDK's when the caller names none (Lifecycle.md row 14,
// a MUST).
func TestClient_LegacyInitializeNamesTheClient(t *testing.T) {
	for name, tt := range map[string]struct {
		given mcp.Implementation
		want  string
	}{
		"the caller's": {given: mcp.Implementation{Name: "caller", Version: "1"}, want: "caller"},
		"this SDK's":   {want: defaultClientInfo().Name},
	} {
		t.Run(name, func(t *testing.T) {
			server := &handshakeServer{version: mcp.LATEST_LEGACY_PROTOCOL_VERSION}
			c := NewClient(server)
			request := mcp.InitializeRequest{}
			request.Params.ClientInfo = tt.given
			_, err := c.Initialize(t.Context(), request)
			require.NoError(t, err)

			var params struct {
				ClientInfo mcp.Implementation `json:"clientInfo"`
			}
			require.NoError(t, json.Unmarshal(server.initParams, &params))
			require.NotEmpty(t, tt.want)
			assert.Equal(t, tt.want, params.ClientInfo.Name)
		})
	}
}
