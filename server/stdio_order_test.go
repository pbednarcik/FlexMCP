package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// A request's notifications reach stdout before its response: progress
// stops once the request completes (Patterns.md row 34), so none of it may
// land after the response that completes it.
func TestStdioWritesARequestsNotificationsBeforeItsResponse(t *testing.T) {
	const progressCount = 50
	mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	mcpServer.AddTool(mcp.NewTool("progress"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		for i := range progressCount {
			_ = mcpServer.SendNotificationToClient(ctx, "notifications/progress", map[string]any{"progress": i})
		}
		return mcp.NewToolResultText("done"), nil
	})
	send, out := stdioPipes(t, mcpServer)
	initializeStdio(t, send, out)

	for call := range 5 {
		id := float64(100 + call)
		send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"progress"}}`, 100+call))
		seen := 0
		for {
			msg := awaitMessage(t, out, func(map[string]any) bool { return true }, "the next line")
			if msg["id"] == id {
				require.Equal(t, progressCount, seen, "call %d: its response came before all of its progress", call)
				break
			}
			if msg["method"] == "notifications/progress" {
				seen++
			}
		}
	}
}

// initializeStdio completes the handshake of a handshake-era client, after
// which the session's notifications reach stdout.
func initializeStdio(t *testing.T, send func(line string), out <-chan map[string]any) {
	t.Helper()
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	awaitMessage(t, out, func(m map[string]any) bool { return m["id"] == float64(1) }, "the initialize result")
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}

// A response is not starved by another request's notifications: the writer
// writes the notifications queued before the response, not every one that
// keeps coming while it does.
func TestStdioAnswersWhileAnotherRequestFloodsNotifications(t *testing.T) {
	stop := make(chan struct{})
	mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	mcpServer.AddTool(mcp.NewTool("flood"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		for {
			select {
			case <-stop:
				return mcp.NewToolResultText("done"), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			default:
				_ = mcpServer.SendNotificationToClient(ctx, "notifications/message", map[string]any{"level": "info", "data": "x"})
			}
		}
	})
	send, out := stdioPipes(t, mcpServer)
	initializeStdio(t, send, out)

	send(`{"jsonrpc":"2.0","id":100,"method":"tools/call","params":{"name":"flood"}}`)
	awaitMessage(t, out, func(m map[string]any) bool { return m["method"] == "notifications/message" }, "the flood")
	send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	awaitMessage(t, out, func(m map[string]any) bool { return m["id"] == float64(2) }, "the ping's response during the flood")
	close(stop)
	awaitMessage(t, out, func(m map[string]any) bool { return m["id"] == float64(100) }, "the flooding tool's response")
}

// blockedWriter is a stdout whose client never reads: a write never returns.
type blockedWriter struct{ unblock chan struct{} }

func (w blockedWriter) Write(p []byte) (int, error) { <-w.unblock; return len(p), nil }

// A cancelled Listen returns while stdout is blocked: the writer stuck on a
// client that stopped reading does not hold it.
func TestStdioListenEndsOnCancelWhileStdoutIsBlocked(t *testing.T) {
	stdioServer := NewStdioServer(NewMCPServer("test", "1.0.0"))
	stdioServer.SetErrorLogger(log.New(io.Discard, "", 0))
	stdout := blockedWriter{unblock: make(chan struct{})}
	t.Cleanup(func() { close(stdout.unblock) })
	stdinReader, stdinWriter := io.Pipe()
	t.Cleanup(func() { _ = stdinWriter.Close() })
	stdioServer.session.notifications <- mcp.JSONRPCNotification{JSONRPC: mcp.JSONRPC_VERSION, Method: "notifications/message"}

	ctx, cancel := context.WithCancel(t.Context())
	returned := make(chan error, 1)
	go func() { returned <- stdioServer.Listen(ctx, stdinReader, stdout) }()
	cancel()
	select {
	case err := <-returned:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("Listen was held by a client that stopped reading")
	}
}

// A cancelled Listen returns while handlers wait to queue their responses
// behind a blocked stdout: the queue is given up on cancel too.
func TestStdioListenEndsOnCancelWithResponsesWaitingToQueue(t *testing.T) {
	const requests = 40 // more than the response queue holds
	var served atomic.Int32
	hooks := &Hooks{}
	hooks.AddBeforeAny(func(context.Context, any, mcp.MCPMethod, any) { served.Add(1) })
	stdioServer := NewStdioServer(NewMCPServer("test", "1.0.0", WithHooks(hooks), WithToolCapabilities(false)))
	stdioServer.SetErrorLogger(log.New(io.Discard, "", 0))
	stdout := blockedWriter{unblock: make(chan struct{})}
	t.Cleanup(func() { close(stdout.unblock) })
	stdinReader, stdinWriter := io.Pipe()
	t.Cleanup(func() { _ = stdinWriter.Close() })

	ctx, cancel := context.WithCancel(t.Context())
	returned := make(chan error, 1)
	go func() { returned <- stdioServer.Listen(ctx, stdinReader, stdout) }()
	go func() {
		for i := range requests {
			line := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/list","params":{"_meta":%s}}`+"\n", i, modernMetaJSON)
			if _, err := io.WriteString(stdinWriter, line); err != nil {
				return
			}
		}
	}()
	require.Eventually(t, func() bool { return served.Load() == requests }, 3*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case err := <-returned:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("Listen was held by handlers waiting to queue their responses")
	}
}

// brokenStdout is a stdout whose every write fails.
type brokenStdout struct{ err error }

func (w brokenStdout) Write([]byte) (int, error) { return 0, w.err }

// A failed write to stdout ends Listen, which returns the error: the client
// is gone, and nothing more could reach it. A parse error's answer and a
// request's response fail the same way.
func TestStdioListenReturnsTheWriteError(t *testing.T) {
	broken := errors.New("stdout is broken")
	for _, line := range []string{"{\n", `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"_meta":` + modernMetaJSON + `}}` + "\n"} {
		stdioServer := NewStdioServer(NewMCPServer("test", "1.0.0"))
		stdioServer.SetErrorLogger(log.New(io.Discard, "", 0))
		stdinReader, stdinWriter := io.Pipe()
		t.Cleanup(func() { _ = stdinWriter.Close() })
		go func() { _, _ = io.WriteString(stdinWriter, line) }()
		returned := make(chan error, 1)
		go func() { returned <- stdioServer.Listen(t.Context(), stdinReader, brokenStdout{err: broken}) }()
		select {
		case err := <-returned:
			require.ErrorIs(t, err, broken, "line %q", line)
		case <-time.After(3 * time.Second):
			t.Fatalf("Listen went on after stdout failed, line %q", line)
		}
	}
}
