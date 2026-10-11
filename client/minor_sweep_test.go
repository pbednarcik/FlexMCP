package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requestMeta decodes the _meta of a request's params as the transport sees
// them.
func requestMeta(t *testing.T, request transport.JSONRPCRequest) map[string]json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(request.Params)
	require.NoError(t, err)
	var params struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	require.NoError(t, json.Unmarshal(encoded, &params))
	return params.Meta
}

// The resultType values a client accepts are the core ones and those of the
// extensions it knows; any other value is invalid, not "complete".
func TestUnknownResultTypesAreRefused(t *testing.T) {
	tests := []struct {
		name   string
		result string
		call   func(*Client) error
	}{
		{name: "tools/list", result: `{"resultType":"bogus","tools":[]}`, call: func(c *Client) error {
			_, err := c.ListTools(t.Context(), mcp.ListToolsRequest{})
			return err
		}},
		{name: "tools/call", result: `{"resultType":"bogus","content":[]}`, call: func(c *Client) error {
			_, err := c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "t"}})
			return err
		}},
		{name: "prompts/get", result: `{"resultType":"bogus","messages":[]}`, call: func(c *Client) error {
			_, err := c.GetPrompt(t.Context(), mcp.GetPromptRequest{Params: mcp.GetPromptParams{Name: "p"}})
			return err
		}},
		{name: "resources/read", result: `{"resultType":"bogus","contents":[]}`, call: func(c *Client) error {
			_, err := c.ReadResource(t.Context(), mcp.ReadResourceRequest{Params: mcp.ReadResourceParams{URI: "file:///x"}})
			return err
		}},
		{name: "completion/complete", result: `{"resultType":"bogus","completion":{"values":[]}}`, call: func(c *Client) error {
			_, err := c.Complete(t.Context(), mcp.CompleteRequest{})
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
				return resultResponse(request, tt.result), nil
			})}
			c := connectScripted(t, tr)

			err := tt.call(c)
			require.Error(t, err)
			assert.ErrorContains(t, err, `resultType "bogus"`)
		})
	}
}

// Every request should carry io.modelcontextprotocol/clientInfo; a caller
// that named none gets this SDK's own.
func TestClientInfoIsSentByDefault(t *testing.T) {
	var seen mcp.Implementation
	var mu sync.Mutex
	tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.Unmarshal(requestMeta(t, request)[mcp.MetaKeyClientInfo], &seen)
		return resultResponse(request, `{"tools":[]}`), nil
	})}
	c := connectScripted(t, tr)

	_, err := c.ListTools(t.Context(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "mcp-go", seen.Name)
	assert.NotEmpty(t, seen.Version)
}

// The roots capability of the modern era has no listChanged: the
// notification it announced is gone, and so is RootListChanges there.
func TestRootsOnTheModernEraHaveNoListChanged(t *testing.T) {
	var roots json.RawMessage
	var mu sync.Mutex
	tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		var capabilities struct {
			Roots json.RawMessage `json:"roots"`
		}
		_ = json.Unmarshal(requestMeta(t, request)[mcp.MetaKeyClientCapabilities], &capabilities)
		mu.Lock()
		roots = capabilities.Roots
		mu.Unlock()
		return resultResponse(request, `{"tools":[]}`), nil
	})}
	c := connectScripted(t, tr, WithRootsHandler(fixedRootsHandler{}))

	_, err := c.ListTools(t.Context(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	mu.Lock()
	assert.JSONEq(t, `{}`, string(roots))
	mu.Unlock()

	require.NoError(t, c.RootListChanges(t.Context()))
	assert.Empty(t, tr.sentNotifications(), "notifications/roots/list_changed was removed in 2026-07-28")
}

// On the handshake era RootListChanges still sends the notification.
func TestRootListChangesIsSentOnTheLegacyEra(t *testing.T) {
	tr := &scriptedTransport{respond: func(_ context.Context, _ int, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		return resultResponse(request, legacyInitializeResult), nil
	}}
	c := connectScripted(t, tr, WithLegacyProtocolOnly())

	require.NoError(t, c.RootListChanges(t.Context()))
	assert.Equal(t, []string{string(mcp.MethodNotificationInitialized), mcp.MethodNotificationRootsListChanged}, tr.sentNotifications())
}

// A root is a file:// URI; the client does not hand a server anything else.
func TestRootsMustBeFileURIs(t *testing.T) {
	var calls atomic.Int32
	s := newInputServer(&calls)
	c := connectInProcess(t, s, "", WithRootsHandler(fixedRootsHandler{roots: []mcp.Root{{URI: "https://example.com/not-a-root"}}}))

	_, err := c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "list_roots"}})
	require.Error(t, err)
	assert.ErrorContains(t, err, "file://")
}

// refreshingToolsServer serves a tool whose x-mcp-header annotation changes
// between listings, and rejects a tools/call that mirrors the old header.
type refreshingToolsServer struct {
	mu         sync.Mutex
	listings   int
	calls      []http.Header
	t          *testing.T
	firstParam string // the header of the first listing
	laterParam string // the header of every later listing
}

func (s *refreshingToolsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.NewDecoder(r.Body).Decode(&request)
	w.Header().Set("Content-Type", "application/json")
	s.mu.Lock()
	defer s.mu.Unlock()
	switch request.Method {
	case string(mcp.MethodServerDiscover):
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"supportedVersions":[%q],"capabilities":{"tools":{}},"serverInfo":{"name":"s","version":"1"}}}`, request.ID, mcp.ProtocolVersion20260728)
	case string(mcp.MethodToolsList):
		s.listings++
		param := s.laterParam
		if s.listings == 1 {
			param = s.firstParam
		}
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"q","inputSchema":{"type":"object","properties":{"region":{"type":"string","x-mcp-header":%q}}}}]}}`, request.ID, param)
	case string(mcp.MethodToolsCall):
		s.calls = append(s.calls, r.Header.Clone())
		if r.Header.Get(mcp.HeaderParamPrefix+s.laterParam) == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":"header mismatch","data":{"header":%q}}}`, request.ID, mcp.HEADER_MISMATCH, mcp.HeaderParamPrefix+s.laterParam)
			return
		}
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"ok"}]}}`, request.ID)
	default:
		s.t.Errorf("unexpected method %q", request.Method)
	}
}

// After a header mismatch about an Mcp-Param-* header the tool's definition
// may have changed: the client lists the tools again and retries the call
// once with headers mirrored from the new definition.
func TestCallToolRefreshesTheToolAfterAParamHeaderMismatch(t *testing.T) {
	server := &refreshingToolsServer{t: t, firstParam: "Region", laterParam: "Zone"}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	trans, err := transport.NewStreamableHTTP(httpServer.URL)
	require.NoError(t, err)
	c := NewClient(trans)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Start(t.Context()))
	_, err = c.Initialize(t.Context(), mcp.InitializeRequest{})
	require.NoError(t, err)
	_, err = c.ListTools(t.Context(), mcp.ListToolsRequest{})
	require.NoError(t, err)

	result, err := c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "q", Arguments: map[string]any{"region": "eu"}}})
	require.NoError(t, err)
	assert.Equal(t, "ok", result.Content[0].(mcp.TextContent).Text)

	server.mu.Lock()
	defer server.mu.Unlock()
	assert.Equal(t, 2, server.listings, "the tools were listed again after the mismatch")
	require.Len(t, server.calls, 2, "one retry")
	assert.Equal(t, "eu", server.calls[0].Get("Mcp-Param-Region"))
	assert.Equal(t, "eu", server.calls[1].Get("Mcp-Param-Zone"))
	assert.Empty(t, server.calls[1].Get("Mcp-Param-Region"), "the stale header is gone")
}
