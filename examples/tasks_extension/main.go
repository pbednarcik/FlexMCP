// Command tasks_extension demonstrates the Tasks extension
// (io.modelcontextprotocol/tasks) of protocol version 2026-07-28.
//
// A tool that runs long hands its call over as a task instead of holding the
// request. The client then polls the task with tasks/get, and may cancel it.
// The page www/docs/pages/servers/tasks.mdx quotes this code.
//
// The demo runs two servers over an in-process transport: one whose tasks live
// in a server.MemoryTasks, and one whose tasks live in a registry of its own
// (jobs.go). With -legacy it serves the 2025-11-25 task tools (legacy.go) on
// stdio instead.
//
// Run it with:
//
//	go run ./examples/tasks_extension
package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/pbednarcik/flexmcp/client"
	"github.com/pbednarcik/flexmcp/client/transport"
	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
)

func main() {
	legacy := flag.Bool("legacy", false, "serve the 2025-11-25 task tools on stdio instead of running the demo")
	flag.Parse()

	if *legacy {
		if err := server.ServeStdio(newLegacyServer()); err != nil {
			log.Fatalf("serving: %v", err)
		}
		return
	}

	ctx := context.Background()

	tasks := server.NewMemoryTasks()
	defer tasks.Close()

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		log.Fatalf("making a task key: %v", err)
	}

	if err := runReport(ctx, newServer(tasks)); err != nil {
		log.Fatalf("report: %v", err)
	}
	if err := runExport(ctx, newGuardedServer(key)); err != nil {
		log.Fatalf("export: %v", err)
	}
}

// newServer serves a "report" tool whose work runs as a task, kept by tasks.
func newServer(tasks *server.MemoryTasks) *server.MCPServer {
	srv := server.NewMCPServer("report-server", "1.0.0",
		server.WithToolCapabilities(true),
		server.WithTaskHandler(tasks),
	)

	srv.AddTool(
		mcp.NewTool("report",
			mcp.WithDescription("builds a report, which takes a while"),
			mcp.WithString("name", mcp.Required(), mcp.Description("the report to build")),
		),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			name := request.GetString("name", "")

			// Run starts the work on its own goroutine and answers with the task.
			// When the client cannot take a task, it runs the work right here and
			// answers with its result.
			return tasks.Run(ctx, func(ctx context.Context, status func(string)) (*mcp.CallToolResult, error) {
				for step := 1; step <= 3; step++ {
					status(fmt.Sprintf("step %d of 3", step))
					select {
					case <-time.After(300 * time.Millisecond):
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
				return mcp.NewToolResultText("report " + name + " is ready"), nil
			},
				server.WithTaskResultPollInterval(time.Second),
				server.WithTaskResultTTL(10*time.Minute),
			)
		},
	)

	return srv
}

// connect connects a client that declares the Tasks extension to srv. A client
// that does not declare it is never handed a task.
func connect(ctx context.Context, srv *server.MCPServer) (*client.Client, error) {
	c := client.NewClient(
		transport.NewInProcessTransport(srv),
		client.WithClientCapabilities(mcp.ClientCapabilities{
			Extensions: map[string]any{mcp.ExtensionTasks: map[string]any{}},
		}),
	)
	if err := c.Start(ctx); err != nil {
		return nil, fmt.Errorf("starting client: %w", err)
	}

	var initRequest mcp.InitializeRequest
	initRequest.Params.ClientInfo = mcp.Implementation{Name: "tasks-example", Version: "1.0.0"}
	if _, err := c.Initialize(ctx, initRequest); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("connecting: %w", err)
	}
	return c, nil
}

// runReport calls "report" and waits for its task to end.
func runReport(ctx context.Context, srv *server.MCPServer) error {
	c, err := connect(ctx, srv)
	if err != nil {
		return err
	}
	defer c.Close()

	var call mcp.CallToolRequest
	call.Params.Name = "report"
	call.Params.Arguments = map[string]any{"name": "weekly"}

	result, err := c.CallTool(ctx, call)
	if err != nil {
		return fmt.Errorf("calling report: %w", err)
	}
	if result.Task == nil {
		// The server answered at once.
		printResult(result)
		return nil
	}
	fmt.Printf("task %s is %s\n", result.Task.TaskId, result.Task.Status)

	state, err := awaitTask(ctx, c, result.Task.TaskId)
	if err != nil {
		return err
	}
	switch state.Status {
	case mcp.TaskStatusCompleted:
		printResult(state.TaskResult)
		return nil
	case mcp.TaskStatusFailed:
		return fmt.Errorf("the task failed: %s", state.Error.Message)
	default:
		return fmt.Errorf("the task ended %s", state.Status)
	}
}

// awaitTask waits for a task to end, answering the input it asks for on the
// way.
func awaitTask(ctx context.Context, c *client.Client, taskID string) (*mcp.ExtensionTaskStateResult, error) {
	for {
		// WaitForExtensionTask polls tasks/get at the interval the server asked
		// for, never faster than every 250 ms.
		state, err := c.WaitForExtensionTask(ctx, taskID, time.Second)
		if err != nil {
			return nil, fmt.Errorf("waiting for the task: %w", err)
		}
		if state.Status != mcp.TaskStatusInputRequired {
			return state, nil
		}
		if err := answerInput(ctx, c, taskID, state); err != nil {
			return nil, fmt.Errorf("answering the task: %w", err)
		}
	}
}

// runExport starts "export" and cancels its task.
func runExport(ctx context.Context, srv *server.MCPServer) error {
	c, err := connect(ctx, srv)
	if err != nil {
		return err
	}
	defer c.Close()

	var call mcp.CallToolRequest
	call.Params.Name = "export"

	result, err := c.CallTool(ctx, call)
	if err != nil {
		return fmt.Errorf("calling export: %w", err)
	}
	if result.Task == nil {
		printResult(result)
		return nil
	}
	return cancelAndCheck(ctx, c, result.Task.TaskId)
}

// cancelAndCheck asks for the task to stop and reads its state back: cancelling
// only asks, and the handler decides what becomes of the task.
func cancelAndCheck(ctx context.Context, c *client.Client, taskID string) error {
	if err := c.CancelExtensionTask(ctx, taskID); err != nil {
		return fmt.Errorf("cancelling the task: %w", err)
	}

	state, err := c.GetExtensionTask(ctx, taskID)
	switch {
	case mcp.IsTaskGone(err):
		// The task ended long ago, or the server restarted: call the tool again.
		fmt.Println("the task is gone")
		return nil
	case err != nil:
		return fmt.Errorf("reading the task: %w", err)
	}
	fmt.Printf("task %s is %s\n", taskID, state.Status)
	return nil
}

// answerInput answers the input requests of a task that is input_required.
// WaitForExtensionTask returns such a task to its caller, which answers with
// UpdateExtensionTask and waits again. A task kept by MemoryTasks never asks;
// a TaskHandler that is also a TaskInputHandler can.
func answerInput(ctx context.Context, c *client.Client, taskID string, state *mcp.ExtensionTaskStateResult) error {
	responses := mcp.InputResponses{}
	for key, request := range state.InputRequests {
		if request.Elicitation == nil {
			continue
		}
		responses[key] = mcp.NewElicitationInputResponse(mcp.ElicitationResult{
			Action:  mcp.ElicitationResponseActionAccept,
			Content: map[string]any{"approve": true},
		})
	}
	return c.UpdateExtensionTask(ctx, taskID, responses)
}

// printResult prints the text of a tool result.
func printResult(result *mcp.CallToolResult) {
	for _, content := range result.Content {
		if text, ok := content.(mcp.TextContent); ok {
			fmt.Println(text.Text)
		}
	}
}
