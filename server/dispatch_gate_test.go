package server

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// dispatchGate mirrors one row of the generator's table
// (server/internal/gen/data.go, a main package, so not importable): the
// capability a method sits behind and the protocol era it belongs to.
type dispatchGate struct {
	method          mcp.MCPMethod
	capability      string // the word in "<capability> not supported"; "" when ungated
	removedInModern bool
	requiresModern  bool
}

var dispatchGates = []dispatchGate{
	{method: mcp.MethodInitialize, removedInModern: true},
	{method: mcp.MethodPing, removedInModern: true},
	{method: mcp.MethodServerDiscover, requiresModern: true},
	{method: mcp.MethodSubscriptionsListen, requiresModern: true},
	{method: mcp.MethodSetLogLevel, capability: "logging", removedInModern: true},
	{method: mcp.MethodResourcesList, capability: "resources"},
	{method: mcp.MethodResourcesTemplatesList, capability: "resources"},
	{method: mcp.MethodResourcesRead, capability: "resources"},
	{method: mcp.MethodPromptsList, capability: "prompts"},
	{method: mcp.MethodPromptsGet, capability: "prompts"},
	{method: mcp.MethodToolsList, capability: "tools"},
	{method: mcp.MethodToolsCall, capability: "tools"},
	{method: mcp.MethodTasksGet, capability: "tasks"},
	{method: mcp.MethodTasksList, capability: "tasks", removedInModern: true},
	{method: mcp.MethodTasksResult, capability: "tasks", removedInModern: true},
	{method: mcp.MethodTasksCancel, capability: "tasks"},
	{method: mcp.MethodCompletionComplete, capability: "completions"},
}

// everyCapabilityServer has every capability on, so only the era and
// parsing gates can answer.
func everyCapabilityServer() *MCPServer {
	return NewMCPServer("gates", "1.0.0",
		WithToolCapabilities(true),
		WithResourceCapabilities(true, true),
		WithPromptCapabilities(true),
		WithLogging(),
		WithTaskCapabilities(true, true, true),
		WithCompletions(),
	)
}

// dispatch sends one request with the given params JSON and returns the
// JSON-RPC error it was answered with.
func dispatch(t *testing.T, srv *MCPServer, method mcp.MCPMethod, params string) mcp.JSONRPCErrorDetails {
	t.Helper()
	message := `{"jsonrpc":"2.0","id":1,"method":"` + string(method) + `","params":` + params + `}`
	resp := srv.HandleMessage(t.Context(), json.RawMessage(message))
	errResp, ok := resp.(mcp.JSONRPCError)
	require.True(t, ok, "%s: expected an error response, got %T %v", method, resp, resp)
	return errResp.Error
}

func modernParams(t *testing.T) string {
	t.Helper()
	meta, err := json.Marshal(modernMeta())
	require.NoError(t, err)
	return `{"_meta":` + string(meta) + `}`
}

func TestDispatchRemovedMethodsAreUnknownToModernRequests(t *testing.T) {
	srv := everyCapabilityServer()
	for _, gate := range dispatchGates {
		if !gate.removedInModern {
			continue
		}
		t.Run(string(gate.method), func(t *testing.T) {
			details := dispatch(t, srv, gate.method, modernParams(t))
			assert.Equal(t, mcp.METHOD_NOT_FOUND, details.Code)
			assert.Equal(t, `"`+string(gate.method)+`" was removed in protocol version 2026-07-28`, details.Message)
		})
	}
}

func TestDispatchDroppedLegacyMethodsAreUnknownInBothEras(t *testing.T) {
	srv := everyCapabilityServer()
	eras := []struct{ name, params string }{
		{"legacy", `{"uri":"file:///a"}`},
		{"modern", modernParams(t)},
	}
	for _, method := range []mcp.MCPMethod{mcp.MethodResourcesSubscribe, mcp.MethodResourcesUnsubscribe} {
		for _, era := range eras {
			t.Run(string(method)+"/"+era.name, func(t *testing.T) {
				details := dispatch(t, srv, method, era.params)
				assert.Equal(t, mcp.METHOD_NOT_FOUND, details.Code)
				assert.Equal(t, "Method "+string(method)+" not found", details.Message)
			})
		}
	}
}

func TestDispatchNeverAnswersAResponse(t *testing.T) {
	srv := everyCapabilityServer()
	for _, tt := range []struct{ name, message string }{
		{"result", `{"jsonrpc":"2.0","id":5,"result":{}}`},
		{"error", `{"jsonrpc":"2.0","id":5,"error":{"code":-32601,"message":"Method not found"}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Nil(t, srv.HandleMessage(t.Context(), json.RawMessage(tt.message)))
		})
	}
}

func TestDispatchModernOnlyMethodsAreUnknownToLegacyRequests(t *testing.T) {
	srv := everyCapabilityServer()
	for _, gate := range dispatchGates {
		if !gate.requiresModern {
			continue
		}
		t.Run(string(gate.method), func(t *testing.T) {
			details := dispatch(t, srv, gate.method, `{}`)
			assert.Equal(t, mcp.METHOD_NOT_FOUND, details.Code)
			assert.Equal(t, `"`+string(gate.method)+`" requires protocol version 2026-07-28 or later`, details.Message)
		})
	}
}

func TestDispatchMethodsBehindAnAbsentCapabilityAreUnknown(t *testing.T) {
	srv := NewMCPServer("bare", "1.0.0")
	for _, gate := range dispatchGates {
		if gate.capability == "" {
			continue
		}
		t.Run(string(gate.method), func(t *testing.T) {
			details := dispatch(t, srv, gate.method, `{}`)
			assert.Equal(t, mcp.METHOD_NOT_FOUND, details.Code)
			assert.Equal(t, gate.capability+" not supported", details.Message)
		})
	}
}

// Params that are not an object cannot decode into any request type; the
// answer is -32600 naming the method, so the client knows which call was
// malformed. Modern-only methods are left out: their era check needs an
// object with _meta, so a non-object is a legacy request to them.
func TestDispatchMalformedParamsAreAnInvalidRequest(t *testing.T) {
	srv := everyCapabilityServer()
	for _, gate := range dispatchGates {
		if gate.requiresModern {
			continue
		}
		t.Run(string(gate.method), func(t *testing.T) {
			details := dispatch(t, srv, gate.method, `5`)
			assert.Equal(t, mcp.INVALID_REQUEST, details.Code)
			assert.Contains(t, details.Message, "unparsable "+string(gate.method)+" request")
		})
	}
}
