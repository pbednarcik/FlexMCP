// conformance starts the MCP server used by the official conformance suite.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

func main() {
	addr := os.Getenv("CONFORMANCE_ADDR")
	if addr == "" {
		addr = "127.0.0.1:3000"
	}

	mcpServer := server.NewMCPServer(
		"mcp-go-conformance",
		"1.0.0",
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, true),
		server.WithPromptCapabilities(true),
		server.WithCompletions(),
		server.WithLogging(),
	)
	registerTools(mcpServer)
	registerResources(mcpServer)
	registerPrompts(mcpServer)

	httpServer := server.NewStreamableHTTPServer(mcpServer)
	mux := http.NewServeMux()
	mux.Handle("/mcp", httpServer)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	log.Printf("MCP conformance server listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func registerTools(mcpServer *server.MCPServer) {
	mcpServer.AddTool(mcp.NewTool("test_simple_text",
		mcp.WithDescription("Returns a simple text response for conformance testing")),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("This is a simple text response for testing."), nil
		})
	mcpServer.AddTool(mcp.NewTool("test_image_content",
		mcp.WithDescription("Returns image content for conformance testing")),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{mcp.NewImageContent(testImage, "image/png")}}, nil
		})
	mcpServer.AddTool(mcp.NewTool("test_audio_content",
		mcp.WithDescription("Returns audio content for conformance testing")),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{mcp.NewAudioContent(testAudio, "audio/wav")}}, nil
		})
	mcpServer.AddTool(mcp.NewTool("test_embedded_resource",
		mcp.WithDescription("Returns an embedded text resource")),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{mcp.NewEmbeddedResource(mcp.TextResourceContents{
				URI: "test://embedded-resource", MIMEType: "text/plain", Text: "This is an embedded resource content.",
			})}}, nil
		})
	mcpServer.AddTool(mcp.NewTool("test_multiple_content_types",
		mcp.WithDescription("Returns text, image, and embedded resource content")),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{
				mcp.TextContent{Type: "text", Text: "Multiple content types test:"},
				mcp.NewImageContent(testImage, "image/png"),
				mcp.NewEmbeddedResource(mcp.TextResourceContents{
					URI: "test://mixed-content-resource", MIMEType: "application/json", Text: `{"test":"data","value":123}`,
				}),
			}}, nil
		})
	mcpServer.AddTool(mcp.NewTool("test_error_handling",
		mcp.WithDescription("Returns an intentional tool error")),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultError("This tool intentionally returns an error for testing"), nil
		})

	mcpServer.AddTool(mcp.NewTool("test_tool_with_logging",
		mcp.WithDescription("Emits log messages during execution")),
		func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			for i, message := range []string{
				"Tool execution started",
				"Tool processing data",
				"Tool execution completed",
			} {
				if i > 0 {
					time.Sleep(50 * time.Millisecond)
				}
				if err := mcpServer.SendNotificationToClient(ctx, string(mcp.MethodNotificationMessage), map[string]any{
					"level":  "info",
					"logger": "conformance-test-server",
					"data":   message,
				}); err != nil {
					return nil, err
				}
			}
			return mcp.NewToolResultText("Tool with logging executed successfully"), nil
		})
	mcpServer.AddTool(mcp.NewTool("test_tool_with_progress",
		mcp.WithDescription("Reports progress notifications")),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var token mcp.ProgressToken
			if request.Params.Meta != nil {
				token = request.Params.Meta.ProgressToken
			}
			if token != nil {
				for i, progress := range []int{0, 50, 100} {
					if i > 0 {
						time.Sleep(50 * time.Millisecond)
					}
					if err := mcpServer.SendNotificationToClient(ctx, string(mcp.MethodNotificationProgress), map[string]any{
						"progressToken": token,
						"progress":      progress,
						"total":         100,
					}); err != nil {
						return nil, err
					}
				}
			}
			return mcp.NewToolResultText("progress reported"), nil
		})
}

func registerResources(mcpServer *server.MCPServer) {
	mcpServer.AddResource(mcp.NewResource("test://static-text", "static-text",
		mcp.WithResourceTitle("Static Text Resource"), mcp.WithResourceDescription("A static text resource for testing"), mcp.WithMIMEType("text/plain")),
		func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{mcp.TextResourceContents{URI: "test://static-text", MIMEType: "text/plain", Text: "This is the content of the static text resource."}}, nil
		})
	mcpServer.AddResource(mcp.NewResource("test://static-binary", "static-binary",
		mcp.WithResourceTitle("Static Binary Resource"), mcp.WithResourceDescription("A static binary resource for testing"), mcp.WithMIMEType("image/png")),
		func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{mcp.BlobResourceContents{URI: "test://static-binary", MIMEType: "image/png", Blob: testImage}}, nil
		})
	mcpServer.AddResourceTemplate(mcp.NewResourceTemplate("test://template/{id}/data", "template",
		mcp.WithTemplateDescription("A resource template with parameter substitution"), mcp.WithTemplateMIMEType("application/json")),
		func(_ context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			parts := strings.Split(request.Params.URI, "/")
			id := ""
			if len(parts) == 5 {
				id = parts[3]
			}
			return []mcp.ResourceContents{mcp.TextResourceContents{URI: request.Params.URI, MIMEType: "application/json", Text: fmt.Sprintf(`{"id":%q,"templateTest":true,"data":%q}`, id, "Data for ID: "+id)}}, nil
		})
}

func registerPrompts(mcpServer *server.MCPServer) {
	mcpServer.AddPrompt(mcp.NewPrompt("test_simple_prompt",
		mcp.WithPromptTitle("Simple Test Prompt"), mcp.WithPromptDescription("A simple prompt without arguments")),
		func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []mcp.PromptMessage{{Role: mcp.RoleUser, Content: mcp.TextContent{Type: "text", Text: "This is a simple prompt for testing."}}}}, nil
		})
	mcpServer.AddPrompt(mcp.NewPrompt("test_prompt_with_arguments",
		mcp.WithPromptTitle("Prompt With Arguments"), mcp.WithPromptDescription("A prompt with required arguments"),
		mcp.WithArgument("arg1", mcp.ArgumentDescription("First test argument"), mcp.RequiredArgument()),
		mcp.WithArgument("arg2", mcp.ArgumentDescription("Second test argument"), mcp.RequiredArgument())),
		func(_ context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			text := fmt.Sprintf("Prompt with arguments: arg1='%s', arg2='%s'", request.Params.Arguments["arg1"], request.Params.Arguments["arg2"])
			return &mcp.GetPromptResult{Messages: []mcp.PromptMessage{{Role: mcp.RoleUser, Content: mcp.TextContent{Type: "text", Text: text}}}}, nil
		})
	mcpServer.AddPrompt(mcp.NewPrompt("test_prompt_with_embedded_resource",
		mcp.WithPromptTitle("Prompt With Embedded Resource"), mcp.WithPromptDescription("A prompt that includes an embedded resource"),
		mcp.WithArgument("resourceUri", mcp.ArgumentDescription("URI of the resource to embed"), mcp.RequiredArgument())),
		func(_ context.Context, request mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			uri := request.Params.Arguments["resourceUri"]
			return &mcp.GetPromptResult{Messages: []mcp.PromptMessage{
				{Role: mcp.RoleUser, Content: mcp.NewEmbeddedResource(mcp.TextResourceContents{URI: uri, MIMEType: "text/plain", Text: "Embedded resource content for testing."})},
				{Role: mcp.RoleUser, Content: mcp.TextContent{Type: "text", Text: "Please process the embedded resource above."}},
			}}, nil
		})
	mcpServer.AddPrompt(mcp.NewPrompt("test_prompt_with_image",
		mcp.WithPromptTitle("Prompt With Image"), mcp.WithPromptDescription("A prompt that includes image content")),
		func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Messages: []mcp.PromptMessage{
				{Role: mcp.RoleUser, Content: mcp.NewImageContent(testImage, "image/png")},
				{Role: mcp.RoleUser, Content: mcp.TextContent{Type: "text", Text: "Please analyze the image above."}},
			}}, nil
		})
}

var (
	testImage = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8DwHwAFBQIAX8jx0gAAAABJRU5ErkJggg=="
	testAudio = "UklGRiYAAABXQVZFZm10IBAAAAABAAEAQB8AAAB9AAACABAAZGF0YQIAAAA="
)
