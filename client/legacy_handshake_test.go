package client

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/client/transport"
	"github.com/pbednarcik/flexmcp/mcp"
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

// recordingElicitation answers every elicitation with a decline and records
// that it was asked.
type recordingElicitation struct{ asked bool }

func (h *recordingElicitation) Elicit(context.Context, mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
	h.asked = true
	return &mcp.ElicitationResult{Action: mcp.ElicitationResponseActionDecline}, nil
}

// A handshake-era server's elicitation in a mode the client did not declare
// is refused -32602 before the handler sees it, as a multi round-trip input
// request is (ClientFeatures.md row 19); a declared mode reaches it.
func TestClient_LegacyElicitationInAnUndeclaredModeIsRefused(t *testing.T) {
	urlRequest := transport.JSONRPCRequest{
		JSONRPC: "2.0",
		ID:      mcp.NewRequestId(int64(1)),
		Method:  string(mcp.MethodElicitationCreate),
		Params:  map[string]any{"mode": "url", "message": "sign in", "url": "https://example.com", "elicitationId": "e1"},
	}
	for name, tt := range map[string]struct {
		declared *mcp.ElicitationCapability
		refused  bool
	}{
		"form only":    {refused: true},
		"url declared": {declared: &mcp.ElicitationCapability{URL: &struct{}{}}},
	} {
		t.Run(name, func(t *testing.T) {
			handler := &recordingElicitation{}
			c := NewClient(&handshakeServer{version: mcp.LATEST_LEGACY_PROTOCOL_VERSION}, WithElicitationHandler(handler))
			request := mcp.InitializeRequest{}
			request.Params.Capabilities.Elicitation = tt.declared
			_, err := c.Initialize(t.Context(), request)
			require.NoError(t, err)

			_, err = c.handleIncomingRequest(t.Context(), urlRequest)
			if tt.refused {
				assert.ErrorIs(t, err, mcp.ErrInvalidParams)
				assert.False(t, handler.asked, "the handler saw an undeclared mode")
				return
			}
			assert.NoError(t, err)
			assert.True(t, handler.asked)
		})
	}
}

// Form mode is declared by a form sub-object, or by an elicitation object
// with neither sub-object, which means form only (ClientFeatures.md row 1);
// an elicitation without a mode is form (row 4). A client that declared url
// alone refuses a form elicitation -32602 before the handler sees it.
func TestClient_LegacyFormElicitationNeedsFormDeclared(t *testing.T) {
	for _, mode := range []string{"form", ""} {
		formRequest := transport.JSONRPCRequest{
			JSONRPC: "2.0",
			ID:      mcp.NewRequestId(int64(1)),
			Method:  string(mcp.MethodElicitationCreate),
			Params: map[string]any{"mode": mode, "message": "fill this", "requestedSchema": map[string]any{
				"type": "object", "properties": map[string]any{},
			}},
		}
		for name, tt := range map[string]struct {
			declared *mcp.ElicitationCapability
			refused  bool
		}{
			"url only":      {declared: &mcp.ElicitationCapability{URL: &struct{}{}}, refused: true},
			"empty object":  {declared: &mcp.ElicitationCapability{}},
			"form declared": {declared: &mcp.ElicitationCapability{Form: &struct{}{}}},
			"both declared": {declared: &mcp.ElicitationCapability{Form: &struct{}{}, URL: &struct{}{}}},
		} {
			t.Run("mode="+mode+"/"+name, func(t *testing.T) {
				handler := &recordingElicitation{}
				c := NewClient(&handshakeServer{version: mcp.LATEST_LEGACY_PROTOCOL_VERSION}, WithElicitationHandler(handler))
				request := mcp.InitializeRequest{}
				request.Params.Capabilities.Elicitation = tt.declared
				_, err := c.Initialize(t.Context(), request)
				require.NoError(t, err)

				_, err = c.handleIncomingRequest(t.Context(), formRequest)
				if tt.refused {
					assert.ErrorIs(t, err, mcp.ErrInvalidParams)
					assert.False(t, handler.asked, "the handler saw an undeclared mode")
					return
				}
				assert.NoError(t, err)
				assert.True(t, handler.asked)
			})
		}
	}
}

// noRoots is a roots handler with no roots.
type noRoots struct{}

func (noRoots) ListRoots(context.Context, mcp.ListRootsRequest) (*mcp.ListRootsResult, error) {
	return &mcp.ListRootsResult{}, nil
}

// TestClient_ARepeatedInitializeThatFallsBackDeclaresHandshakeCapabilities
// initializes a client with a roots handler against a modern server, then
// again against one that refuses server/discover: the handshake it falls back
// to declares roots with listChanged, as every handshake does, not as the
// modern era it starts from would.
func TestClient_ARepeatedInitializeThatFallsBackDeclaresHandshakeCapabilities(t *testing.T) {
	var legacy atomic.Bool
	var initializeParams atomic.Value
	tr := &scriptedTransport{respond: func(_ context.Context, _ int, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		switch request.Method {
		case string(mcp.MethodServerDiscover):
			if legacy.Load() {
				return transport.NewJSONRPCErrorResponse(request.ID, mcp.METHOD_NOT_FOUND, "method not found", nil), nil
			}
			return resultResponse(request, discoverResult), nil
		case string(mcp.MethodInitialize):
			encoded, err := json.Marshal(request.Params)
			if err != nil {
				return nil, err
			}
			initializeParams.Store(encoded)
			return resultResponse(request, legacyInitializeResult), nil
		}
		return resultResponse(request, `{}`), nil
	}}
	c := connectScripted(t, tr, WithRootsHandler(noRoots{}))
	require.True(t, c.isModern(), "the first Initialize settles on the modern era")

	legacy.Store(true)
	_, err := c.Initialize(t.Context(), mcp.InitializeRequest{})
	require.NoError(t, err)

	encoded, ok := initializeParams.Load().([]byte)
	require.True(t, ok, "the second Initialize sent initialize")
	var params struct {
		Capabilities struct {
			Roots *struct {
				ListChanged bool `json:"listChanged"`
			} `json:"roots"`
		} `json:"capabilities"`
	}
	require.NoError(t, json.Unmarshal(encoded, &params))
	require.NotNil(t, params.Capabilities.Roots, "roots is declared")
	assert.True(t, params.Capabilities.Roots.ListChanged, "the handshake declares roots with listChanged")
}

// TestClient_AHandshakeAnsweredWithAModernVersionIsRefused answers the
// initialize handshake with protocol version 2026-07-28, which the handshake
// cannot negotiate: that version removed it. A client that cannot speak the
// answer does not take it (Lifecycle.md row 18): Initialize fails with the
// version, and the client is neither initialized nor on the modern era.
func TestClient_AHandshakeAnsweredWithAModernVersionIsRefused(t *testing.T) {
	server := &handshakeServer{version: mcp.ProtocolVersion20260728}
	c := NewClient(server)

	_, err := c.Initialize(t.Context(), mcp.InitializeRequest{})

	var unsupported mcp.UnsupportedProtocolVersionError
	require.ErrorAs(t, err, &unsupported)
	assert.Equal(t, mcp.ProtocolVersion20260728, unsupported.Version)
	assert.False(t, c.IsInitialized(), "the client is not initialized")
	assert.False(t, c.isModern(), "the client is not on the modern era")
}
