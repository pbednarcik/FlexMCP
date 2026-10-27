package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
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

// callToolAs invokes a tool through the full dispatch path as a modern client
// that declared caps on the request.
func callToolAs(t *testing.T, srv *MCPServer, params mcp.CallToolParams, caps mcp.ClientCapabilities) (any, *requestError) {
	t.Helper()
	ctx := srv.WithContext(t.Context(), newMRTRSession("session"))
	ctx = WithRequestProtocolInfo(ctx, &RequestProtocolInfo{
		Modern:             true,
		ProtocolVersion:    mcp.ProtocolVersion20260728,
		ClientCapabilities: &caps,
	})
	return srv.handleToolCall(ctx, 1, mcp.CallToolRequest{Params: params})
}

// A server never asks a client for input its declared capabilities cannot
// serve (Patterns.md row 5, ClientFeatures.md rows 2 and 22): the call is
// refused with -32021, and the error's data names the capabilities the client
// would need (BaseProtocol.md row 28).
func TestMultiRoundTrip_RefusesInputTheClientCannotServe(t *testing.T) {
	srv := newMRTRServer(t)
	srv.AddTool(mcp.NewTool("signIn"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return NewInputRequestBuilder("").Elicit("login", mcp.ElicitationParams{
			Mode: mcp.ElicitationModeURL, Message: "sign in", URL: "https://example.com/login",
		}).ToolResult(), nil
	})
	srv.AddTool(mcp.NewTool("summarise"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return NewInputRequestBuilder("").Sample("summary", mcp.CreateMessageParams{
			MaxTokens: 10, Tools: []mcp.Tool{mcp.NewTool("lookup")},
		}).ToolResult(), nil
	})
	srv.AddTool(mcp.NewTool("whereAmI"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return NewInputRequestBuilder("").Roots("roots").ToolResult(), nil
	})

	elicitation := func(url bool) *mcp.ElicitationCapability {
		c := &mcp.ElicitationCapability{}
		if url {
			c.URL = &struct{}{}
		}
		return c
	}
	sampling := func(tools bool) *mcp.SamplingCapability {
		c := &mcp.SamplingCapability{}
		if tools {
			c.Tools = &struct{}{}
		}
		return c
	}
	roots := &struct {
		ListChanged bool `json:"listChanged,omitempty"`
	}{}
	formElicitation := &mcp.ElicitationCapability{Form: &struct{}{}}

	tests := []struct {
		name     string
		tool     string
		declared mcp.ClientCapabilities
		// missing names the capability the error must report; empty when the
		// client can serve the request.
		missing string
		// required checks the error's data.requiredCapabilities.
		required func(mcp.ClientCapabilities) bool
	}{
		{
			name: "form elicitation needs elicitation", tool: "confirmThenGreet",
			missing:  "elicitation",
			required: func(c mcp.ClientCapabilities) bool { return c.Elicitation != nil && c.Elicitation.URL == nil },
		},
		{
			name: "form elicitation is served with elicitation", tool: "confirmThenGreet",
			declared: mcp.ClientCapabilities{Elicitation: elicitation(false)},
		},
		{
			// ClientFeatures.md row 2: a client that declared URL mode only
			// is not asked for a form.
			name: "form elicitation needs elicitation.form when only url is declared", tool: "confirmThenGreet",
			declared: mcp.ClientCapabilities{Elicitation: elicitation(true)},
			missing:  "elicitation.form",
			required: func(c mcp.ClientCapabilities) bool { return c.Elicitation != nil && c.Elicitation.Form != nil },
		},
		{
			name: "form elicitation is served with elicitation.form", tool: "confirmThenGreet",
			declared: mcp.ClientCapabilities{Elicitation: formElicitation},
		},
		{
			name: "url elicitation needs elicitation.url", tool: "signIn",
			declared: mcp.ClientCapabilities{Elicitation: elicitation(false)},
			missing:  "elicitation.url",
			required: func(c mcp.ClientCapabilities) bool { return c.Elicitation != nil && c.Elicitation.URL != nil },
		},
		{
			name: "url elicitation is served with elicitation.url", tool: "signIn",
			declared: mcp.ClientCapabilities{Elicitation: elicitation(true)},
		},
		{
			name: "sampling with tools needs sampling.tools", tool: "summarise",
			declared: mcp.ClientCapabilities{Sampling: sampling(false)},
			missing:  "sampling.tools",
			required: func(c mcp.ClientCapabilities) bool { return c.Sampling != nil && c.Sampling.Tools != nil },
		},
		{
			name: "sampling with tools is served with sampling.tools", tool: "summarise",
			declared: mcp.ClientCapabilities{Sampling: sampling(true)},
		},
		{
			name: "roots needs roots", tool: "whereAmI",
			missing:  "roots",
			required: func(c mcp.ClientCapabilities) bool { return c.Roots != nil },
		},
		{
			name: "roots is served with roots", tool: "whereAmI",
			declared: mcp.ClientCapabilities{Roots: roots},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, reqErr := callToolAs(t, srv, mcp.CallToolParams{Name: tt.tool}, tt.declared)
			if tt.missing == "" {
				require.Nil(t, reqErr)
				assert.True(t, result.(*mcp.CallToolResult).NeedsInput())
				return
			}
			assert.Nil(t, result)
			require.NotNil(t, reqErr)
			assert.Equal(t, mcp.MISSING_REQUIRED_CLIENT_CAPABILITY, reqErr.code)
			assert.True(t, mcp.IsMissingRequiredClientCapability(reqErr))
			assert.ErrorContains(t, reqErr, tt.missing)
			data, ok := reqErr.data.(mcp.MissingRequiredClientCapabilityData)
			require.True(t, ok, "error data is %T, want the required capabilities", reqErr.data)
			assert.True(t, tt.required(data.RequiredCapabilities), "data.requiredCapabilities = %+v", data.RequiredCapabilities)
		})
	}

	// Over HTTP the refusal is 400 with the same data in the body.
	t.Run("over HTTP", func(t *testing.T) {
		httpServer := httptest.NewServer(NewStreamableHTTPServer(srv))
		t.Cleanup(httpServer.Close)
		resp := postModern(t, httpServer.URL, mcp.MethodToolsCall, map[string]any{"name": "confirmThenGreet"})
		assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
		errDetails := decodeJSONRPC(t, resp)["error"].(map[string]any)
		assert.Equal(t, float64(mcp.MISSING_REQUIRED_CLIENT_CAPABILITY), errDetails["code"])
		data, _ := errDetails["data"].(map[string]any)
		required, _ := data["requiredCapabilities"].(map[string]any)
		assert.Contains(t, required, "elicitation")
	})
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

// TestClientLacksAnUndeclaredExtension pins the extension half of the
// capability check: every extension in the required set must be declared
// under the request's own capabilities.extensions (Lifecycle.md rows 8 and
// 10), whatever settings object it carries.
func TestClientLacksAnUndeclaredExtension(t *testing.T) {
	required := mcp.ClientCapabilities{Extensions: map[string]any{"example.com/a": map[string]any{}, "example.com/b": map[string]any{}}}
	declared := func(extensions map[string]any) *RequestProtocolInfo {
		return &RequestProtocolInfo{Modern: true, ClientCapabilities: &mcp.ClientCapabilities{Extensions: extensions}}
	}

	assert.False(t, clientLacks(declared(map[string]any{"example.com/a": map[string]any{}, "example.com/b": map[string]any{"setting": true}}), required), "both declared")
	assert.True(t, clientLacks(declared(map[string]any{"example.com/a": map[string]any{}}), required), "one missing")
	assert.True(t, clientLacks(declared(nil), required), "none declared")
	assert.True(t, clientLacks(nil, required), "no protocol info")
	assert.False(t, clientLacks(declared(nil), mcp.ClientCapabilities{}), "nothing required")
}
