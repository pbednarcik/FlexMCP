package client

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedRootsHandler answers roots/list with a fixed list, or fails.
type fixedRootsHandler struct {
	roots []mcp.Root
	err   error
}

func (h fixedRootsHandler) ListRoots(context.Context, mcp.ListRootsRequest) (*mcp.ListRootsResult, error) {
	if h.err != nil {
		return nil, h.err
	}
	return &mcp.ListRootsResult{Roots: h.roots}, nil
}

// echoSamplingHandler answers a sampling request with the prompt it was given.
type echoSamplingHandler struct{}

func (echoSamplingHandler) CreateMessage(_ context.Context, request mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
	if len(request.Messages) == 0 {
		return nil, errors.New("no messages to sample from")
	}
	content, err := mcp.ParseSamplingContent(request.Messages[0].Content)
	if err != nil {
		return nil, err
	}
	text, _ := content.(mcp.TextContent)
	return &mcp.CreateMessageResult{
		Role:    mcp.RoleAssistant,
		Content: mcp.TextContent{Type: "text", Text: "echo: " + text.Text},
		Model:   "echo",
	}, nil
}

// newInputServer serves tools and a prompt that need input from the client
// before they can finish. calls counts the tools/call handler invocations.
func newInputServer(calls *atomic.Int32) *server.MCPServer {
	s := server.NewMCPServer("test-server", "1.0.0",
		server.WithToolCapabilities(true), server.WithPromptCapabilities(true), server.WithResourceCapabilities(true, true))

	s.AddTool(mcp.NewTool("list_roots"), func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		if roots := server.RootsResponse(request.Params.InputResponses, "roots"); roots != nil {
			uris := make([]string, 0, len(roots.Roots))
			for _, root := range roots.Roots {
				uris = append(uris, root.URI)
			}
			return mcp.NewToolResultText(strings.Join(uris, ",")), nil
		}
		return server.NewInputRequestBuilder("roots").Roots("roots").ToolResult(), nil
	})

	s.AddTool(mcp.NewTool("summarise"), func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		if answer := server.SamplingResponse(request.Params.InputResponses, "llm"); answer != nil {
			content, err := mcp.ParseSamplingContent(answer.Content)
			if err != nil {
				return nil, err
			}
			text, _ := content.(mcp.TextContent)
			return mcp.NewToolResultText(text.Text), nil
		}
		return server.NewInputRequestBuilder("llm").Sample("llm", mcp.CreateMessageParams{
			Messages:  []mcp.SamplingMessage{{Role: mcp.RoleUser, Content: mcp.TextContent{Type: "text", Text: "summarise this"}}},
			MaxTokens: 16,
		}).ToolResult(), nil
	})

	s.AddTool(mcp.NewTool("busy"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return server.NewInputRequestBuilder("").ToolResult(), nil
	})

	s.AddTool(mcp.NewTool("insatiable"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return server.NewInputRequestBuilder("again").
			Elicit("more", mcp.ElicitationParams{Mode: mcp.ElicitationModeForm, Message: "More?", RequestedSchema: map[string]any{"type": "object"}}).
			ToolResult(), nil
	})

	s.AddPrompt(mcp.Prompt{Name: "greeting"}, func(_ context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		if answer := server.ElicitationResponse(request.Params.InputResponses, "who"); answer != nil {
			content, _ := answer.Content.(map[string]any)
			name, _ := content["name"].(string)
			return &mcp.GetPromptResult{Messages: []mcp.PromptMessage{mcp.NewPromptMessage(mcp.RoleUser, mcp.NewTextContent("hello "+name))}}, nil
		}
		return server.NewInputRequestBuilder("step=1").
			Elicit("who", mcp.ElicitationParams{Mode: mcp.ElicitationModeForm, Message: "What is your name?", RequestedSchema: map[string]any{"type": "object"}}).
			PromptResult(), nil
	})

	return s
}

func TestCallToolAnswersRootsAndSamplingRequests(t *testing.T) {
	tests := []struct {
		name    string
		tool    string
		options []ClientOption
		want    string
		wantErr string
		// wantMissing names the capability the server reports missing
		// (-32021) when the client never declared it.
		wantMissing string
	}{
		{
			name:    "roots are answered",
			tool:    "list_roots",
			options: []ClientOption{WithRootsHandler(fixedRootsHandler{roots: []mcp.Root{{URI: "file:///work", Name: "work"}, {URI: "file:///tmp"}}})},
			want:    "file:///work,file:///tmp",
		},
		{
			name:    "sampling is answered",
			tool:    "summarise",
			options: []ClientOption{WithSamplingHandler(echoSamplingHandler{})},
			want:    "echo: summarise this",
		},
		{
			// A client with no roots handler declares no roots capability, so
			// the server refuses to ask rather than asking what it cannot answer.
			name:        "no roots handler",
			tool:        "list_roots",
			wantMissing: "roots",
		},
		{
			name:    "a failing roots handler",
			tool:    "list_roots",
			options: []ClientOption{WithRootsHandler(fixedRootsHandler{err: errors.New("roots unavailable")})},
			wantErr: "roots unavailable",
		},
		{
			name:        "no sampling handler",
			tool:        "summarise",
			wantMissing: "sampling",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			c := connectInProcess(t, newInputServer(&calls), "", tt.options...)

			result, err := c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: tt.tool}})
			if tt.wantMissing != "" {
				require.ErrorContains(t, err, tt.wantMissing)
				assert.True(t, mcp.IsMissingRequiredClientCapability(err), "want the server's -32021, got %v", err)
				assert.Equal(t, int32(1), calls.Load(), "the server refuses once, the client does not retry")
				return
			}
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				assert.ErrorContains(t, err, "fulfilling input request")
				assert.Equal(t, int32(1), calls.Load(), "the call is not retried without an answer")
				return
			}
			require.NoError(t, err)
			require.Len(t, result.Content, 1)
			assert.Equal(t, tt.want, result.Content[0].(mcp.TextContent).Text)
			assert.Equal(t, int32(2), calls.Load(), "one call to ask, one to answer")
		})
	}
}

func TestCallToolGivesUpWhenTheServerKeepsSheddingLoad(t *testing.T) {
	var calls atomic.Int32
	c := connectInProcess(t, newInputServer(&calls), "")

	_, err := c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "busy"}})
	require.ErrorIs(t, err, ErrMaxInputRoundTripsExceeded)
	assert.Equal(t, int32(3), calls.Load())
}

func TestCallToolHonoursTheRoundTripLimit(t *testing.T) {
	var calls atomic.Int32
	c := connectInProcess(t, newInputServer(&calls), "", WithElicitationHandler(acceptingElicitationHandler{}), WithMaxInputRoundTrips(2))

	_, err := c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "insatiable"}})
	require.ErrorIs(t, err, ErrMaxInputRoundTripsExceeded)
	assert.ErrorContains(t, err, "(2)")
	assert.Equal(t, int32(2), calls.Load())
}

func TestGetPromptAnswersInputRequests(t *testing.T) {
	var calls atomic.Int32
	handler := &nameElicitationHandler{}
	c := connectInProcess(t, newInputServer(&calls), "", WithElicitationHandler(handler))

	result, err := c.GetPrompt(t.Context(), mcp.GetPromptRequest{Params: mcp.GetPromptParams{Name: "greeting"}})
	require.NoError(t, err)
	require.Len(t, result.Messages, 1)
	assert.Equal(t, "hello MCP Go", result.Messages[0].Content.(mcp.TextContent).Text)
	assert.Equal(t, 1, handler.calls)
}

func TestPromptAndResourceErrorsSurface(t *testing.T) {
	var calls atomic.Int32
	c := connectInProcess(t, newInputServer(&calls), "")

	_, err := c.GetPrompt(t.Context(), mcp.GetPromptRequest{Params: mcp.GetPromptParams{Name: "missing"}})
	require.Error(t, err)

	_, err = c.ReadResource(t.Context(), mcp.ReadResourceRequest{Params: mcp.ReadResourceParams{URI: "test://missing"}})
	require.Error(t, err)
}
