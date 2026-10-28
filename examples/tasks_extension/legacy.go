package main

import (
	"context"
	"log"
	"time"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
)

// newLegacyServer serves a task tool of protocol version 2025-11-25, where the
// client asks for a task with params.task and the server runs the tool in the
// background. A 2026-07-28 request never gets one of these.
func newLegacyServer() *server.MCPServer {
	taskHooks := &server.TaskHooks{}

	taskHooks.AddOnTaskCreated(func(ctx context.Context, metrics server.TaskMetrics) {
		log.Printf("Task %s created for tool %s", metrics.TaskID, metrics.ToolName)
	})

	taskHooks.AddOnTaskCompleted(func(ctx context.Context, metrics server.TaskMetrics) {
		log.Printf("Task %s completed in %v", metrics.TaskID, metrics.Duration)
	})

	taskHooks.AddOnTaskFailed(func(ctx context.Context, metrics server.TaskMetrics) {
		log.Printf("Task %s failed: %v", metrics.TaskID, metrics.Error)
	})

	taskHooks.AddOnTaskCancelled(func(ctx context.Context, metrics server.TaskMetrics) {
		log.Printf("Task %s was cancelled", metrics.TaskID)
	})

	taskHooks.AddOnTaskStatusChanged(func(ctx context.Context, metrics server.TaskMetrics) {
		log.Printf("Task %s status: %s", metrics.TaskID, metrics.Status)
	})

	s := server.NewMCPServer("legacy-tasks", "1.0.0",
		server.WithToolCapabilities(true),
		server.WithTaskCapabilities(true, true, true), // list, cancel, toolCallTasks
		server.WithTaskHooks(taskHooks),
		server.WithMaxConcurrentTasks(10),
		server.WithTaskTTL(time.Hour),
	)

	tool := mcp.NewTool("long_process",
		mcp.WithDescription("Run a long process asynchronously"),
		mcp.WithTaskSupport(mcp.TaskSupportRequired),
		mcp.WithString("input", mcp.Required(), mcp.Description("Input data")),
	)

	s.AddTaskTool(tool, longProcess)

	// A regular tool with TaskSupportOptional runs as a task only when the
	// client asks for one with params.task, and synchronously otherwise.
	optional := mcp.NewTool("quick_or_task",
		mcp.WithDescription("Answers at once, or as a task when asked to"),
		mcp.WithTaskSupport(mcp.TaskSupportOptional),
	)

	s.AddTool(optional, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("done"), nil
	})

	return s
}

// longProcess runs in the background. The server makes the task and its id; what
// the handler returns is the result the client fetches with tasks/result.
func longProcess(ctx context.Context, req mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
	input := req.GetString("input", "")

	// Check ctx.Done() to support cancellation.
	for range 100 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}

	return &mcp.CreateTaskResult{
		Content: []mcp.Content{mcp.NewTextContent("processed " + input)},
	}, nil
}
