package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pbednarcik/flexmcp/client/transport"
	"github.com/pbednarcik/flexmcp/mcp"
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
			assert.ErrorContains(t, err, `unaccepted resultType "bogus"`)
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
	assert.Equal(t, "flexmcp", seen.Name)
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
	mu              sync.Mutex
	listings        int
	calls           []http.Header
	t               *testing.T
	firstParam      string // the header of the first listing
	laterParam      string // the header of every later listing
	lowercaseHeader bool   // name the header in lower case in the error's data
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
			name := mcp.HeaderParamPrefix + s.laterParam
			if s.lowercaseHeader {
				name = strings.ToLower(name)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":"header mismatch","data":{"header":%q}}}`, request.ID, mcp.HEADER_MISMATCH, name)
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

// The "task" result type belongs to the Tasks extension, which a client
// declares as extensions["io.modelcontextprotocol/tasks"] in the capabilities
// every modern request carries, and only tools/call can be answered with a
// task. The tasks capability of 2025-11-25 is another thing (the client's own,
// for the requests it receives), and before 2026-07-28 there was no
// resultType at all.
func TestTaskResultTypeNeedsTheTasksExtension(t *testing.T) {
	const taskResult = `{"resultType":"task","taskId":"t1","status":"working","createdAt":"2026-01-01T00:00:00Z","lastUpdatedAt":"2026-01-01T00:00:00Z","ttlMs":null,"pollIntervalMs":1000}`
	withExtension := WithClientCapabilities(mcp.ClientCapabilities{Extensions: map[string]any{mcp.ExtensionTasks: map[string]any{}}})
	withLegacyTasks := WithClientCapabilities(mcp.ClientCapabilities{Tasks: &mcp.TasksCapability{}})
	callTool := func(c *Client) (mcp.ResultType, error) {
		result, err := c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "t"}})
		if err != nil {
			return "", err
		}
		// An accepted task is a handle the caller polls with: it is kept.
		require.NotNil(t, result.Task, "the task handle is kept")
		assert.Equal(t, "t1", result.Task.TaskId)
		assert.Equal(t, mcp.TaskStatusWorking, result.Task.Status)
		require.NotNil(t, result.Task.PollIntervalMs)
		assert.Equal(t, int64(1000), *result.Task.PollIntervalMs)
		assert.Nil(t, result.Task.TTLMs, "a null ttlMs is unlimited")
		return result.ResultType, nil
	}
	listTools := func(c *Client) (mcp.ResultType, error) {
		result, err := c.ListTools(t.Context(), mcp.ListToolsRequest{})
		if err != nil {
			return "", err
		}
		return result.ResultType, nil
	}
	tests := []struct {
		name     string
		options  []ClientOption
		legacy   bool
		call     func(*Client) (mcp.ResultType, error)
		accepted bool
	}{
		{name: "tools/call with the extension", options: []ClientOption{withExtension}, call: callTool, accepted: true},
		{name: "tools/call without it", call: callTool},
		{name: "tools/call with only the 2025-11-25 tasks capability", options: []ClientOption{withLegacyTasks}, call: callTool},
		{name: "tools/list with the extension", options: []ClientOption{withExtension}, call: listTools},
		{name: "tools/call with the extension on the handshake era", options: []ClientOption{withExtension, WithLegacyProtocolOnly()}, legacy: true, call: callTool},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			answer := func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
				if tt.legacy && request.Method == string(mcp.MethodInitialize) {
					return resultResponse(request, legacyInitializeResult), nil
				}
				return resultResponse(request, taskResult), nil
			}
			tr := &scriptedTransport{respond: modernScript(answer)}
			c := connectScripted(t, tr, tt.options...)

			resultType, err := tt.call(c)
			if !tt.accepted {
				require.Error(t, err)
				assert.ErrorContains(t, err, `unaccepted resultType "task"`)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, mcp.ResultTypeTask, resultType)
		})
	}
}

// Header names compare case-insensitively: a server naming the header in
// lower case still gets the refresh and the retry.
func TestCallToolRetriesOnALowercaseParamHeaderName(t *testing.T) {
	server := &refreshingToolsServer{t: t, firstParam: "Region", laterParam: "Zone", lowercaseHeader: true}
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

	_, err = c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "q", Arguments: map[string]any{"region": "eu"}}})
	require.NoError(t, err)
	server.mu.Lock()
	defer server.mu.Unlock()
	assert.Len(t, server.calls, 2, "the retry happened")
}

// A roots capability the caller supplied with listChanged is sent without it
// on the modern era, where the sub-capability is gone; what the caller
// configured is left as it is, so a handshake-era connection still sends it.
func TestSuppliedRootsCapabilityLosesListChangedOnTheModernEra(t *testing.T) {
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
	supplied := mcp.ClientCapabilities{Roots: &struct {
		ListChanged bool `json:"listChanged,omitempty"`
	}{ListChanged: true}}
	c := connectScripted(t, tr, WithClientCapabilities(supplied))

	_, err := c.ListTools(t.Context(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	mu.Lock()
	assert.JSONEq(t, `{}`, string(roots))
	mu.Unlock()
	assert.True(t, c.GetClientCapabilities().Roots.ListChanged, "the configuration is untouched")
}

// A 2025-11-25 tasks capability the caller supplied is not sent on the
// modern era, whose ClientCapabilities has no tasks member (Lifecycle.md row
// 8); what the caller configured is left as it is, so a handshake-era
// connection still sends it.
func TestSuppliedTasksCapabilityIsLeftOutOnTheModernEra(t *testing.T) {
	var declared map[string]json.RawMessage
	var mu sync.Mutex
	tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		var capabilities map[string]json.RawMessage
		_ = json.Unmarshal(requestMeta(t, request)[mcp.MetaKeyClientCapabilities], &capabilities)
		mu.Lock()
		declared = capabilities
		mu.Unlock()
		return resultResponse(request, `{"tools":[]}`), nil
	})}
	supplied := mcp.ClientCapabilities{
		Tasks:      &mcp.TasksCapability{List: &struct{}{}},
		Extensions: map[string]any{mcp.ExtensionTasks: map[string]any{}},
	}
	c := connectScripted(t, tr, WithClientCapabilities(supplied))

	_, err := c.ListTools(t.Context(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	mu.Lock()
	assert.NotContains(t, declared, "tasks")
	assert.Contains(t, declared, "extensions")
	mu.Unlock()
	assert.NotNil(t, c.GetClientCapabilities().Tasks, "the configuration is untouched")
}

// The default clientInfo names this SDK at the version the binary was built
// against: the dependency's version, the replacement's when the module is
// replaced (a local replacement has none), the main module's when this is
// the main module.
func TestDefaultClientInfoReadsTheBuildInfo(t *testing.T) {
	module := func(version string, replace *debug.Module) *debug.Module {
		return &debug.Module{Path: modulePath, Version: version, Replace: replace}
	}
	tests := []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{name: "no build info", want: "(devel)"},
		{name: "a dependency", info: &debug.BuildInfo{Main: debug.Module{Path: "example.com/app"}, Deps: []*debug.Module{module("v0.44.0", nil)}}, want: "v0.44.0"},
		{name: "replaced by a local checkout", info: &debug.BuildInfo{Main: debug.Module{Path: "example.com/app"}, Deps: []*debug.Module{module("v0.44.0", &debug.Module{Path: "../mcp-go"})}}, want: "(devel)"},
		{name: "replaced by a fork at a version", info: &debug.BuildInfo{Main: debug.Module{Path: "example.com/app"}, Deps: []*debug.Module{module("v0.44.0", &debug.Module{Path: "example.com/fork", Version: "v0.50.1"})}}, want: "v0.50.1"},
		{name: "the main module", info: &debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "v0.45.0"}}, want: "v0.45.0"},
		{name: "the main module from its tree", info: &debug.BuildInfo{Main: debug.Module{Path: modulePath, Version: "(devel)"}}, want: "(devel)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := clientInfoFrom(tt.info)
			assert.Equal(t, "flexmcp", info.Name)
			assert.Equal(t, tt.want, info.Version)
		})
	}
}
