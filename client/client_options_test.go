package client

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithProtocolVersionPinsTheNegotiation(t *testing.T) {
	tests := []struct {
		name           string
		pinned         string
		wantVersion    string
		wantHandshakes int32
	}{
		{name: "a legacy version keeps the initialize handshake", pinned: mcp.LATEST_LEGACY_PROTOCOL_VERSION, wantVersion: mcp.LATEST_LEGACY_PROTOCOL_VERSION, wantHandshakes: 1},
		{name: "a modern version skips the handshake", pinned: mcp.ProtocolVersion20260728, wantVersion: mcp.ProtocolVersion20260728},
		{name: "no pin negotiates the newest", wantVersion: mcp.ProtocolVersion20260728},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var handshakes atomic.Int32
			hooks := &server.Hooks{}
			hooks.AddAfterInitialize(func(context.Context, any, *mcp.InitializeRequest, *mcp.InitializeResult) { handshakes.Add(1) })
			s := server.NewMCPServer("test-server", "1.0.0", server.WithHooks(hooks))

			c := connectInProcess(t, s, "", WithProtocolVersion(tt.pinned))

			assert.Equal(t, tt.wantVersion, c.ProtocolVersion())
			assert.Equal(t, tt.wantHandshakes, handshakes.Load())
		})
	}
}

func TestWithClientCapabilitiesMergeWithTheRequest(t *testing.T) {
	hooks := &server.Hooks{}
	capture := captureSession(hooks)
	s := server.NewMCPServer("test-server", "1.0.0", server.WithHooks(hooks))
	configured := mcp.ClientCapabilities{
		Experimental: map[string]any{"batching": true},
		Roots: &struct {
			ListChanged bool `json:"listChanged,omitempty"`
		}{ListChanged: false},
	}
	requested := mcp.ClientCapabilities{
		Extensions: map[string]any{"example.com/ext": map[string]any{}},
		Roots: &struct {
			ListChanged bool `json:"listChanged,omitempty"`
		}{ListChanged: true},
		Sampling:    &mcp.SamplingCapability{},
		Elicitation: &mcp.ElicitationCapability{},
		Tasks:       &mcp.TasksCapability{List: &struct{}{}},
	}

	c, err := NewInProcessClientWithOptions(s, WithClientCapabilities(configured))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Start(t.Context()))
	_, err = c.Initialize(t.Context(), mcp.InitializeRequest{Params: mcp.InitializeParams{
		ProtocolVersion: mcp.LATEST_LEGACY_PROTOCOL_VERSION,
		ClientInfo:      mcp.Implementation{Name: "test-client", Version: "1.0.0"},
		Capabilities:    requested,
	}})
	require.NoError(t, err)

	want := requested
	want.Experimental = configured.Experimental
	assert.Equal(t, want, c.GetClientCapabilities())
	assert.Equal(t, want, capture.get(t).(server.SessionWithClientInfo).GetClientCapabilities(),
		"the server sees the merged set")
}

// A roots handler declares the roots capability. The listChanged
// sub-capability went with notifications/roots/list_changed in protocol
// version 2026-07-28, so it is declared on the handshake era only.
func TestWithRootsHandlerDeclaresTheRootsCapability(t *testing.T) {
	for _, version := range []string{mcp.LATEST_LEGACY_PROTOCOL_VERSION, mcp.ProtocolVersion20260728} {
		t.Run(version, func(t *testing.T) {
			hooks := &server.Hooks{}
			capture := captureSession(hooks)
			s := server.NewMCPServer("test-server", "1.0.0", server.WithHooks(hooks))

			c := connectInProcess(t, s, version, WithRootsHandler(fixedRootsHandler{}))
			require.Equal(t, version, c.ProtocolVersion())

			got := capture.get(t).(server.SessionWithClientInfo).GetClientCapabilities()
			require.NotNil(t, got.Roots)
			assert.Equal(t, !mcp.IsModernProtocol(version), got.Roots.ListChanged)
		})
	}
}

func TestClientWithoutATransportCannotStart(t *testing.T) {
	c := NewClient(nil)

	require.EqualError(t, c.Start(t.Context()), "transport is nil")
	assert.Empty(t, c.GetSessionId())
	assert.Empty(t, c.ProtocolVersion())
	// Nothing was opened, so Close has nothing to close.
	assert.NoError(t, c.Close())
}

func TestInitializeRecordsTheServerCapabilities(t *testing.T) {
	for _, version := range []string{mcp.LATEST_LEGACY_PROTOCOL_VERSION, mcp.ProtocolVersion20260728} {
		t.Run(version, func(t *testing.T) {
			s := server.NewMCPServer("test-server", "1.0.0", server.WithToolCapabilities(true), server.WithPromptCapabilities(false))

			c := connectInProcess(t, s, version)

			got := c.GetServerCapabilities()
			require.NotNil(t, got.Tools)
			assert.True(t, got.Tools.ListChanged)
			require.NotNil(t, got.Prompts)
			assert.False(t, got.Prompts.ListChanged)
			assert.Nil(t, got.Resources)
		})
	}
}
