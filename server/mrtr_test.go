package server

import (
	"context"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// confirmThenGreet is a tool that needs a name from the user before it can
// answer, written in the multi round-trip style.
func confirmThenGreet(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if answer := ElicitationResponse(request.Params.InputResponses, "who"); answer != nil {
		if answer.Action != mcp.ElicitationResponseActionAccept {
			return mcp.NewToolResultText("nobody to greet"), nil
		}
		content, _ := answer.Content.(map[string]any)
		name, _ := content["name"].(string)
		return mcp.NewToolResultText("hello " + name), nil
	}

	return NewInputRequestBuilder("step=1").
		Elicit("who", mcp.ElicitationParams{
			Mode:    mcp.ElicitationModeForm,
			Message: "What is your name?",
			RequestedSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"name": map[string]any{"type": "string"}},
			},
		}).
		ToolResult(), nil
}

func newMRTRServer(t *testing.T) *MCPServer {
	t.Helper()
	srv := NewMCPServer("mrtr-test", "1.0.0", WithToolCapabilities(true))
	srv.AddTool(mcp.NewTool("confirmThenGreet"), confirmThenGreet)
	return srv
}

// callToolInEra invokes a tool through the full dispatch path, in the protocol
// era selected by modern, and returns the result or the request error.
func callToolInEra(
	t *testing.T,
	srv *MCPServer,
	params mcp.CallToolParams,
	modern bool,
) (*mcp.CallToolResult, *requestError) {
	t.Helper()

	ctx := srv.WithContext(t.Context(), newMRTRSession("session"))
	info := &RequestProtocolInfo{}
	if modern {
		info = &RequestProtocolInfo{
			Modern:             true,
			ProtocolVersion:    mcp.ProtocolVersion20260728,
			ClientCapabilities: &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapability{}},
		}
	}
	ctx = WithRequestProtocolInfo(ctx, info)

	result, reqErr := srv.handleToolCall(ctx, 1, mcp.CallToolRequest{Params: params})
	if reqErr != nil {
		return nil, reqErr
	}
	toolResult, ok := result.(*mcp.CallToolResult)
	require.True(t, ok, "expected a CallToolResult, got %T", result)
	return toolResult, nil
}

func TestMultiRoundTrip_ModernClientReceivesInputRequest(t *testing.T) {
	srv := newMRTRServer(t)

	// First call: the handler needs a name, so the client is asked for one.
	result, reqErr := callToolInEra(t, srv, mcp.CallToolParams{Name: "confirmThenGreet"}, true)
	require.Nil(t, reqErr)

	require.True(t, result.NeedsInput())
	assert.Equal(t, mcp.ResultTypeInputRequired, result.ResultType)
	assert.Equal(t, "step=1", result.RequestState)

	request, ok := result.InputRequests["who"]
	require.True(t, ok, "expected an input request keyed 'who'")
	assert.Equal(t, mcp.MethodElicitationCreate, request.Method)
	require.NotNil(t, request.Elicitation)
	assert.Equal(t, "What is your name?", request.Elicitation.Message)
}

func TestMultiRoundTrip_ModernClientRetriesWithAnswer(t *testing.T) {
	srv := newMRTRServer(t)

	// Second call: the client has collected the name and retries, echoing the
	// opaque request state back.
	result, reqErr := callToolInEra(t, srv, mcp.CallToolParams{
		Name:         "confirmThenGreet",
		RequestState: "step=1",
		InputResponses: mcp.InputResponses{
			"who": mcp.NewElicitationInputResponse(mcp.ElicitationResult{
				Action:  mcp.ElicitationResponseActionAccept,
				Content: map[string]any{"name": "MCP Go"},
			}),
		},
	}, true)
	require.Nil(t, reqErr)

	assert.False(t, result.NeedsInput())
	require.Len(t, result.Content, 1)
	assert.Equal(t, "hello MCP Go", result.Content[0].(mcp.TextContent).Text)
}

// A client on a protocol version before 2026-07-28 cannot answer an input
// request, and the server issues no server-initiated request on its behalf,
// so the call fails with ErrInputRequiresModernClient. A handler that sheds
// load asks nothing, so that client is told to retry later instead.
func TestMultiRoundTrip_LegacyClientGetsAnError(t *testing.T) {
	srv := newMRTRServer(t)
	srv.AddTool(mcp.NewTool("busy"), func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return NewInputRequestBuilder("retry-later").ToolResult(), nil
	})

	for _, tt := range []struct {
		tool string
		want error
	}{
		{"confirmThenGreet", ErrInputRequiresModernClient},
		{"busy", ErrLoadShedding},
	} {
		t.Run(tt.tool, func(t *testing.T) {
			result, reqErr := callToolInEra(t, srv, mcp.CallToolParams{Name: tt.tool}, false)
			assert.Nil(t, result)
			require.NotNil(t, reqErr)
			assert.Equal(t, mcp.INTERNAL_ERROR, reqErr.code)
			assert.ErrorIs(t, reqErr, tt.want)
		})
	}
}

func TestMultiRoundTrip_InputResponseAccessors(t *testing.T) {
	responses := mcp.InputResponses{
		"elicit": mcp.NewElicitationInputResponse(mcp.ElicitationResult{
			Action: mcp.ElicitationResponseActionDecline,
		}),
		"sample": mcp.NewSamplingInputResponse(mcp.CreateMessageResult{Model: "test-model"}),
		"roots": mcp.NewRootsInputResponse(mcp.ListRootsResult{
			Roots: []mcp.Root{{URI: "file:///tmp", Name: "tmp"}},
		}),
	}

	elicitation := ElicitationResponse(responses, "elicit")
	require.NotNil(t, elicitation)
	assert.Equal(t, mcp.ElicitationResponseActionDecline, elicitation.Action)

	sampling := SamplingResponse(responses, "sample")
	require.NotNil(t, sampling)
	assert.Equal(t, "test-model", sampling.Model)

	roots := RootsResponse(responses, "roots")
	require.NotNil(t, roots)
	require.Len(t, roots.Roots, 1)
	assert.Equal(t, "file:///tmp", roots.Roots[0].URI)

	assert.Nil(t, ElicitationResponse(responses, "absent"))
}

// mrtrSession is a plain client session with client info.
type mrtrSession struct {
	clientInfoStore
	sessionID string
	notify    chan mcp.JSONRPCNotification
}

func newMRTRSession(id string) *mrtrSession {
	return &mrtrSession{
		sessionID: id,
		notify:    make(chan mcp.JSONRPCNotification, 16),
	}
}

func (s *mrtrSession) SessionID() string { return s.sessionID }
func (s *mrtrSession) NotificationChannel() chan<- mcp.JSONRPCNotification {
	return s.notify
}
func (s *mrtrSession) Initialize()       {}
func (s *mrtrSession) Initialized() bool { return true }

var (
	_ ClientSession         = (*mrtrSession)(nil)
	_ SessionWithClientInfo = (*mrtrSession)(nil)
)
