package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

func readResourceURI(t *testing.T, s *MCPServer, uri string) mcp.JSONRPCMessage {
	t.Helper()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "resources/read", "params": map[string]string{"uri": uri}})
	require.NoError(t, err)
	return s.HandleMessage(t.Context(), body)
}

// A template serves a URI only when its variables match it, not when its
// regexp alone does: a URI naming another variable is not found.
func TestATemplateServesOnlyAURIItsVariablesMatch(t *testing.T) {
	for _, c := range []struct {
		name, template, uri string
		served              bool
	}{
		{"wrong query variable", "test://items{?id}", "test://items?other=1", false},
		{"wrong continuation variable", "test://items?fixed=1{&id}", "test://items?fixed=1&other=1", false},
		{"wrong path parameter", "test://items{;id}", "test://items;other=1", false},
		{"query variable", "test://items{?id}", "test://items?id=1", true},
		{"omitted variable", "test://items{?id}", "test://items", true},
		{"path variable", "test://items/{id}", "test://items/1", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := NewMCPServer("p", "1.0", WithResourceCapabilities(false, false))
			called := false
			s.AddResourceTemplate(mcp.NewResourceTemplate(c.template, "items"), func(_ context.Context, r mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
				called = true
				return []mcp.ResourceContents{mcp.TextResourceContents{URI: r.Params.URI, Text: "matched"}}, nil
			})
			answer := readResourceURI(t, s, c.uri)
			assert.Equal(t, c.served, called)
			if !c.served {
				refused, ok := answer.(mcp.JSONRPCError)
				require.True(t, ok, "%#v", answer)
				assert.Equal(t, mcp.RESOURCE_NOT_FOUND, refused.Error.Code)
			}
		})
	}
}

// Of two templates with the same literal prefix, the one whose variables
// match the URI serves it, whichever was registered first.
func TestTheTemplateWhoseVariablesMatchServesTheURI(t *testing.T) {
	s := NewMCPServer("p", "1.0", WithResourceCapabilities(false, false))
	var servedBy string
	var arguments map[string]any
	for _, name := range []string{"a", "b"} {
		s.AddResourceTemplate(mcp.NewResourceTemplate("test://items{?"+name+"}", name), func(_ context.Context, r mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			servedBy, arguments = name, r.Params.Arguments
			return []mcp.ResourceContents{mcp.TextResourceContents{URI: r.Params.URI, Text: name}}, nil
		})
	}
	readResourceURI(t, s, "test://items?b=1")
	assert.Equal(t, "b", servedBy)
	assert.Equal(t, map[string]any{"b": []string{"1"}}, arguments)
}
