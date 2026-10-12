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
