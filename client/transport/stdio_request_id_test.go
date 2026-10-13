package transport

import (
	"bufio"
	"bytes"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// lockedBuffer is a bytes.Buffer safe for a logger and a test to share.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A request id is a string or an integer and never null (MCP base protocol):
// a server request whose id cannot be read is answered as an invalid request
// with a null id (JSON-RPC 2.0, section 5), and a response without a readable
// id, which no pending request can claim, is logged rather than lost.
func TestStdio_AMessageWithAnUnreadableIDIsNotIgnored(t *testing.T) {
	fromServer, serverOut := io.Pipe()
	toServer, clientOut := io.Pipe()
	var logs lockedBuffer
	trans := NewIO(fromServer, clientOut, io.NopCloser(strings.NewReader("")))
	trans.logger = slog.New(slog.NewTextHandler(&logs, nil))
	require.NoError(t, trans.Start(t.Context()))
	t.Cleanup(func() { _ = trans.Close(); _ = serverOut.Close() })

	replies := make(chan string, 2)
	go func() {
		reader := bufio.NewReader(toServer)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			replies <- strings.TrimSpace(line)
		}
	}()

	for _, line := range []string{
		`{"jsonrpc":"2.0","id":1.5,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request id"}}`,
	} {
		_, err := io.WriteString(serverOut, line+"\n")
		require.NoError(t, err)
	}

	for range 2 {
		select {
		case reply := <-replies:
			assert.JSONEq(t, `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request id"}}`, reply)
		case <-time.After(2 * time.Second):
			t.Fatal("a server request with an unreadable id was not answered")
		}
	}
	require.Eventually(t, func() bool { return strings.Contains(logs.String(), "Invalid request id") },
		2*time.Second, 10*time.Millisecond, "an error with a null id was dropped without a trace")
}

// A server request is never a response, whatever its method: one named "",
// or one whose method is not a string, carrying the id of the client's pending
// request is answered, and the pending request gets its own response.
func TestStdio_AServerRequestWithAnyMethodIsNotAResponse(t *testing.T) {
	fromServer, serverOut := io.Pipe()
	toServer, clientOut := io.Pipe()
	trans := NewIO(fromServer, clientOut, io.NopCloser(strings.NewReader("")))
	trans.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	require.NoError(t, trans.Start(t.Context()))
	t.Cleanup(func() { _ = trans.Close(); _ = serverOut.Close() })

	replies := make(chan string, 3)
	go func() {
		reader := bufio.NewReader(toServer)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			replies <- strings.TrimSpace(line)
		}
	}()

	type result struct {
		response *JSONRPCResponse
		err      error
	}
	done := make(chan result, 1)
	go func() {
		response, err := trans.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Method: "tools/list"})
		done <- result{response, err}
	}()
	select {
	case <-replies:
	case <-time.After(2 * time.Second):
		t.Fatal("the request was not sent")
	}
	for _, line := range []string{
		`{"jsonrpc":"2.0","id":1,"method":""}`,
		`{"jsonrpc":"2.0","id":1,"method":null,"result":{}}`,
		`{"jsonrpc":"2.0","id":1,"result":{"answer":42}}`,
	} {
		_, err := io.WriteString(serverOut, line+"\n")
		require.NoError(t, err)
	}

	select {
	case got := <-done:
		require.NoError(t, got.err)
		assert.JSONEq(t, `{"answer":42}`, string(got.response.Result))
	case <-time.After(2 * time.Second):
		t.Fatal("the pending request got no response")
	}
	for _, want := range []string{
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"No request handler configured"}}`,
		`{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"Invalid request: method must be a string"}}`,
	} {
		select {
		case reply := <-replies:
			assert.JSONEq(t, want, reply)
		case <-time.After(2 * time.Second):
			t.Fatalf("no answer; want %s", want)
		}
	}
}
