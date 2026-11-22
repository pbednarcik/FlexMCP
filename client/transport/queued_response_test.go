package transport

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// queued waits until the transport's reader has delivered every pending
// response, which it does under mu, removing the request's channel.
func queued(t *testing.T, pending func() int) {
	t.Helper()
	require.Eventually(t, func() bool { return pending() == 0 }, 5*time.Second, time.Millisecond, "the response is queued")
}

// A stdio response the reader queued before the caller's cancel is the
// answer: it arrived. A select choosing at random between the two would fail
// about half of these requests, and the client would cancel a request that
// had finished.
func TestStdioAResponseQueuedBeforeTheCancelIsTheAnswer(t *testing.T) {
	for i := range 50 {
		serverIn, clientOut := io.Pipe()
		clientIn, serverOut := io.Pipe()
		c := NewIO(clientIn, clientOut, io.NopCloser(strings.NewReader("")))
		require.NoError(t, c.Start(t.Context()))
		go func() {
			line, err := bufio.NewReader(serverIn).ReadBytes('\n')
			if err != nil {
				return
			}
			var request struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(line, &request)
			_, _ = fmt.Fprintf(serverOut, `{"jsonrpc":"2.0","id":%s,"result":{}}`+"\n", request.ID)
		}()

		ctx, cancel := context.WithCancel(t.Context())
		c.testHookAwaitResponse = func() {
			queued(t, func() int {
				c.mu.RLock()
				defer c.mu.RUnlock()
				return len(c.responses)
			})
			cancel()
		}
		response, err := c.SendRequest(ctx, JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(i + 1)), Method: "ping"})
		require.NoError(t, err, "round %d", i)
		require.NotNil(t, response)

		require.NoError(t, c.Close())
		_ = serverOut.Close()
		_ = serverIn.Close()
	}
}

// The same on the legacy SSE transport, for the caller's cancel and for the
// transport's own response timeout.
func TestSSEAResponseQueuedBeforeTheWaitEndsIsTheAnswer(t *testing.T) {
	for _, end := range []string{"cancel", "timeout"} {
		t.Run(end, func(t *testing.T) {
			// A server and one client of its own: the mock streams to the
			// last client connected, and forgets it when any stream ends.
			url, closeServer := startMockSSEEchoServer()
			defer closeServer()
			var options []ClientOption
			if end == "timeout" {
				options = append(options, WithResponseTimeout(time.Millisecond))
			}
			c, err := NewSSE(url, options...)
			require.NoError(t, err)
			require.NoError(t, c.Start(t.Context()))
			defer func() { require.NoError(t, c.Close()) }()

			for i := range 25 {
				ctx, cancel := context.WithCancel(t.Context())
				c.testHookAwaitResponse = func() {
					queued(t, func() int {
						c.mu.RLock()
						defer c.mu.RUnlock()
						return len(c.responses)
					})
					if end == "cancel" {
						cancel()
					} else {
						time.Sleep(10 * time.Millisecond) // the timer has fired
					}
				}
				response, err := c.SendRequest(ctx, JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(i + 1)), Method: "debug/echo"})
				cancel()
				require.NoError(t, err, "round %d", i)
				require.NotNil(t, response)
			}
		})
	}
}

// A legacy SSE server may send the response on the stream before it answers
// the POST. A response queued while the POST is in flight is the answer when
// the caller's cancel then ends the POST.
func TestSSEAResponseQueuedDuringThePOSTIsTheAnswer(t *testing.T) {
	var mu sync.Mutex
	var stream http.ResponseWriter
	sent := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		mu.Lock()
		stream = w
		_, _ = fmt.Fprint(w, "event: endpoint\ndata: /message\n\n")
		w.(http.Flusher).Flush()
		mu.Unlock()
		<-r.Context().Done()
		mu.Lock()
		stream = nil
		mu.Unlock()
	})
	mux.HandleFunc("/message", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		mu.Lock()
		if stream != nil {
			_, _ = fmt.Fprintf(stream, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{}}\n\n", request.ID)
			stream.(http.Flusher).Flush()
		}
		mu.Unlock()
		sent <- struct{}{}
		// The POST is answered only once the caller has given up on it.
		<-r.Context().Done()
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c, err := NewSSE(server.URL)
	require.NoError(t, err)
	require.NoError(t, c.Start(t.Context()))
	defer func() { require.NoError(t, c.Close()) }()

	for i := range 10 {
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			<-sent
			// The channel was registered before the POST: none left means
			// the reader queued the response.
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
				c.mu.RLock()
				pending := len(c.responses)
				c.mu.RUnlock()
				if pending == 0 {
					break
				}
			}
			cancel()
		}()
		response, err := c.SendRequest(ctx, JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(i + 1)), Method: "ping"})
		cancel()
		require.NoError(t, err, "round %d", i)
		require.NotNil(t, response)
	}
}
