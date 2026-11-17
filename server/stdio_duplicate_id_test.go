package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pbednarcik/flexmcp/mcp"
)

// A request on the id of a tool call still waiting in the queue is refused,
// repeating the id, and the queued call is served when its turn comes
// (BaseProtocol.md row 4): the id is taken when the call is read, not when a
// worker picks it up.
func TestStdioServer_ADuplicateOfAQueuedCallIsRefused(t *testing.T) {
	started, release := make(chan string, 4), make(chan struct{})
	send, out := stdioWithWorkers(t, blockingTools(started, release), 1, nil)
	send(toolCallLine(10, "block"))
	awaitStarted(t, started, "block")
	send(toolCallLine(11, "queued"))
	send(`{"jsonrpc":"2.0","id":11,"method":"ping"}`)

	refusal := awaitID(t, out, "11", "the answer to the duplicate on id 11")
	if !isInFlightRefusal(refusal) {
		t.Fatalf("the duplicate of a queued call got %s, want the in-flight refusal", refusal.raw)
	}
	noneOnID(t, out, "11", 200*time.Millisecond, "id 11 was answered again before its call ran")
	close(release)
	result := awaitID(t, out, "11", "the queued call's result")
	if result.Error != nil || !strings.Contains(string(result.Result), "done") {
		t.Fatalf("the queued call got %s, want its result", result.raw)
	}
}

// A refused duplicate leaves the id to the call it duplicates, so a cancel
// naming the id still reaches that call and nothing more is sent for it
// (Transports.md row 6), whether the call is running or still queued.
func TestStdioServer_ARefusedDuplicateLeavesTheCancelToTheCall(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		started, ended := make(chan string, 4), make(chan struct{})
		mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(true))
		mcpServer.AddTool(mcp.NewTool("wait"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			started <- "wait"
			<-ctx.Done()
			close(ended)
			return mcp.NewToolResultText("late"), nil
		})
		send, out := stdioWithWorkers(t, mcpServer, 2, nil)
		send(toolCallLine(20, "wait"))
		awaitStarted(t, started, "wait")
		send(`{"jsonrpc":"2.0","id":20,"method":"ping"}`)
		if refusal := awaitID(t, out, "20", "the answer to the duplicate on id 20"); !isInFlightRefusal(refusal) {
			t.Fatalf("the duplicate of a running call got %s, want the in-flight refusal", refusal.raw)
		}

		send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":20}}`)
		select {
		case <-ended:
		case <-time.After(3 * time.Second):
			t.Fatal("the cancel never reached the running call")
		}
		noneOnID(t, out, "20", 300*time.Millisecond, "the cancelled call was answered")
	})

	t.Run("queued", func(t *testing.T) {
		started, release := make(chan string, 4), make(chan struct{})
		send, out := stdioWithWorkers(t, blockingTools(started, release), 1, nil)
		send(toolCallLine(10, "block"))
		awaitStarted(t, started, "block")
		send(toolCallLine(12, "queued"))
		send(`{"jsonrpc":"2.0","id":12,"method":"ping"}`)
		send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":12}}`)
		// The read loop takes the cancel before the ping behind it, so the
		// ping's answer says the cancel was taken before the worker is freed.
		send(`{"jsonrpc":"2.0","id":14,"method":"ping"}`)
		if refusal := awaitID(t, out, "12", "the answer to the duplicate on id 12"); !isInFlightRefusal(refusal) {
			t.Fatalf("the duplicate of a queued call got %s, want the in-flight refusal", refusal.raw)
		}
		awaitID(t, out, "14", "the ping behind the cancel")

		// The one worker takes the queue in order: the marker call starting
		// next says the cancelled call was dropped without starting.
		send(toolCallLine(13, "marker"))
		close(release)
		awaitStarted(t, started, "marker")
		noneOnID(t, out, "12", 300*time.Millisecond, "the cancelled queued call was answered")
	})
}

// A cancel frees the id at once: a request reusing it is served while the
// cancelled call still runs or waits, and the cancelled call's response is
// dropped.
func TestStdioServer_ACancelFreesTheIDAtOnce(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprintf("queued=%v", queued), func(t *testing.T) {
			started, stubborn := make(chan string, 4), make(chan struct{})
			release := sync.OnceFunc(func() { close(stubborn) })
			defer release()
			mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(true))
			for _, name := range []string{"stubborn", "marker"} {
				mcpServer.AddTool(mcp.NewTool(name), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					started <- name
					<-stubborn
					return mcp.NewToolResultText(name), nil
				})
			}
			workers := 2
			if queued {
				workers = 1
			}
			send, out := stdioWithWorkers(t, mcpServer, workers, nil)
			if queued {
				send(toolCallLine(1, "stubborn"))
				awaitStarted(t, started, "stubborn")
			}
			send(toolCallLine(30, "stubborn"))
			if !queued {
				awaitStarted(t, started, "stubborn")
			}
			send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":30}}`)
			send(`{"jsonrpc":"2.0","id":30,"method":"ping"}`)
			if reuse := awaitID(t, out, "30", "the answer to the request reusing id 30"); reuse.Error != nil || string(reuse.Result) != "{}" {
				t.Fatalf("the ping reusing a cancelled id got %s, want its result", reuse.raw)
			}

			send(toolCallLine(31, "marker"))
			release()
			awaitStarted(t, started, "marker")
			noneOnID(t, out, "30", 300*time.Millisecond, "the cancelled call was answered")
		})
	}
}

// The error hooks see the stdio server's refusal of a duplicate as they see
// the core's, with ErrRequestIDInFlight and the request's id, and no request
// hook runs for the refused duplicate.
func TestStdioServer_TheRefusalReachesTheErrorHooks(t *testing.T) {
	var mu sync.Mutex
	var errs []error
	var ids []any
	initialized := 0
	hooks := &Hooks{}
	hooks.AddOnError(func(_ context.Context, id any, _ mcp.MCPMethod, _ any, err error) {
		mu.Lock()
		defer mu.Unlock()
		errs, ids = append(errs, err), append(ids, id)
	})
	hooks.AddOnRequestInitialization(func(context.Context, any, any) error {
		mu.Lock()
		defer mu.Unlock()
		initialized++
		return nil
	})
	started, release := make(chan string, 4), make(chan struct{})
	send, out := stdioWithWorkers(t, blockingTools(started, release, WithHooks(hooks)), 1, nil)
	send(toolCallLine(10, "block"))
	awaitStarted(t, started, "block")
	send(toolCallLine(11, "queued"))
	send(`{"jsonrpc":"2.0","id":11,"method":"ping"}`)
	answer := awaitID(t, out, "11", "the answer to the duplicate on id 11")

	mu.Lock()
	defer mu.Unlock()
	if len(errs) != 1 || !errors.Is(errs[0], ErrRequestIDInFlight) || fmt.Sprint(ids[0]) != "11" {
		t.Fatalf("OnError saw %v for ids %v, want ErrRequestIDInFlight once for id 11 (the duplicate got %s)", errs, ids, answer.raw)
	}
	if initialized != 1 {
		t.Fatalf("OnRequestInitialization ran %d times, want once, for the running call", initialized)
	}
}

// A client that stops reading cannot stall the read loop by sending
// duplicates: their refusals wait for the writer off the loop, and a cancel
// sent after them still reaches the call.
func TestStdioServer_RefusalsDoNotStallTheReadLoop(t *testing.T) {
	started, cancelled := make(chan string, 1), make(chan struct{})
	mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(true))
	mcpServer.AddTool(mcp.NewTool("wait"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		started <- "wait"
		<-ctx.Done()
		close(cancelled)
		return nil, ctx.Err()
	})
	stdout := blockedWriter{unblock: make(chan struct{})}
	send, _ := stdioWithWorkers(t, mcpServer, 1, stdout)
	t.Cleanup(func() { close(stdout.unblock) })
	send(toolCallLine(1, "wait"))
	awaitStarted(t, started, "wait")
	for range 40 {
		send(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("the cancel never reached the call: the read loop is stalled")
	}
}

// stdioLine is one message a stdio server wrote to stdout.
type stdioLine struct {
	raw    string
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// stdioWithWorkers starts a stdio server with the given number of tool call
// workers for the life of the test. It writes to stdout, or, when stdout is
// nil, to a pipe whose messages it returns. The sender it returns never
// blocks the test on a stalled read loop.
func stdioWithWorkers(t *testing.T, mcpServer *MCPServer, workers int, stdout io.Writer) (send func(line string), out <-chan stdioLine) {
	t.Helper()
	stdioServer := NewStdioServer(mcpServer)
	WithWorkerPoolSize(workers)(stdioServer)
	stdioServer.SetErrorLogger(log.New(io.Discard, "", 0))
	messages := make(chan stdioLine, 256)
	if stdout == nil {
		stdoutReader, stdoutWriter := io.Pipe()
		stdout = stdoutWriter
		t.Cleanup(func() { _ = stdoutReader.Close() })
		go func() {
			scanner := bufio.NewScanner(stdoutReader)
			for scanner.Scan() {
				line := stdioLine{raw: scanner.Text()}
				if json.Unmarshal(scanner.Bytes(), &line) == nil {
					messages <- line
				}
			}
		}()
	}
	stdinReader, stdinWriter := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	listened := make(chan struct{})
	go func() {
		defer close(listened)
		_ = stdioServer.Listen(ctx, stdinReader, stdout)
	}()
	lines := make(chan string, 256)
	go func() {
		for line := range lines {
			if _, err := stdinWriter.Write([]byte(line + "\n")); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = stdinReader.Close()
		<-listened
		close(lines)
	})
	return func(line string) { lines <- line }, messages
}

// blockingTools is a server whose tools "block", "queued" and "marker" each
// report their start, then return once release is closed or their context
// ends.
func blockingTools(started chan<- string, release <-chan struct{}, opts ...ServerOption) *MCPServer {
	mcpServer := NewMCPServer("test", "1.0.0", append([]ServerOption{WithToolCapabilities(true)}, opts...)...)
	for _, name := range []string{"block", "queued", "marker"} {
		mcpServer.AddTool(mcp.NewTool(name), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			started <- name
			select {
			case <-release:
				return mcp.NewToolResultText("done"), nil
			case <-ctx.Done():
				return mcp.NewToolResultText("late"), nil
			}
		})
	}
	return mcpServer
}

// toolCallLine is a tools/call request on id for the tool name.
func toolCallLine(id int, name string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q}}`, id, name)
}

// awaitStarted fails the test unless the next tool to start is want.
func awaitStarted(t *testing.T, started <-chan string, want string) {
	t.Helper()
	select {
	case name := <-started:
		if name != want {
			t.Fatalf("tool %q started, want %q", name, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for tool %q to start", want)
	}
}

// awaitID returns the next message on id, skipping others.
func awaitID(t *testing.T, out <-chan stdioLine, id, describe string) stdioLine {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case line := <-out:
			if string(line.ID) == id {
				return line
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", describe)
			return stdioLine{}
		}
	}
}

// noneOnID fails the test when a message on id arrives within window: an
// absence, which no poll can wait for.
func noneOnID(t *testing.T, out <-chan stdioLine, id string, window time.Duration, describe string) {
	t.Helper()
	deadline := time.After(window)
	for {
		select {
		case line := <-out:
			if string(line.ID) == id {
				t.Fatalf("%s: %s", describe, line.raw)
			}
		case <-deadline:
			return
		}
	}
}

// isInFlightRefusal reports whether line is the refusal of a request whose id
// is in flight already.
func isInFlightRefusal(line stdioLine) bool {
	return line.Error != nil && line.Error.Code == mcp.INVALID_REQUEST && line.Error.Message == ErrRequestIDInFlight.Error()
}
