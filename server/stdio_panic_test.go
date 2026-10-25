package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStdioServer_ToolCallWorkerPanicRecovery(t *testing.T) {
	// Create a server with a tool that panics and one that works
	server := NewMCPServer("test", "1.0.0", WithToolCapabilities(true))
	server.AddTools(
		ServerTool{
			Tool: mcp.Tool{
				Name:        "panic-tool",
				Description: "A tool that panics",
			},
			Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				panic("deliberate panic in stdio handler")
			},
		},
		ServerTool{
			Tool: mcp.Tool{
				Name:        "safe-tool",
				Description: "A tool that works",
			},
			Handler: func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return mcp.NewToolResultText("ok"), nil
			},
		},
	)

	stdioServer := NewStdioServer(server)

	// Build input: initialize, then panic tool call, then safe tool call
	initMsg := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`
	panicMsg := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"panic-tool","arguments":{}}}`
	safeMsg := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"safe-tool","arguments":{}}}`

	// The client keeps stdin open until the three answers are in. A tool call
	// still queued when stdin closes is dropped, by design: the server exits
	// promptly at EOF (Transports.md row 8). Closing stdin right after writing
	// would race the workers against that, and this test is about a worker
	// surviving a panic.
	stdin, stdinWriter := io.Pipe()
	stdoutReader, stdout := io.Pipe()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	listened := make(chan error, 1)
	go func() {
		listened <- stdioServer.Listen(ctx, stdin, stdout)
		_ = stdout.Close()
	}()
	go func() {
		for _, line := range []string{initMsg, panicMsg, safeMsg} {
			if _, err := io.WriteString(stdinWriter, line+"\n"); err != nil {
				return
			}
		}
	}()

	// Parse responses
	scanner := bufio.NewScanner(stdoutReader)
	var responses []json.RawMessage
	for len(responses) < 3 && scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		responses = append(responses, json.RawMessage(line))
	}
	require.NoError(t, scanner.Err())
	_ = stdinWriter.Close()
	go func() { _, _ = io.Copy(io.Discard, stdoutReader) }()
	// Listen returns nil on EOF (input exhausted)
	require.NoError(t, <-listened)

	// Should have at least 3 responses (initialize + panic error + safe result)
	require.GreaterOrEqual(t, len(responses), 3, "expected at least 3 responses, got %d", len(responses))

	// Verify: panic produces an error response AND safe tool gets a result
	var foundPanicError, foundSafeResponse bool
	for _, resp := range responses {
		var msg struct {
			ID     any             `json:"id"`
			Result json.RawMessage `json:"result,omitempty"`
			Error  json.RawMessage `json:"error,omitempty"`
		}
		if err := json.Unmarshal(resp, &msg); err != nil {
			continue
		}

		// The panic's value stays on the server; the client gets the fixed
		// internal-error message.
		if msg.Error != nil && strings.Contains(string(msg.Error), internalErrorMessage) {
			foundPanicError = true
			assert.NotContains(t, string(msg.Error), "panic")
		}

		// Check for id=3 (safe tool response)
		if id, ok := msg.ID.(float64); ok && int(id) == 3 {
			foundSafeResponse = true
			assert.NotNil(t, msg.Result, "safe tool should have a result")
			assert.Nil(t, msg.Error, "safe tool should not have an error")
		}
	}
	assert.True(t, foundPanicError, "client should receive INTERNAL_ERROR for panicking tool call")
	assert.True(t, foundSafeResponse, "worker should survive panic and process subsequent tool calls")
}

// stdioResponses runs input through a stdio server until EOF, failing the test
// instead of crashing it if Listen panics, and returns the responses by id.
func stdioResponses(t *testing.T, stdioServer *StdioServer, input string) map[float64]json.RawMessage {
	t.Helper()
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NotPanics(t, func() {
		require.NoError(t, stdioServer.Listen(ctx, strings.NewReader(input), &output))
	})

	responses := map[float64]json.RawMessage{}
	scanner := bufio.NewScanner(&output)
	for scanner.Scan() {
		var msg struct {
			ID any `json:"id"`
		}
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &msg), "output line %q", scanner.Text())
		id, ok := msg.ID.(float64)
		require.True(t, ok, "response without a numeric id: %s", scanner.Text())
		responses[id] = json.RawMessage(scanner.Text())
	}
	return responses
}

// Notifications are handled on the read loop. A panicking notification handler
// used to take the whole server down; now it is recovered, and since a
// notification gets no response, nothing is written for it.
func TestStdioServer_NotificationHandlerPanicRecovery(t *testing.T) {
	server := NewMCPServer("test", "1.0.0")
	server.AddNotificationHandler("notifications/boom", func(context.Context, mcp.JSONRPCNotification) {
		panic("deliberate panic in notification handler")
	})

	responses := stdioResponses(t, NewStdioServer(server), strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/boom"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
	}, "\n")+"\n")

	assert.Len(t, responses, 2, "only the two requests get a response")
	assert.Contains(t, responses, float64(2), "the server should still answer after the panic")
}
