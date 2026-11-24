package server

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// A tool's result, decorated without reflection, comes out as the reflective
// copy of the same result does: the same fields, the same bytes.
func TestDecorateToolResultAsByReflection(t *testing.T) {
	s := NewMCPServer("test", "1.0.0")
	for _, tc := range []struct {
		name   string
		result mcp.CallToolResult
	}{
		{"no _meta", mcp.CallToolResult{Content: []mcp.Content{mcp.NewTextContent("ok")}}},
		{"_meta of its own", mcp.CallToolResult{
			Meta:    &mcp.Meta{ProgressToken: "p-1", AdditionalFields: map[string]any{"kept": true}},
			Content: []mcp.Content{mcp.NewTextContent("ok")},
		}},
		{"an empty _meta", mcp.CallToolResult{Meta: &mcp.Meta{}}},
		{"its own server info", mcp.CallToolResult{
			Meta: &mcp.Meta{AdditionalFields: map[string]any{
				mcp.MetaKeyServerInfo: mcp.Implementation{Name: "other", Version: "2"},
			}},
		}},
		{"its own result type", mcp.CallToolResult{ResultType: mcp.ResultTypeInputRequired}},
		{"structured and an error", mcp.CallToolResult{
			StructuredContent: map[string]any{"n": 1}, IsError: true,
			Content: []mcp.Content{mcp.NewTextContent("bad")},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fast, ok := s.decorateResult(&tc.result, mcp.MethodToolsCall)
			require.True(t, ok)
			slow, ok := s.decorateResult(tc.result, mcp.MethodToolsCall)
			require.True(t, ok)
			got, ok := fast.(*mcp.CallToolResult)
			require.True(t, ok)
			assert.Equal(t, slow.(mcp.CallToolResult), *got)
			gotBytes, err := json.Marshal(got)
			require.NoError(t, err)
			wantBytes, err := json.Marshal(slow)
			require.NoError(t, err)
			assert.Equal(t, string(wantBytes), string(gotBytes))
		})
	}
	nothing, ok := s.decorateResult((*mcp.CallToolResult)(nil), mcp.MethodToolsCall)
	assert.False(t, ok)
	assert.Nil(t, nothing)
}

// Decorating a tool's result costs 4 allocations: the copy and its _meta
// together, the _meta's map and its group, and the response boxed anew; 5
// when the copy was made by reflection and its _meta apart.
func TestDecorateToolResultAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	s := NewMCPServer("test", "1.0.0")
	info := &RequestProtocolInfo{Modern: true, ProtocolVersion: mcp.ProtocolVersion20260728}
	result := mcp.NewToolResultText("ok")
	response := mcp.JSONRPCResponse{Result: result}
	allocs := testing.AllocsPerRun(1000, func() {
		_ = s.decorateResponse(t.Context(), info, mcp.MethodToolsCall, response)
	})
	assert.LessOrEqual(t, allocs, 4.0, "allocations per tool result decorated")
}
