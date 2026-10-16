package transport

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Close ends the handlers still serving requests the server sent: their
// context is cancelled, as the SSE and Streamable HTTP transports do, rather
// than left running for a transport that will never send their answer.
func TestStdio_CloseStopsTheHandlersOfServerRequests(t *testing.T) {
	serverOut, toClient := io.Pipe()
	stdio := NewIO(serverOut, nopWriteCloser{Writer: io.Discard}, nil)
	entered, stopped := make(chan struct{}), make(chan struct{})
	stdio.SetRequestHandler(func(ctx context.Context, _ JSONRPCRequest) (*JSONRPCResponse, error) {
		close(entered)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	})
	require.NoError(t, stdio.Start(t.Context()))
	t.Cleanup(func() { _ = toClient.Close() })

	go func() {
		_, _ = fmt.Fprintln(toClient, `{"jsonrpc":"2.0","id":1,"method":"roots/list"}`)
	}()
	<-entered
	require.NoError(t, stdio.Close())

	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler was still running after Close")
	}
}
