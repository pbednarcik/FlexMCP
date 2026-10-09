package client

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// greetAfterAskingName asks for a name through a multi round-trip result,
// then greets with the answer.
func greetAfterAskingName(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if answer := server.ElicitationResponse(request.Params.InputResponses, "who"); answer != nil {
		content, _ := answer.Content.(map[string]any)
		name, _ := content["name"].(string)
		return mcp.NewToolResultText("hello " + name), nil
	}
	return server.NewInputRequestBuilder("step=1").
		Elicit("who", mcp.ElicitationParams{Mode: mcp.ElicitationModeForm, Message: "What is your name?", RequestedSchema: map[string]any{"type": "object"}}).
		ToolResult(), nil
}

type nameElicitationHandler struct {
	calls   int
	message string
}

func (h *nameElicitationHandler) Elicit(_ context.Context, request mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
	h.calls++
	h.message = request.Params.Message
	return &mcp.ElicitationResult{Action: mcp.ElicitationResponseActionAccept, Content: map[string]any{"name": "MCP Go"}}, nil
}

func TestNewInProcessClientWithOptionsAppliesThem(t *testing.T) {
	mcpServer := server.NewMCPServer("test-server", "1.0.0", server.WithToolCapabilities(true))
	mcpServer.AddTool(mcp.NewTool("greet"), greetAfterAskingName)
	handler := &nameElicitationHandler{}

	c, err := NewInProcessClientWithOptions(mcpServer, WithElicitationHandler(handler), WithMaxInputRoundTrips(3))
	require.NoError(t, err)
	assert.Equal(t, 3, c.maxInputRoundTrips)
	t.Cleanup(func() { require.NoError(t, c.Close()) })

	require.NoError(t, c.Start(t.Context()))
	_, err = c.Initialize(t.Context(), mcp.InitializeRequest{Params: mcp.InitializeParams{
		ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
		ClientInfo:      mcp.Implementation{Name: "test-client", Version: "1.0.0"},
	}})
	require.NoError(t, err)

	result, err := c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "greet"}})
	require.NoError(t, err)
	require.Len(t, result.Content, 1)
	assert.Equal(t, "hello MCP Go", result.Content[0].(mcp.TextContent).Text)
	assert.Equal(t, 1, handler.calls)
	assert.Equal(t, "What is your name?", handler.message)
}

// A tool that needs authorization first returns mcp.URLElicitationRequiredError,
// and a client using protocol version 2025-11-25, which defines that error,
// gets it back with the URL to send the user to.
func TestInProcessURLElicitationRequiredError(t *testing.T) {
	mcpServer := server.NewMCPServer("test-server", "1.0.0", server.WithToolCapabilities(true))
	mcpServer.AddTool(mcp.NewTool("protected_action"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, mcp.URLElicitationRequiredError{Elicitations: []mcp.ElicitationParams{{
			Mode:          mcp.ElicitationModeURL,
			ElicitationID: "auth-1",
			URL:           "https://example.com/authorize?id=auth-1",
			Message:       "Authorization is required to access this resource.",
		}}}
	})

	c, err := NewInProcessClient(mcpServer)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, c.Close()) })
	require.NoError(t, c.Start(t.Context()))
	_, err = c.Initialize(t.Context(), mcp.InitializeRequest{Params: mcp.InitializeParams{
		ProtocolVersion: mcp.LATEST_LEGACY_PROTOCOL_VERSION,
		ClientInfo:      mcp.Implementation{Name: "test-client", Version: "1.0.0"},
		Capabilities:    mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapability{URL: &struct{}{}}},
	}})
	require.NoError(t, err)

	_, err = c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "protected_action"}})
	var required mcp.URLElicitationRequiredError
	require.ErrorAs(t, err, &required)
	require.Len(t, required.Elicitations, 1)
	assert.Equal(t, "https://example.com/authorize?id=auth-1", required.Elicitations[0].URL)
}
