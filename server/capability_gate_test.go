package server

import (
	"context"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// A capability a server gains at runtime, by its first tool, resource or
// prompt, is checked by the dispatch while it is being registered: requests
// in flight read the gate as AddTool writes it. Run with -race.
func TestCapabilityGateIsSafeAgainstRegistration(t *testing.T) {
	add := map[string]func(*MCPServer){
		"tools/list": func(s *MCPServer) {
			s.AddTool(mcp.NewTool("late"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultText("ok"), nil
			})
		},
		"resources/list": func(s *MCPServer) {
			s.AddResource(mcp.NewResource("test://late", "late"), func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
				return nil, nil
			})
		},
		"prompts/list": func(s *MCPServer) {
			s.AddPrompt(mcp.NewPrompt("late"), func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				return &mcp.GetPromptResult{}, nil
			})
		},
	}
	for method, register := range add {
		t.Run(method, func(t *testing.T) {
			for range 20 {
				s := NewMCPServer("race", "1.0.0")
				message := []byte(`{"jsonrpc":"2.0","id":1,"method":"` + method + `"}`)
				var wg sync.WaitGroup
				wg.Go(func() {
					for range 20 {
						s.HandleMessage(t.Context(), message)
					}
				})
				register(s)
				wg.Wait()
			}
		})
	}
}
