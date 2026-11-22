package server

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// On stdio, a cancellation whose member names only fold onto the defined
// ones is refused as the core refuses it: it cancels nothing, and the request
// is answered. One spelled as defined cancels it.
func TestStdioCancelsOnlyOnACancellationSpelledAsDefined(t *testing.T) {
	for line, cancels := range map[string]bool{
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`:               true,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"RequestId":7}}`:               false,
		`{"jsonrpc":"2.0","Method":"notifications/cancelled","params":{"requestId":7}}`:               false,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7,"requestId":7}}`: false,
	} {
		t.Run(line, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			handlerErr := make(chan error, 1)
			srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
			srv.AddTool(mcp.NewTool("slow"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				close(started)
				select {
				case <-ctx.Done():
				case <-release:
				}
				handlerErr <- ctx.Err()
				return mcp.NewToolResultText("done"), nil
			})
			send, out := stdioPipes(t, srv)
			send(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"slow"}}`)
			<-started
			send(line)
			// A cancellation reaches the handler at once; give a refused one
			// the time it would take, then let the handler finish.
			var err error
			select {
			case err = <-handlerErr:
			case <-time.After(200 * time.Millisecond):
				close(release)
				err = <-handlerErr
			}
			send(`{"jsonrpc":"2.0","id":8,"method":"ping"}`)
			answered7 := false
			for done := false; !done; {
				select {
				case msg := <-out:
					switch msg["id"] {
					case float64(7):
						answered7 = true
					case float64(8):
						done = true
					}
				case <-time.After(5 * time.Second):
					t.Fatal("no answer to the ping")
				}
			}
			if cancels {
				require.ErrorIs(t, err, context.Canceled)
				assert.False(t, answered7, "a cancelled request is not answered")
			} else {
				require.NoError(t, err, "a refused cancellation cancels nothing")
				assert.True(t, answered7, "the request goes on and is answered")
			}
		})
	}
}
