package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// returnsWithin fails the test when f does not return within two seconds, as
// when a middleware or filter lock is still held, rather than hanging it.
func returnsWithin(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: the lock is still held", what)
	}
}

// A prompt or resource middleware that panics while wrapping does not keep
// the middleware lock, and one that adds a middleware while wrapping does not
// deadlock: the chain is built outside the lock, as the tool chain is.
func TestPromptAndResourceMiddlewareChainsAreBuiltOutsideTheLock(t *testing.T) {
	promptHandler := func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []mcp.PromptMessage{}}, nil
	}
	resourceHandler := func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		return []mcp.ResourceContents{}, nil
	}
	templateHandler := func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		return []mcp.ResourceContents{}, nil
	}
	getPrompt := func(s *MCPServer) {
		_, _ = s.handleGetPrompt(t.Context(), 1, mcp.GetPromptRequest{Params: mcp.GetPromptParams{Name: "p"}})
	}
	readResource := func(uri string) func(s *MCPServer) {
		return func(s *MCPServer) {
			_, _ = s.handleReadResource(t.Context(), 1, mcp.ReadResourceRequest{Params: mcp.ReadResourceParams{URI: uri}})
		}
	}
	newServer := func(opt ServerOption) *MCPServer {
		s := NewMCPServer("test", "1.0.0", opt)
		s.AddPrompt(mcp.NewPrompt("p"), promptHandler)
		s.AddResource(mcp.NewResource("test://direct", "direct"), resourceHandler)
		s.AddResourceTemplate(mcp.NewResourceTemplate("test://items/{id}", "items"), templateHandler)
		return s
	}

	promptBug := WithPromptHandlerMiddleware(func(PromptHandlerFunc) PromptHandlerFunc { panic("wrap bug") })
	resourceBug := WithResourceHandlerMiddleware(func(ResourceHandlerFunc) ResourceHandlerFunc { panic("wrap bug") })
	addPrompt := func(s *MCPServer) {
		WithPromptHandlerMiddleware(func(next PromptHandlerFunc) PromptHandlerFunc { return next })(s)
	}
	addResource := func(s *MCPServer) {
		WithResourceHandlerMiddleware(func(next ResourceHandlerFunc) ResourceHandlerFunc { return next })(s)
	}

	tests := []struct {
		name string
		bug  ServerOption
		call func(*MCPServer)
		add  func(*MCPServer)
	}{
		{"prompt", promptBug, getPrompt, addPrompt},
		{"resource", resourceBug, readResource("test://direct"), addResource},
		{"resource template", resourceBug, readResource("test://items/1"), addResource},
	}
	for _, tt := range tests {
		t.Run(tt.name+" panicking while wrapping", func(t *testing.T) {
			s := newServer(tt.bug)
			func() {
				defer func() { require.Equal(t, "wrap bug", recover()) }()
				tt.call(s)
			}()
			returnsWithin(t, "adding a middleware after a panic while wrapping", func() { tt.add(s) })
		})
	}

	reentrant := []struct {
		name string
		opt  func(s **MCPServer) ServerOption
		call func(*MCPServer)
	}{
		{"prompt", func(s **MCPServer) ServerOption {
			var once sync.Once
			return WithPromptHandlerMiddleware(func(next PromptHandlerFunc) PromptHandlerFunc {
				once.Do(func() { addPrompt(*s) })
				return next
			})
		}, getPrompt},
		{"resource", func(s **MCPServer) ServerOption {
			var once sync.Once
			return WithResourceHandlerMiddleware(func(next ResourceHandlerFunc) ResourceHandlerFunc {
				once.Do(func() { addResource(*s) })
				return next
			})
		}, readResource("test://direct")},
		{"resource template", func(s **MCPServer) ServerOption {
			var once sync.Once
			return WithResourceHandlerMiddleware(func(next ResourceHandlerFunc) ResourceHandlerFunc {
				once.Do(func() { addResource(*s) })
				return next
			})
		}, readResource("test://items/1")},
	}
	for _, tt := range reentrant {
		t.Run(tt.name+" adding a middleware while wrapping", func(t *testing.T) {
			var s *MCPServer
			s = newServer(tt.opt(&s))
			returnsWithin(t, "a middleware added while wrapping", func() { tt.call(s) })
		})
	}
}

// A tool or prompt filter runs outside the filter lock, as the middleware
// chains are built: one that panics does not keep the lock, and one that adds
// a filter while filtering does not deadlock.
func TestToolAndPromptFiltersRunOutsideTheLock(t *testing.T) {
	addToolFilter := func(s *MCPServer) {
		WithToolFilter(func(_ context.Context, tools []mcp.Tool) []mcp.Tool { return tools })(s)
	}
	addPromptFilter := func(s *MCPServer) {
		WithPromptFilter(func(_ context.Context, prompts []mcp.Prompt) []mcp.Prompt { return prompts })(s)
	}
	tool := mcp.NewTool("t")
	prompt := mcp.NewPrompt("p")
	listTools := func(s *MCPServer) { s.filteredTools(t.Context()) }
	checkTool := func(s *MCPServer) { s.passesToolFilters(t.Context(), tool) }
	listPrompts := func(s *MCPServer) { s.filteredPrompts(t.Context()) }
	checkPrompt := func(s *MCPServer) { s.passesPromptFilters(t.Context(), prompt) }
	toolFilter := func(f func()) ServerOption {
		return WithToolFilter(func(_ context.Context, tools []mcp.Tool) []mcp.Tool { f(); return tools })
	}
	promptFilter := func(f func()) ServerOption {
		return WithPromptFilter(func(_ context.Context, prompts []mcp.Prompt) []mcp.Prompt { f(); return prompts })
	}
	newServer := func(opt ServerOption) *MCPServer {
		s := NewMCPServer("test", "1.0.0", opt)
		s.AddTool(tool, func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ran"), nil
		})
		s.AddPrompt(prompt, func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{}, nil
		})
		return s
	}

	tests := []struct {
		name   string
		filter func(func()) ServerOption
		call   func(*MCPServer)
		add    func(*MCPServer)
	}{
		{"tool list", toolFilter, listTools, addToolFilter},
		{"tool call", toolFilter, checkTool, addToolFilter},
		{"prompt list", promptFilter, listPrompts, addPromptFilter},
		{"prompt get", promptFilter, checkPrompt, addPromptFilter},
	}
	for _, tt := range tests {
		t.Run(tt.name+" panicking", func(t *testing.T) {
			s := newServer(tt.filter(func() { panic("filter bug") }))
			func() {
				defer func() { require.Equal(t, "filter bug", recover()) }()
				tt.call(s)
			}()
			returnsWithin(t, "adding a filter after a panicking filter", func() { tt.add(s) })
		})
		t.Run(tt.name+" adding a filter while filtering", func(t *testing.T) {
			var s *MCPServer
			var once sync.Once
			s = newServer(tt.filter(func() { once.Do(func() { tt.add(s) }) }))
			returnsWithin(t, "a filter added while filtering", func() { tt.call(s) })
		})
	}
}
