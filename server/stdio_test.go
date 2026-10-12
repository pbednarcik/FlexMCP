package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestStdioServer(t *testing.T) {
	t.Run("Can instantiate", func(t *testing.T) {
		mcpServer := NewMCPServer("test", "1.0.0")
		stdioServer := NewStdioServer(mcpServer)

		if stdioServer.server == nil {
			t.Error("MCPServer should not be nil")
		}
		if stdioServer.errLogger == nil {
			t.Error("errLogger should not be nil")
		}
	})

	t.Run("Can send and receive messages", func(t *testing.T) {
		// Create pipes for stdin and stdout
		stdinReader, stdinWriter := io.Pipe()
		stdoutReader, stdoutWriter := io.Pipe()

		// Create server
		mcpServer := NewMCPServer("test", "1.0.0",
			WithResourceCapabilities(true, true),
		)
		stdioServer := NewStdioServer(mcpServer)
		stdioServer.SetErrorLogger(log.New(io.Discard, "", 0))

		// Create context with cancel
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		// Create error channel to catch server errors
		serverErrCh := make(chan error, 1)

		// Start server in goroutine
		go func() {
			err := stdioServer.Listen(ctx, stdinReader, stdoutWriter)
			if err != nil && err != io.EOF && err != context.Canceled {
				serverErrCh <- err
			}
			stdoutWriter.Close()
			close(serverErrCh)
		}()

		// Create test message
		initRequest := map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "initialize",
			"params": map[string]any{
				"protocolVersion": "2024-11-05",
				"clientInfo": map[string]any{
					"name":    "test-client",
					"version": "1.0.0",
				},
			},
		}

		// Send request
		requestBytes, err := json.Marshal(initRequest)
		if err != nil {
			t.Fatal(err)
		}
		_, err = stdinWriter.Write(append(requestBytes, '\n'))
		if err != nil {
			t.Fatal(err)
		}

		// Read response
		scanner := bufio.NewScanner(stdoutReader)
		if !scanner.Scan() {
			t.Fatal("failed to read response")
		}
		responseBytes := scanner.Bytes()

		var response map[string]any
		if err := json.Unmarshal(responseBytes, &response); err != nil {
			t.Fatalf("failed to unmarshal response: %v", err)
		}

		// Verify response structure
		if response["jsonrpc"] != "2.0" {
			t.Errorf("expected jsonrpc version 2.0, got %v", response["jsonrpc"])
		}
		if response["id"].(float64) != 1 {
			t.Errorf("expected id 1, got %v", response["id"])
		}
		if response["error"] != nil {
			t.Errorf("unexpected error in response: %v", response["error"])
		}
		if response["result"] == nil {
			t.Error("expected result in response")
		}

		// Clean up
		cancel()
		stdinWriter.Close()

		// Check for server errors
		if err := <-serverErrCh; err != nil {
			t.Errorf("unexpected server error: %v", err)
		}
	})

	t.Run("Can use a custom context function", func(t *testing.T) {
		// Use a custom context key to store a test value.
		type testContextKey struct{}
		testValFromContext := func(ctx context.Context) string {
			val := ctx.Value(testContextKey{})
			if val == nil {
				return ""
			}
			return val.(string)
		}
		// Create a context function that sets a test value from the environment.
		// In real life this could be used to send configuration in a similar way,
		// or from a config file.
		const testEnvVar = "TEST_ENV_VAR"
		setTestValFromEnv := func(ctx context.Context) context.Context {
			return context.WithValue(ctx, testContextKey{}, os.Getenv(testEnvVar))
		}
		t.Setenv(testEnvVar, "test_value")

		// Create pipes for stdin and stdout
		stdinReader, stdinWriter := io.Pipe()
		stdoutReader, stdoutWriter := io.Pipe()

		// Create server
		mcpServer := NewMCPServer("test", "1.0.0")
		// Add a tool which uses the context function.
		mcpServer.AddTool(mcp.NewTool("test_tool"), func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// Note this is agnostic to the transport type i.e. doesn't know about request headers.
			testVal := testValFromContext(ctx)
			return mcp.NewToolResultText(testVal), nil
		})
		stdioServer := NewStdioServer(mcpServer)
		stdioServer.SetErrorLogger(log.New(io.Discard, "", 0))
		stdioServer.SetContextFunc(setTestValFromEnv)

		// Create context with cancel
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		// Create error channel to catch server errors
		serverErrCh := make(chan error, 1)

		// Start server in goroutine
		go func() {
			err := stdioServer.Listen(ctx, stdinReader, stdoutWriter)
			if err != nil && err != io.EOF && err != context.Canceled {
				serverErrCh <- err
			}
			stdoutWriter.Close()
			close(serverErrCh)
		}()

		// Create test message
		initRequest := map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "initialize",
			"params": map[string]any{
				"protocolVersion": "2024-11-05",
				"clientInfo": map[string]any{
					"name":    "test-client",
					"version": "1.0.0",
				},
			},
		}

		// Send request
		requestBytes, err := json.Marshal(initRequest)
		if err != nil {
			t.Fatal(err)
		}
		_, err = stdinWriter.Write(append(requestBytes, '\n'))
		if err != nil {
			t.Fatal(err)
		}

		// Read response
		scanner := bufio.NewScanner(stdoutReader)
		if !scanner.Scan() {
			t.Fatal("failed to read response")
		}
		responseBytes := scanner.Bytes()

		var response map[string]any
		if err := json.Unmarshal(responseBytes, &response); err != nil {
			t.Fatalf("failed to unmarshal response: %v", err)
		}

		// Verify response structure
		if response["jsonrpc"] != "2.0" {
			t.Errorf("expected jsonrpc version 2.0, got %v", response["jsonrpc"])
		}
		if response["id"].(float64) != 1 {
			t.Errorf("expected id 1, got %v", response["id"])
		}
		if response["error"] != nil {
			t.Errorf("unexpected error in response: %v", response["error"])
		}
		if response["result"] == nil {
			t.Error("expected result in response")
		}

		// Call the tool.
		toolRequest := map[string]any{
			"jsonrpc": "2.0",
			"id":      2,
			"method":  "tools/call",
			"params": map[string]any{
				"name": "test_tool",
			},
		}
		requestBytes, err = json.Marshal(toolRequest)
		if err != nil {
			t.Fatalf("Failed to marshal tool request: %v", err)
		}

		_, err = stdinWriter.Write(append(requestBytes, '\n'))
		if err != nil {
			t.Fatal(err)
		}

		if !scanner.Scan() {
			t.Fatal("failed to read response")
		}
		responseBytes = scanner.Bytes()

		response = map[string]any{}
		if err := json.Unmarshal(responseBytes, &response); err != nil {
			t.Fatalf("failed to unmarshal response: %v", err)
		}

		if response["jsonrpc"] != "2.0" {
			t.Errorf("Expected jsonrpc 2.0, got %v", response["jsonrpc"])
		}
		if response["id"].(float64) != 2 {
			t.Errorf("Expected id 2, got %v", response["id"])
		}
		if response["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"] != "test_value" {
			t.Errorf("Expected result 'test_value', got %v", response["result"])
		}
		if response["error"] != nil {
			t.Errorf("Expected no error, got %v", response["error"])
		}

		// Clean up
		cancel()
		stdinWriter.Close()

		// Check for server errors
		if err := <-serverErrCh; err != nil {
			t.Errorf("unexpected server error: %v", err)
		}
	})

	t.Run("Can handle concurrent tool calls", func(t *testing.T) {
		// Create pipes for stdin and stdout
		stdinReader, stdinWriter := io.Pipe()
		stdoutReader, stdoutWriter := io.Pipe()

		// Track tool call executions (sync.Map is already thread-safe)
		var callCount sync.Map

		// Create server with test tools
		mcpServer := NewMCPServer("test", "1.0.0")

		// Add multiple tools that simulate work and track concurrent execution
		for i := range 5 {
			toolName := fmt.Sprintf("test_tool_%d", i)
			mcpServer.AddTool(
				mcp.NewTool(toolName),
				func(name string) func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
						// Track concurrent executions
						count, _ := callCount.LoadOrStore(name, 0)
						callCount.Store(name, count.(int)+1)

						// Simulate some work
						time.Sleep(10 * time.Millisecond)

						return mcp.NewToolResultText(fmt.Sprintf("Result from %s", name)), nil
					}
				}(toolName),
			)
		}

		stdioServer := NewStdioServer(mcpServer)
		stdioServer.SetErrorLogger(log.New(io.Discard, "", 0))

		// Create context with cancel
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		// Start server
		serverErrCh := make(chan error, 1)
		go func() {
			err := stdioServer.Listen(ctx, stdinReader, stdoutWriter)
			if err != nil && err != io.EOF && err != context.Canceled {
				serverErrCh <- err
			}
			stdoutWriter.Close()
			close(serverErrCh)
		}()

		// Initialize the session
		initRequest := map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "initialize",
			"params": map[string]any{
				"protocolVersion": "2024-11-05",
				"clientInfo": map[string]any{
					"name":    "test-client",
					"version": "1.0.0",
				},
			},
		}

		requestBytes, _ := json.Marshal(initRequest)
		if _, err := stdinWriter.Write(append(requestBytes, '\n')); err != nil {
			t.Fatalf("Failed to write init request: %v", err)
		}

		// Read init response
		scanner := bufio.NewScanner(stdoutReader)
		scanner.Scan()

		// Send multiple concurrent tool calls
		var wg sync.WaitGroup
		responseChan := make(chan string, 10)

		// Send 10 concurrent tool calls
		for i := range 10 {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()

				toolRequest := map[string]any{
					"jsonrpc": "2.0",
					"id":      id + 2,
					"method":  "tools/call",
					"params": map[string]any{
						"name": fmt.Sprintf("test_tool_%d", id%5),
					},
				}

				requestBytes, _ := json.Marshal(toolRequest)
				if _, err := stdinWriter.Write(append(requestBytes, '\n')); err != nil {
					t.Errorf("Failed to write tool request %d: %v", id, err)
				}
			}(i)
		}

		// Read all responses
		go func() {
			for range 10 {
				if scanner.Scan() {
					responseChan <- scanner.Text()
				}
			}
			close(responseChan)
		}()

		// Wait for all requests to be sent
		wg.Wait()

		// Collect responses
		responses := make([]string, 0, 10)
		timeout := time.After(2 * time.Second)

	collectLoop:
		for len(responses) < 10 {
			select {
			case resp, ok := <-responseChan:
				if !ok {
					break collectLoop
				}
				responses = append(responses, resp)
			case <-timeout:
				t.Fatal("Timeout waiting for responses")
			}
		}
		// Verify we got all responses
		if len(responses) != 10 {
			t.Errorf("Expected 10 responses, got %d", len(responses))
		}

		// Verify no errors in responses
		for _, resp := range responses {
			var response map[string]any
			if err := json.Unmarshal([]byte(resp), &response); err != nil {
				t.Errorf("Failed to unmarshal response: %v", err)
				continue
			}

			if response["error"] != nil {
				t.Errorf("Unexpected error in response: %v", response["error"])
			}

			// Verify response has expected structure
			if response["result"] == nil {
				t.Error("Expected result in response")
			}
		}

		// Verify tools were called
		callCount.Range(func(key, value any) bool {
			toolName := key.(string)
			count := value.(int)
			if count == 0 {
				t.Errorf("Tool %s was not called", toolName)
			}
			return true
		})

		// Clean up
		cancel()
		stdinWriter.Close()

		// Check for server errors
		if err := <-serverErrCh; err != nil {
			t.Errorf("Server error: %v", err)
		}
	})

	t.Run("Configuration options respect bounds", func(t *testing.T) {
		mcpServer := NewMCPServer("test", "1.0.0")

		// Test worker pool size bounds
		stdioServer := NewStdioServer(mcpServer)
		WithWorkerPoolSize(150)(stdioServer)
		if stdioServer.workerPoolSize != 100 { // Should use maximum
			t.Errorf("Expected maximum worker pool size 100, got %d", stdioServer.workerPoolSize)
		}

		// Test valid worker pool size
		stdioServer = NewStdioServer(mcpServer)
		WithWorkerPoolSize(50)(stdioServer)
		if stdioServer.workerPoolSize != 50 {
			t.Errorf("Expected worker pool size 50, got %d", stdioServer.workerPoolSize)
		}

		// Test queue size bounds
		stdioServer = NewStdioServer(mcpServer)
		WithQueueSize(20000)(stdioServer)
		if stdioServer.queueSize != 10000 { // Should use maximum
			t.Errorf("Expected maximum queue size 10000, got %d", stdioServer.queueSize)
		}

		// Test valid queue size
		stdioServer = NewStdioServer(mcpServer)
		WithQueueSize(500)(stdioServer)
		if stdioServer.queueSize != 500 {
			t.Errorf("Expected queue size 500, got %d", stdioServer.queueSize)
		}

		// Test zero and negative values
		stdioServer = NewStdioServer(mcpServer)
		WithWorkerPoolSize(0)(stdioServer)
		WithQueueSize(-10)(stdioServer)
		if stdioServer.workerPoolSize != 5 {
			t.Errorf("Expected default worker pool size 5 for zero input, got %d", stdioServer.workerPoolSize)
		}
		if stdioServer.queueSize != 100 {
			t.Errorf("Expected default queue size 100 for negative input, got %d", stdioServer.queueSize)
		}
	})
}

func TestStdioServeRequestsDuringSubscriptionsListen(t *testing.T) {
	// subscriptions/listen holds its stream open for the life of the
	// subscription. Dispatching it on the read loop would stall every later
	// request, so the server must keep serving while it is open.
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()

	mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(true))
	mcpServer.AddTool(mcp.NewTool("ping"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("pong"), nil
	})

	stdioServer := NewStdioServer(mcpServer)
	stdioServer.SetErrorLogger(log.New(io.Discard, "", 0))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = stdioServer.Listen(ctx, stdinReader, stdoutWriter) }()

	responses := make(chan map[string]any, 4)
	go func() {
		scanner := bufio.NewScanner(stdoutReader)
		for scanner.Scan() {
			var msg map[string]any
			if json.Unmarshal(scanner.Bytes(), &msg) == nil {
				responses <- msg
			}
		}
	}()

	meta := map[string]any{
		"io.modelcontextprotocol/protocolVersion":    mcp.ProtocolVersion20260728,
		"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "test-client", "version": "1.0.0"},
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
	send := func(id any, method string, params map[string]any) {
		params["_meta"] = meta
		b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if err != nil {
			t.Errorf("marshalling %s: %v", method, err)
			return
		}
		// Written from a goroutine so a stalled read loop fails the test by
		// timeout rather than blocking it forever on the unbuffered pipe.
		go func() { _, _ = stdinWriter.Write(append(b, '\n')) }()
	}

	// Wait for a message matching want, ignoring anything else on the stream.
	await := func(want func(map[string]any) bool, describe string) map[string]any {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case msg := <-responses:
				if want(msg) {
					return msg
				}
			case <-deadline:
				t.Fatalf("timed out waiting for %s", describe)
			}
		}
	}

	send("listen-1", string(mcp.MethodSubscriptionsListen), map[string]any{
		"notifications": map[string]any{"toolsListChanged": true},
	})
	await(func(m map[string]any) bool {
		return m["method"] == string(mcp.MethodNotificationSubscriptionsAcknowledged)
	}, "subscriptions/listen acknowledgement")

	send(2, string(mcp.MethodToolsList), map[string]any{})
	listed := await(func(m map[string]any) bool {
		id, ok := m["id"].(float64)
		return ok && id == 2
	}, "tools/list response while the subscription stream is open")

	require.Nil(t, listed["error"], "unexpected error in tools/list response")
	require.NotNil(t, listed["result"], "expected a result in the tools/list response")
}

func TestStdioJoinsRequestHandlersOnEOF(t *testing.T) {
	// EOF does not cancel the caller's context, so Listen must release its own
	// request handlers and wait for them. Otherwise a handler that blocks - as
	// subscriptions/listen does for the life of its stream - outlives the
	// session it was registered against.
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})

	mcpServer := NewMCPServer("test", "1.0.0", WithResourceCapabilities(false, false))
	mcpServer.AddResource(
		mcp.NewResource("test://blocking", "blocking"),
		func(ctx context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			close(started)
			<-ctx.Done()
			close(cancelled)
			// Stay in the handler so the test can tell a Listen that waits from
			// one that merely cancels.
			<-release
			return nil, ctx.Err()
		},
	)

	stdinReader, stdinWriter := io.Pipe()

	stdioServer := NewStdioServer(mcpServer)
	stdioServer.SetErrorLogger(log.New(io.Discard, "", 0))

	// The caller's context stays live for the whole test, as it would in a
	// long-running host: only EOF should release the handler.
	listenDone := make(chan error, 1)
	go func() { listenDone <- stdioServer.Listen(t.Context(), stdinReader, io.Discard) }()

	request, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  string(mcp.MethodResourcesRead),
		"params":  map[string]any{"uri": "test://blocking"},
	})
	require.NoError(t, err)
	go func() { _, _ = stdinWriter.Write(append(request, '\n')) }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("resource handler never started")
	}

	// The client goes away.
	require.NoError(t, stdinWriter.Close())

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("Listen did not cancel the request handler on EOF")
	}

	select {
	case err := <-listenDone:
		t.Fatalf("Listen returned while the request handler was still running: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-listenDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Listen did not return once the request handler finished")
	}
}

// TestStdioSendsNothingForACancelledRequest pins Transports.md row 6: after a
// notifications/cancelled for an in-flight request, the stdio server sends
// nothing more for that request, not even the error its handler returns on
// the cancelled context.
func TestStdioSendsNothingForACancelledRequest(t *testing.T) {
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()

	started := make(chan struct{})
	returned := make(chan struct{})
	mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	mcpServer.AddTool(mcp.NewTool("block"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(started)
		<-ctx.Done()
		close(returned)
		return nil, ctx.Err()
	})
	mcpServer.AddTool(mcp.NewTool("ping"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("pong"), nil
	})

	stdioServer := NewStdioServer(mcpServer)
	stdioServer.SetErrorLogger(log.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = stdioServer.Listen(ctx, stdinReader, stdoutWriter) }()

	responses := make(chan map[string]any, 8)
	go func() {
		scanner := bufio.NewScanner(stdoutReader)
		for scanner.Scan() {
			var msg map[string]any
			if json.Unmarshal(scanner.Bytes(), &msg) == nil {
				responses <- msg
			}
		}
	}()
	send := func(line string) {
		go func() { _, _ = stdinWriter.Write([]byte(line + "\n")) }()
	}

	send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"block"}}`)
	<-started
	send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the cancel did not end the handler")
	}

	// A later request is answered; nothing for request 7 arrives before it.
	send(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"ping"}}`)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case msg := <-responses:
			switch msg["id"] {
			case float64(7):
				t.Fatalf("the cancelled request was answered: %v", msg)
			case float64(8):
				require.Nil(t, msg["error"], "request 8: %v", msg)
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for request 8")
		}
	}
}

// stdioPipes starts a stdio server on pipes for the life of the test and
// returns a sender for stdin lines and a channel of the JSON messages written
// to stdout.
func stdioPipes(t *testing.T, mcpServer *MCPServer) (send func(line string), out <-chan map[string]any) {
	t.Helper()
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	stdioServer := NewStdioServer(mcpServer)
	stdioServer.SetErrorLogger(log.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() { _ = stdioServer.Listen(ctx, stdinReader, stdoutWriter) }()
	messages := make(chan map[string]any, 64)
	go func() {
		scanner := bufio.NewScanner(stdoutReader)
		for scanner.Scan() {
			var msg map[string]any
			if json.Unmarshal(scanner.Bytes(), &msg) == nil {
				messages <- msg
			}
		}
	}()
	// One writer goroutine keeps the lines in the order they were sent, which
	// a goroutine per line does not; it is off the test's goroutine so a
	// stalled read loop fails a test by timeout rather than blocking it.
	lines := make(chan string, 64)
	go func() {
		for line := range lines {
			if _, err := stdinWriter.Write([]byte(line + "\n")); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { close(lines) })
	return func(line string) { lines <- line }, messages
}

// awaitMessage returns the next message matching want, skipping others.
func awaitMessage(t *testing.T, out <-chan map[string]any, want func(map[string]any) bool, describe string) map[string]any {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case msg := <-out:
			if want(msg) {
				return msg
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", describe)
			return nil
		}
	}
}

// TestStdioServersInOneProcessAreIndependent pins that each stdio server owns
// its session: a notification one MCPServer sends reaches the stdout of the
// stdio server serving it, and never another's.
func TestStdioServersInOneProcessAreIndependent(t *testing.T) {
	noisy := NewMCPServer("noisy", "1.0.0", WithToolCapabilities(true))
	quiet := NewMCPServer("quiet", "1.0.0", WithToolCapabilities(true))
	sendNoisy, outNoisy := stdioPipes(t, noisy)
	_, outQuiet := stdioPipes(t, quiet)

	// A handshake-era client: once initialized, every broadcast reaches it.
	sendNoisy(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	awaitMessage(t, outNoisy, func(m map[string]any) bool { return m["id"] == float64(1) }, "the initialize result")
	sendNoisy(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	const changes = 20
	for i := range changes {
		noisy.AddTool(mcp.NewTool(fmt.Sprintf("tool-%d", i)), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})
	}
	isListChanged := func(m map[string]any) bool { return m["method"] == string(mcp.MethodNotificationToolsListChanged) }
	for i := range changes {
		awaitMessage(t, outNoisy, isListChanged, fmt.Sprintf("tools/list_changed %d on the noisy server's stdout", i+1))
	}
	select {
	case msg := <-outQuiet:
		t.Fatalf("the quiet server's stdout carried another server's message: %v", msg)
	default:
	}
}

// modernMetaJSON is the _meta every modern request carries, as a JSON object.
const modernMetaJSON = `{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}`

// TestStdioListenStreamsAreFilteredAndTagged pins the stdio side of
// subscriptions/listen (Transports.md row 5, Patterns.md rows 14 and 17): a
// modern client may hold several streams on the one stdio channel, each
// receives only the types it asked for, every notification carries its
// stream's id, and a cancelled stream receives nothing more.
func TestStdioListenStreamsAreFilteredAndTagged(t *testing.T) {
	mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(true), WithPromptCapabilities(true))
	send, out := stdioPipes(t, mcpServer)
	isAck := func(m map[string]any) bool {
		return m["method"] == string(mcp.MethodNotificationSubscriptionsAcknowledged)
	}
	isMethod := func(method mcp.MCPMethod) func(map[string]any) bool {
		return func(m map[string]any) bool { return m["method"] == string(method) }
	}
	subscriptionID := func(m map[string]any) any {
		params, _ := m["params"].(map[string]any)
		meta, _ := params["_meta"].(map[string]any)
		return meta[mcp.MetaKeySubscriptionID]
	}
	addTool := func(name string) {
		mcpServer.AddTool(mcp.NewTool(name), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})
	}

	send(`{"jsonrpc":"2.0","id":"listen-1","method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true},"_meta":` + modernMetaJSON + `}}`)
	ack := awaitMessage(t, out, isAck, "the first acknowledgement")
	require.Equal(t, "listen-1", subscriptionID(ack))

	addTool("one")
	changed := awaitMessage(t, out, isMethod(mcp.MethodNotificationToolsListChanged), "tools/list_changed on the first stream")
	require.Equal(t, "listen-1", subscriptionID(changed), "a listen-stream notification carries its stream's id")

	send(`{"jsonrpc":"2.0","id":"listen-2","method":"subscriptions/listen","params":{"notifications":{"promptsListChanged":true},"_meta":` + modernMetaJSON + `}}`)
	ack = awaitMessage(t, out, isAck, "the second acknowledgement")
	require.Equal(t, "listen-2", subscriptionID(ack))

	mcpServer.AddPrompt(mcp.NewPrompt("greeting"), func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{}, nil
	})
	changed = awaitMessage(t, out, isMethod(mcp.MethodNotificationPromptsListChanged), "prompts/list_changed on the second stream")
	require.Equal(t, "listen-2", subscriptionID(changed), "only the stream that asked for prompts receives them")

	// Ending the first stream ends its deliveries; a request after the next
	// change proves nothing arrived for it.
	send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"listen-1"}}`)
	send(`{"jsonrpc":"2.0","id":5,"method":"tools/list","params":{"_meta":` + modernMetaJSON + `}}`)
	awaitMessage(t, out, func(m map[string]any) bool { return m["id"] == float64(5) }, "the tools/list after the cancel")
	addTool("two")
	send(`{"jsonrpc":"2.0","id":6,"method":"tools/list","params":{"_meta":` + modernMetaJSON + `}}`)
	for {
		msg := awaitMessage(t, out, func(map[string]any) bool { return true }, "the tools/list after the second change")
		if msg["id"] == float64(6) {
			break
		}
		require.NotEqual(t, string(mcp.MethodNotificationToolsListChanged), msg["method"], "a cancelled stream still received %v", msg)
	}
}

// TestStdioBroadcastsReachOnlyWhoAskedForThem pins that a modern stdio client
// with no listen stream receives no broadcast, while a handshake-era client
// receives them as before, untagged.
func TestStdioBroadcastsReachOnlyWhoAskedForThem(t *testing.T) {
	addTool := func(mcpServer *MCPServer, name string) {
		mcpServer.AddTool(mcp.NewTool(name), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})
	}
	isListChanged := func(m map[string]any) bool { return m["method"] == string(mcp.MethodNotificationToolsListChanged) }

	t.Run("a modern client without a stream", func(t *testing.T) {
		mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(true))
		send, out := stdioPipes(t, mcpServer)
		send(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":` + modernMetaJSON + `}}`)
		awaitMessage(t, out, func(m map[string]any) bool { return m["id"] == float64(1) }, "the first tools/list")
		addTool(mcpServer, "late")
		send(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{"_meta":` + modernMetaJSON + `}}`)
		for {
			msg := awaitMessage(t, out, func(map[string]any) bool { return true }, "the second tools/list")
			if msg["id"] == float64(2) {
				return
			}
			require.False(t, isListChanged(msg), "a broadcast reached a client that never asked: %v", msg)
		}
	})

	t.Run("a handshake-era client", func(t *testing.T) {
		mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(true))
		send, out := stdioPipes(t, mcpServer)
		send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
		awaitMessage(t, out, func(m map[string]any) bool { return m["id"] == float64(1) }, "the initialize result")
		send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		addTool(mcpServer, "late")
		changed := awaitMessage(t, out, isListChanged, "tools/list_changed for the legacy client")
		params, _ := changed["params"].(map[string]any)
		require.Nil(t, params["_meta"], "a legacy broadcast carries no subscription id")
	})
}

// The end of input is the shutdown: a tool call still queued then is not
// started. Listen cancels the context its workers share at EOF, so a worker
// that finds it cancelled with a call in the queue drops the call.
func TestStdioServer_AQueuedToolCallDoesNotStartAfterTheInputEnds(t *testing.T) {
	started := 0
	mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(true))
	mcpServer.AddTool(mcp.NewTool("count"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		started++
		return mcp.NewToolResultText("ok"), nil
	})
	stdioServer := NewStdioServer(mcpServer)
	ended, cancel := context.WithCancel(t.Context())
	cancel()

	// A worker picks at random between a queued call and the cancellation;
	// each round gives it that choice once more.
	for round := range 64 {
		var output bytes.Buffer
		stdioServer.toolCallQueue = make(chan *toolCallWork, 1)
		stdioServer.toolCallQueue <- &toolCallWork{
			ctx:     ended,
			id:      round,
			message: json.RawMessage(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"count"}}`, round)),
			writer:  &output,
		}
		stdioServer.workerWg.Add(1)
		stdioServer.toolCallWorker(ended)
		require.Zero(t, started, "round %d: a queued call started after the input ended", round)
		require.Zero(t, output.Len(), "round %d: a queued call was answered after the input ended: %s", round, output.String())
	}
}

// A tool call the client cancels while it waits in the queue is not started
// (a receiver of a cancellation should stop processing the request): the
// cancel finds no handler to stop yet, so the worker drops the call.
func TestStdioServer_AToolCallCancelledWhileQueuedIsNotStarted(t *testing.T) {
	started := 0
	mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(true))
	mcpServer.AddTool(mcp.NewTool("count"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		started++
		return mcp.NewToolResultText("ok"), nil
	})
	stdioServer := NewStdioServer(mcpServer)
	id := int64(7)
	stdioServer.cancelled.start(id)
	stdioServer.cancelled.cancel(id)
	var output bytes.Buffer
	stdioServer.toolCallQueue = make(chan *toolCallWork, 1)
	stdioServer.toolCallQueue <- &toolCallWork{
		ctx:     t.Context(),
		id:      id,
		message: json.RawMessage(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"count"}}`),
		writer:  &output,
	}
	close(stdioServer.toolCallQueue)

	stdioServer.workerWg.Add(1)
	stdioServer.toolCallWorker(t.Context())
	require.Zero(t, started, "a call cancelled while queued was started")
	require.Zero(t, output.Len(), "a call cancelled while queued was answered: %s", output.String())
}

// A tool call that finds the queue full is refused at once, not run on the
// read loop: the loop keeps reading, so the end of input, the shutdown, is
// seen however many lines are still waiting, and the pool and the queue
// bound the calls in flight.
func TestStdioServer_ACallPastAFullQueueIsRefused(t *testing.T) {
	const overflow = 100 // more than the reader holds ahead of the loop
	started := make(chan string, overflow+2)
	mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(true))
	for _, name := range []string{"busy", "queued", "overflow"} {
		mcpServer.AddTool(mcp.NewTool(name), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			started <- name
			<-ctx.Done()
			return nil, ctx.Err()
		})
	}
	stdioServer := NewStdioServer(mcpServer)
	WithWorkerPoolSize(1)(stdioServer)
	WithQueueSize(1)(stdioServer)
	stdioServer.SetErrorLogger(log.New(io.Discard, "", 0))

	stdin, input := io.Pipe()
	var output bytes.Buffer
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	listened := make(chan error, 1)
	go func() { listened <- stdioServer.Listen(ctx, stdin, &output) }()
	call := func(id int, name string) error {
		_, err := fmt.Fprintf(input, `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q}}`+"\n", id, name)
		return err
	}

	require.NoError(t, call(1, "busy"))
	require.Equal(t, "busy", <-started)
	// The rest is written on its own goroutine: a server that stops reading
	// would block the writes, and the test must still reach its deadline.
	go func() {
		defer input.Close()
		if call(2, "queued") != nil {
			return
		}
		for id := range overflow {
			if call(3+id, "overflow") != nil {
				return
			}
		}
	}()

	select {
	case err := <-listened:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("the end of input was not seen; Listen is still running")
	}
	close(started)
	for name := range started {
		t.Errorf("%s started after the only worker was taken", name)
	}
	refused := 0
	scanner := bufio.NewScanner(&output)
	for scanner.Scan() {
		var response mcp.JSONRPCError
		if json.Unmarshal(scanner.Bytes(), &response) == nil && response.Error.Code == mcp.INTERNAL_ERROR &&
			strings.HasPrefix(response.Error.Message, "server busy") {
			refused++
		}
	}
	require.Equal(t, overflow, refused, "every call past the full queue is refused")
}

// A tools/call without an id is a notification, and a notification is never
// answered (JSON-RPC 2.0, section 4.1), not even with a refusal when the queue
// is full.
func TestStdioServer_AnOverflowedNotificationIsNotAnswered(t *testing.T) {
	started := make(chan struct{}, 3)
	mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(true))
	mcpServer.AddTool(mcp.NewTool("hold"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	stdioServer := NewStdioServer(mcpServer)
	WithWorkerPoolSize(1)(stdioServer)
	WithQueueSize(1)(stdioServer)
	stdioServer.SetErrorLogger(log.New(io.Discard, "", 0))

	stdin, input := io.Pipe()
	var output bytes.Buffer
	listened := make(chan error, 1)
	go func() { listened <- stdioServer.Listen(t.Context(), stdin, &output) }()

	_, err := fmt.Fprintln(input, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"hold"}}`)
	require.NoError(t, err)
	<-started
	go func() {
		defer input.Close()
		if _, err := fmt.Fprintln(input, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"hold"}}`); err != nil {
			return
		}
		_, _ = fmt.Fprintln(input, `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"hold"}}`)
	}()

	select {
	case err := <-listened:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("the end of input was not seen; Listen is still running")
	}
	require.NotContains(t, output.String(), "server busy", "a notification was answered")
}
