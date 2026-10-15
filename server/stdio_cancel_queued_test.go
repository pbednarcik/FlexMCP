package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// gatedWriter is a stdout whose writes wait at a gate the test opens; the
// first write entered is announced, so the test knows the writer is held.
type gatedWriter struct {
	gate    chan struct{}
	writing chan struct{}
	w       io.Writer
}

func (g *gatedWriter) Write(p []byte) (int, error) {
	select {
	case g.writing <- struct{}{}:
	default:
	}
	<-g.gate
	return g.w.Write(p)
}

// gatedStdio starts a stdio server for mcpServer on pipes, with a gated
// stdout, for the life of the test: it returns a sender for stdin lines
// (synchronous: the line is in the read loop's hands when it returns), the
// JSON messages written to stdout, the gate, and the server itself.
func gatedStdio(t *testing.T, mcpServer *MCPServer) (send func(line string), out <-chan map[string]any, stdout *gatedWriter, stdioServer *StdioServer) {
	t.Helper()
	stdioServer = NewStdioServer(mcpServer)
	stdioServer.SetErrorLogger(log.New(io.Discard, "", 0))
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	stdout = &gatedWriter{gate: make(chan struct{}), writing: make(chan struct{}, 1), w: stdoutWriter}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() { _ = stdioServer.Listen(ctx, stdinReader, stdout) }()
	messages := make(chan map[string]any, 16)
	go func() {
		scanner := bufio.NewScanner(stdoutReader)
		for scanner.Scan() {
			var msg map[string]any
			if json.Unmarshal(scanner.Bytes(), &msg) == nil {
				messages <- msg
			}
		}
	}()
	send = func(line string) {
		_, err := io.WriteString(stdinWriter, line+"\n")
		require.NoError(t, err)
	}
	return send, messages, stdout, stdioServer
}

// A cancel that arrives after a request's response was queued, but before
// the writer wrote it, still drops the response: nothing is sent for a
// cancelled request (Transports.md row 6), and the queue is no exception.
func TestStdioDropsAQueuedResponseOnCancel(t *testing.T) {
	mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	mcpServer.AddTool(mcp.NewTool("quick"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("done"), nil
	})
	send, out, stdout, stdioServer := gatedStdio(t, mcpServer)

	// The writer is held on the ping's response; the call's response is
	// queued behind it when the cancel arrives, and request 8 after that.
	send(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	<-stdout.writing
	send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"quick"}}`)
	require.Eventually(t, func() bool { return len(stdioServer.responses) == 1 }, 3*time.Second, time.Millisecond)
	send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`)
	send(`{"jsonrpc":"2.0","id":8,"method":"ping"}`)
	require.Eventually(t, func() bool { return len(stdioServer.responses) == 2 }, 3*time.Second, time.Millisecond)
	close(stdout.gate)

	deadline := time.After(5 * time.Second)
	for {
		select {
		case msg := <-out:
			switch msg["id"] {
			case float64(7):
				t.Fatalf("the cancelled request was answered from the queue: %v", msg)
			case float64(8):
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for request 8")
		}
	}
}

// A cancelled request's response, queued behind a held write, is still
// dropped when the client reuses the id meanwhile, as it may once it has
// cancelled (BaseProtocol.md row 4): the tracker tells the two requests on
// the id apart, so the client sees one answer for it, the new request's.
func TestStdioDropsACancelledResponseAfterItsIDIsReused(t *testing.T) {
	entered := make(chan struct{}, 1)
	mcpServer := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	mcpServer.AddTool(mcp.NewTool("wait"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return mcp.NewToolResultText("the cancelled call's answer"), nil
	})
	send, out, stdout, stdioServer := gatedStdio(t, mcpServer)

	send(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	<-stdout.writing
	send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"wait"}}`)
	<-entered
	send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`)
	require.Eventually(t, func() bool { return len(stdioServer.responses) == 1 }, 3*time.Second, time.Millisecond, "the cancelled call's response was not queued")
	send(`{"jsonrpc":"2.0","id":7,"method":"ping"}`)
	require.Eventually(t, func() bool { return len(stdioServer.responses) == 2 }, 3*time.Second, time.Millisecond, "the reused id's response was not queued")
	close(stdout.gate)
	send(`{"jsonrpc":"2.0","id":9,"method":"ping"}`)

	answers := 0
	deadline := time.After(5 * time.Second)
	for {
		select {
		case msg := <-out:
			switch msg["id"] {
			case float64(7):
				answers++
				require.NotContains(t, fmt.Sprint(msg["result"]), "cancelled call", "the cancelled call's response was written as the reused id's")
			case float64(9):
				require.Equal(t, 1, answers, "id 7 was not answered exactly once")
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for request 9")
		}
	}
}
