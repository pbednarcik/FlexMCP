package server

import (
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An empty URI is still a tie-break: the cursor after a resource with an empty
// URI keeps its dot, so the next page starts after that resource and not after
// every resource of the same name.
func TestPaginationEmptyResourceTieBreak(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		nextURI   string
		configure func(*MCPServer)
		collect   func(any) ([]string, *mcp.Cursor)
	}{
		{
			name: "resources", method: "resources/list", nextURI: "file:///next",
			configure: func(s *MCPServer) {
				s.AddResource(mcp.NewResource("", "shared"), nil)
				s.AddResource(mcp.NewResource("file:///next", "shared"), nil)
			},
			collect: func(result any) ([]string, *mcp.Cursor) {
				page := result.(mcp.ListResourcesResult)
				uris := make([]string, 0, len(page.Resources))
				for _, item := range page.Resources {
					uris = append(uris, item.URI)
				}
				return uris, page.NextCursor
			},
		},
		{
			name: "resource templates", method: "resources/templates/list", nextURI: "repo://next/{path}",
			configure: func(s *MCPServer) {
				s.AddResourceTemplate(mcp.NewResourceTemplate("", "shared"), nil)
				s.AddResourceTemplate(mcp.NewResourceTemplate("repo://next/{path}", "shared"), nil)
			},
			collect: func(result any) ([]string, *mcp.Cursor) {
				page := result.(mcp.ListResourceTemplatesResult)
				uris := make([]string, 0, len(page.ResourceTemplates))
				for _, item := range page.ResourceTemplates {
					uris = append(uris, item.URITemplate.Raw())
				}
				return uris, page.NextCursor
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewMCPServer("test-server", "1.0.0", WithResourceCapabilities(false, false), WithPaginationLimit(1))
			tt.configure(s)
			// The first request sends an empty cursor, which means the start
			// of the list.
			var cursor mcp.Cursor
			for pageNumber, expected := range [][]string{{""}, {tt.nextURI}, {}} {
				response := s.HandleMessage(t.Context(), fmt.Appendf(nil,
					`{"jsonrpc":"2.0","id":%d,"method":%q,"params":{"cursor":%q}}`, pageNumber+1, tt.method, cursor))
				rpc, ok := response.(mcp.JSONRPCResponse)
				require.True(t, ok, "unexpected response: %+v", response)
				uris, next := tt.collect(rpc.Result)
				assert.Equal(t, expected, uris)
				if pageNumber == 2 {
					assert.Nil(t, next)
					continue
				}
				require.NotNil(t, next)
				require.NotEmpty(t, *next)
				if pageNumber == 0 {
					assert.Equal(t, mcp.Cursor(base64.StdEncoding.EncodeToString([]byte("shared"))+"."), *next)
				}
				cursor = *next
			}
		})
	}
}

// A cursor the server issues is never empty, since an empty cursor means the
// start of the list: paging past an element with an empty name moves on
// rather than starting over.
func TestPaginationPastAnEmptyName(t *testing.T) {
	s := NewMCPServer("test", "1.0.0", WithPaginationLimit(1))
	prompts := []mcp.Prompt{{Name: ""}, {Name: "z"}}
	var names []string
	var cursor *mcp.Cursor
	for range 4 {
		page, next, err := listByPagination(t.Context(), s, cursor, prompts)
		require.NoError(t, err)
		for _, prompt := range page {
			names = append(names, prompt.Name)
		}
		if next == nil {
			break
		}
		require.NotEmpty(t, *next, "an issued cursor is never the start-of-list cursor")
		cursor = next
	}
	assert.Equal(t, []string{"", "z"}, names)
}
