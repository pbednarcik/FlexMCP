package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubTaskHandler is the smallest TaskHandler: it knows no task.
type stubTaskHandler struct{}

func (stubTaskHandler) TaskState(context.Context, TaskRef) (TaskState, error) {
	return TaskState{}, ErrTaskGone
}

func (stubTaskHandler) CancelTask(context.Context, TaskRef) error { return ErrTaskGone }

// discoveredExtensions runs server/discover against a server built with opts
// and returns capabilities.extensions as the client sees it.
func discoveredExtensions(t *testing.T, opts ...ServerOption) map[string]any {
	t.Helper()
	srv := newModernTestServer(t, opts...)
	message := decodeJSONRPC(t, postModern(t, srv.URL, mcp.MethodServerDiscover, nil))
	result, ok := message["result"].(map[string]any)
	require.True(t, ok, "expected a result, got %v", message)
	capabilities, ok := result["capabilities"].(map[string]any)
	require.True(t, ok, "expected capabilities, got %v", result)
	extensions, _ := capabilities["extensions"].(map[string]any)
	return extensions
}

func TestTaskHandlerIsAdvertisedOnDiscover(t *testing.T) {
	extensions := discoveredExtensions(t, WithTaskHandler(stubTaskHandler{}))
	assert.Equal(t, map[string]any{mcp.ExtensionTasks: map[string]any{}}, extensions)
}

func TestTaskAdvertisementSurvivesOptionOrder(t *testing.T) {
	want := map[string]any{
		"x/y":              map[string]any{},
		mcp.ExtensionTasks: map[string]any{},
	}
	for name, order := range map[string]func(map[string]any) []ServerOption{
		"extensions before handler": func(in map[string]any) []ServerOption {
			return []ServerOption{WithExtensions(in), WithTaskHandler(stubTaskHandler{})}
		},
		"extensions after handler": func(in map[string]any) []ServerOption {
			return []ServerOption{WithTaskHandler(stubTaskHandler{}), WithExtensions(in)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := map[string]any{"x/y": map[string]any{}}
			opts := order(in)

			assert.Equal(t, want, discoveredExtensions(t, opts...))
			assert.Equal(t, map[string]any{"x/y": map[string]any{}}, in, "the caller's map must not be written to")
		})
	}
}

func TestWithExtensionsKeepsItsOwnCopy(t *testing.T) {
	in := map[string]any{"x/y": map[string]any{}}
	s := NewMCPServer("copy", "1.0.0", WithExtensions(in))

	in["late/addition"] = map[string]any{}
	delete(in, "x/y")

	want := map[string]any{"x/y": map[string]any{}}
	assert.Equal(t, want, s.serverCapabilitiesSnapshot().Extensions)
	discovered, rpcErr := s.handleDiscover(t.Context(), nil, mcp.DiscoverRequest{})
	require.Nil(t, rpcErr)
	assert.Equal(t, want, discovered.Capabilities.Extensions)
}

func TestWithExtensionsTasksEntryWithoutHandlerPanics(t *testing.T) {
	assert.Panics(t, func() {
		NewMCPServer("no-handler", "1.0.0",
			WithExtensions(map[string]any{mcp.ExtensionTasks: map[string]any{}}))
	})
	assert.NotPanics(t, func() {
		NewMCPServer("handler", "1.0.0",
			WithExtensions(map[string]any{mcp.ExtensionTasks: map[string]any{}}),
			WithTaskHandler(stubTaskHandler{}))
	})
}

func TestLegacyInitializeLeavesTheTasksExtensionOut(t *testing.T) {
	in := map[string]any{
		"x/y":              map[string]any{},
		mcp.ExtensionTasks: map[string]any{},
	}
	server := NewMCPServer("legacy", "1.0.0", WithExtensions(in), WithTaskHandler(stubTaskHandler{}))
	assert.Contains(t, in, mcp.ExtensionTasks, "the caller's map must keep its entry")
	response := server.HandleMessage(t.Context(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test-client","version":"1.0.0"}}}`))
	resp, ok := response.(mcp.JSONRPCResponse)
	require.True(t, ok, "expected JSONRPCResponse, got %T", response)
	wire, err := json.Marshal(resp)
	require.NoError(t, err)

	assert.Contains(t, string(wire), `"x/y"`)
	assert.NotContains(t, string(wire), mcp.ExtensionTasks)
}

func TestNoTaskHandlerNoAdvertisement(t *testing.T) {
	assert.Empty(t, discoveredExtensions(t))
	assert.NotContains(t, discoveredExtensions(t, WithExtensions(map[string]any{"x/y": map[string]any{}})), mcp.ExtensionTasks)
}
