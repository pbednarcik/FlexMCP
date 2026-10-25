package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/client/transport"
	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
)

// askingServer serves a tool that asks the client for n elicitations at
// once and answers with the number of responses it got back.
func askingServer(n int) *server.MCPServer {
	s := server.NewMCPServer("test-server", "1.0.0", server.WithToolCapabilities(true))
	s.AddTool(mcp.NewTool("ask"), func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if len(request.Params.InputResponses) > 0 {
			return mcp.NewToolResultText(fmt.Sprint(len(request.Params.InputResponses))), nil
		}
		b := server.NewInputRequestBuilder("s")
		for i := range n {
			b = b.Elicit(fmt.Sprintf("q%d", i), mcp.ElicitationParams{Mode: mcp.ElicitationModeForm, Message: "?", RequestedSchema: map[string]any{"type": "object"}})
		}
		return b.ToolResult(), nil
	})
	return s
}

// concurrencyCountingHandler accepts every elicitation and records how many
// of its calls ran at once.
type concurrencyCountingHandler struct {
	running, peak atomic.Int32
}

func (h *concurrencyCountingHandler) Elicit(context.Context, mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
	now := h.running.Add(1)
	defer h.running.Add(-1)
	runtime.Gosched() // a second call, if there may be one, gets its turn
	for {
		peak := h.peak.Load()
		if now <= peak || h.peak.CompareAndSwap(peak, now) {
			break
		}
	}
	return &mcp.ElicitationResult{Action: mcp.ElicitationResponseActionAccept, Content: map[string]any{}}, nil
}

// A server's input requests are fulfilled one at a time: the user sees one
// prompt at a time, whatever a server asks, and a handler need not be safe
// to call concurrently.
func TestInputRequestsAreFulfilledOneAtATime(t *testing.T) {
	handler := &concurrencyCountingHandler{}
	c := connectInProcess(t, askingServer(200), "", WithElicitationHandler(handler))

	result, err := c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "ask"}})
	require.NoError(t, err)
	assert.Equal(t, "200", result.Content[0].(mcp.TextContent).Text, "every request was answered")
	assert.Equal(t, int32(1), handler.peak.Load(), "handlers ran one at a time")
}

type panickingRootsHandler struct{}

func (panickingRootsHandler) ListRoots(context.Context, mcp.ListRootsRequest) (*mcp.ListRootsResult, error) {
	var m map[string]int
	m["x"] = 1 //nolint:staticcheck // the nil-map write is the panic under test
	return nil, nil
}

// A handler that panics fails the call with an error; the process lives.
func TestAPanickingHandlerFailsTheCall(t *testing.T) {
	var calls atomic.Int32
	c := connectInProcess(t, newInputServer(&calls), "", WithRootsHandler(panickingRootsHandler{}))

	_, err := c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "list_roots"}})
	require.Error(t, err)
	assert.ErrorContains(t, err, "panic")
	assert.ErrorContains(t, err, "nil map")
}

// decliningServer serves a tool that asks one question and answers with the
// action the client took.
func decliningServer() *server.MCPServer {
	s := server.NewMCPServer("test-server", "1.0.0", server.WithToolCapabilities(true))
	s.AddTool(mcp.NewTool("ask"), func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if answer := server.ElicitationResponse(request.Params.InputResponses, "who"); answer != nil {
			return mcp.NewToolResultText(string(answer.Action)), nil
		}
		return server.NewInputRequestBuilder("s").
			Elicit("who", mcp.ElicitationParams{Mode: mcp.ElicitationModeForm, Message: "?", RequestedSchema: map[string]any{"type": "object"}}).
			ToolResult(), nil
	})
	return s
}

type failingElicitationHandler struct{ err error }

func (h failingElicitationHandler) Elicit(context.Context, mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
	return nil, h.err
}

// An elicitation handler's error is the user not answering: the server gets
// a cancel, as when the user dismisses the prompt, and the call goes on.
func TestAnElicitationHandlerErrorAnswersCancel(t *testing.T) {
	c := connectInProcess(t, decliningServer(), "", WithElicitationHandler(failingElicitationHandler{err: errors.New("dialog dismissed")}))

	result, err := c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "ask"}})
	require.NoError(t, err)
	assert.Equal(t, string(mcp.ElicitationResponseActionCancel), result.Content[0].(mcp.TextContent).Text)
}

type cancellingElicitationHandler struct{ cancel context.CancelFunc }

func (h cancellingElicitationHandler) Elicit(ctx context.Context, _ mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
	h.cancel()
	<-ctx.Done()
	return nil, ctx.Err()
}

// A call whose context ends while the user is asked fails with that error,
// not with a cancel answer sent on.
func TestAnElicitationEndedByTheContextFailsTheCall(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c := connectInProcess(t, decliningServer(), "", WithElicitationHandler(cancellingElicitationHandler{cancel: cancel}))

	_, err := c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "ask"}})
	require.ErrorIs(t, err, context.Canceled)
	assert.ErrorContains(t, err, "fulfilling input request", "the failure is the handler's, not a retry's")
}

// typedSamplingHandler answers only when the content reached it typed, as
// the handshake-era path delivers it.
type typedSamplingHandler struct{}

func (typedSamplingHandler) CreateMessage(_ context.Context, request mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
	text, ok := request.Messages[0].Content.(mcp.TextContent)
	if !ok {
		return nil, fmt.Errorf("content is %T, not mcp.TextContent", request.Messages[0].Content)
	}
	return &mcp.CreateMessageResult{Role: mcp.RoleAssistant, Content: mcp.TextContent{Type: "text", Text: "echo: " + text.Text}, Model: "echo"}, nil
}

// A sampling request reaches the handler with typed content on the
// multi round-trip path, as it does on the handshake-era path.
func TestSamplingContentReachesTheHandlerTyped(t *testing.T) {
	var calls atomic.Int32
	c := connectInProcess(t, newInputServer(&calls), "", WithSamplingHandler(typedSamplingHandler{}))

	result, err := c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "summarise"}})
	require.NoError(t, err)
	assert.Equal(t, "echo: summarise this", result.Content[0].(mcp.TextContent).Text)
}

// countingElicitationHandler counts its calls and accepts.
type countingElicitationHandler struct{ calls atomic.Int32 }

func (h *countingElicitationHandler) Elicit(context.Context, mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
	h.calls.Add(1)
	return &mcp.ElicitationResult{Action: mcp.ElicitationResponseActionAccept, Content: map[string]any{}}, nil
}

// elicitationScript answers tools/call with one elicitation input request.
func elicitationScript(params mcp.ElicitationParams) func(context.Context, int, transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	return modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		encoded, err := json.Marshal(server.NewInputRequestBuilder("s").Elicit("q", params).ToolResult())
		if err != nil {
			return nil, err
		}
		return resultResponse(request, string(encoded)), nil
	})
}

// An elicitation request that is not valid, or asks for a mode the client
// did not declare, never reaches the handler: the call fails instead.
func TestInvalidOrUndeclaredElicitationsNeverReachTheHandler(t *testing.T) {
	for name, params := range map[string]mcp.ElicitationParams{
		"url mode without a url":              {Mode: mcp.ElicitationModeURL, Message: "open"},
		"a bogus mode":                        {Mode: "bogus", Message: "?"},
		"a form without a schema":             {Mode: mcp.ElicitationModeForm, Message: "?"},
		"url mode the client did not declare": {Mode: mcp.ElicitationModeURL, Message: "open", URL: "https://example.com/consent"},
	} {
		t.Run(name, func(t *testing.T) {
			handler := &countingElicitationHandler{}
			tr := &scriptedTransport{respond: elicitationScript(params)}
			c := connectScripted(t, tr, WithElicitationHandler(handler))

			_, err := c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "ask"}})
			require.Error(t, err)
			assert.Equal(t, int32(0), handler.calls.Load(), "the handler was not called")
		})
	}
}
