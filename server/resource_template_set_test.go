package server

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// SetResourceTemplates replaces the templates in one step: a read that lands
// while they are replaced finds the template, never none.
func TestSetResourceTemplatesReplacesInOneStep(t *testing.T) {
	s := NewMCPServer("test", "1.0.0", WithResourceCapabilities(false, false))
	template := ServerResourceTemplate{
		Template: mcp.NewResourceTemplate("file:///{a}", "a"),
		Handler: func(_ context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{mcp.TextResourceContents{URI: request.Params.URI, Text: "a"}}, nil
		},
	}
	s.SetResourceTemplates(template)

	var notFound atomic.Int32
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, reqErr := s.handleReadResource(t.Context(), 1,
				mcp.ReadResourceRequest{Params: mcp.ReadResourceParams{URI: "file:///x"}}); reqErr != nil {
				notFound.Add(1)
			}
		}
	}()
	for range 5000 {
		s.SetResourceTemplates(template)
	}
	close(stop)
	<-stopped
	require.Zero(t, notFound.Load(), "reads found no template while the templates were replaced")
}

// SetResources and SetPrompts replace their lists in one step as well.
func TestSetResourcesAndSetPromptsReplaceInOneStep(t *testing.T) {
	read := func(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		return []mcp.ResourceContents{mcp.TextResourceContents{URI: request.Params.URI, Text: "a"}}, nil
	}
	get := func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{}, nil
	}
	cases := map[string]struct {
		set  func(s *MCPServer)
		find func(s *MCPServer) *requestError
	}{
		"resources": {
			set: func(s *MCPServer) {
				s.SetResources(ServerResource{Resource: mcp.NewResource("file:///a", "a"), Handler: read})
			},
			find: func(s *MCPServer) *requestError {
				_, reqErr := s.handleReadResource(t.Context(), 1, mcp.ReadResourceRequest{Params: mcp.ReadResourceParams{URI: "file:///a"}})
				return reqErr
			},
		},
		"prompts": {
			set: func(s *MCPServer) { s.SetPrompts(ServerPrompt{Prompt: mcp.NewPrompt("p"), Handler: get}) },
			find: func(s *MCPServer) *requestError {
				_, reqErr := s.handleGetPrompt(t.Context(), 1, mcp.GetPromptRequest{Params: mcp.GetPromptParams{Name: "p"}})
				return reqErr
			},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := NewMCPServer("test", "1.0.0", WithResourceCapabilities(false, false), WithPromptCapabilities(false))
			c.set(s)
			var notFound atomic.Int32
			stop := make(chan struct{})
			stopped := make(chan struct{})
			go func() {
				defer close(stopped)
				for {
					select {
					case <-stop:
						return
					default:
					}
					if c.find(s) != nil {
						notFound.Add(1)
					}
				}
			}()
			for range 5000 {
				c.set(s)
			}
			close(stop)
			<-stopped
			require.Zero(t, notFound.Load(), "reads found nothing while the list was replaced")
		})
	}
}
