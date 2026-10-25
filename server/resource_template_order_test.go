package server

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// Among the templates matching a URI, the one with the longest literal
// prefix is served, whatever the order they were registered in; between
// equal prefixes the one registered first.
func TestReadResourcePrefersTheMostSpecificTemplate(t *testing.T) {
	orders := [][]string{
		{"file:///{+path}", "file:///docs/{name}"},
		{"file:///docs/{name}", "file:///{+path}"},
	}
	for _, order := range orders {
		s := NewMCPServer("test", "1.0.0", WithResourceCapabilities(false, false))
		for _, raw := range order {
			s.AddResourceTemplate(mcp.NewResourceTemplate(raw, raw),
				func(_ context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
					return []mcp.ResourceContents{mcp.TextResourceContents{URI: request.Params.URI, Text: raw}}, nil
				})
		}
		for range 20 {
			result, reqErr := s.handleReadResource(t.Context(), 1,
				mcp.ReadResourceRequest{Params: mcp.ReadResourceParams{URI: "file:///docs/a"}})
			require.Nil(t, reqErr)
			require.Equal(t, "file:///docs/{name}", result.Contents[0].(mcp.TextResourceContents).Text,
				"templates registered in the order %v", order)
		}
	}

	// Equal prefixes: the first registered wins, every time.
	s := NewMCPServer("test", "1.0.0", WithResourceCapabilities(false, false))
	for _, raw := range []string{"file:///{a}", "file:///{b}"} {
		s.AddResourceTemplate(mcp.NewResourceTemplate(raw, raw),
			func(_ context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
				return []mcp.ResourceContents{mcp.TextResourceContents{URI: request.Params.URI, Text: raw}}, nil
			})
	}
	for range 20 {
		result, reqErr := s.handleReadResource(t.Context(), 1,
			mcp.ReadResourceRequest{Params: mcp.ReadResourceParams{URI: "file:///x"}})
		require.Nil(t, reqErr)
		require.Equal(t, "file:///{a}", result.Contents[0].(mcp.TextResourceContents).Text)
	}
}

// Registering a template again keeps its place: between equal prefixes the
// first registered still wins, so an update to a handler does not reorder
// the templates.
func TestReRegisteringATemplateKeepsItsOrder(t *testing.T) {
	s := NewMCPServer("test", "1.0.0", WithResourceCapabilities(false, false))
	add := func(raw string) {
		s.AddResourceTemplate(mcp.NewResourceTemplate(raw, raw),
			func(_ context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
				return []mcp.ResourceContents{mcp.TextResourceContents{URI: request.Params.URI, Text: raw}}, nil
			})
	}
	add("file:///{a}")
	add("file:///{b}")
	add("file:///{a}")
	for range 20 {
		result, reqErr := s.handleReadResource(t.Context(), 1,
			mcp.ReadResourceRequest{Params: mcp.ReadResourceParams{URI: "file:///x"}})
		require.Nil(t, reqErr)
		require.Equal(t, "file:///{a}", result.Contents[0].(mcp.TextResourceContents).Text)
	}
}
