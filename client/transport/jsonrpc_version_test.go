package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// badVersionRequests are server requests whose jsonrpc member is not "2.0":
// another version, none at all, a number.
var badVersionRequests = []string{
	`{"jsonrpc":"1.0","id":11,"method":"ping"}`,
	`{"id":12,"method":"ping"}`,
	`{"jsonrpc":2,"id":13,"method":"ping"}`,
}

const badVersionAnswer = `{"code":-32600,"message":"Invalid request: jsonrpc must be \"2.0\""}`

// A server request whose jsonrpc member is not "2.0" is not a JSON-RPC 2.0
// request: it is answered as invalid, with its id, and not served
// (JSON-RPC 2.0, sections 4 and 5).
func TestStdio_AServerRequestOfAnotherVersionIsInvalid(t *testing.T) {
	serverOut, toClient := io.Pipe()
	t.Cleanup(func() { _ = toClient.Close() })
	sink := &concurrentBuffer{}
	stdio := NewIO(serverOut, sink, nil)
	stdio.SetRequestHandler(func(context.Context, JSONRPCRequest) (*JSONRPCResponse, error) {
		t.Error("a request of another version was served")
		return nil, nil
	})
	require.NoError(t, stdio.Start(t.Context()))
	t.Cleanup(func() { _ = stdio.Close() })

	go func() {
		for _, request := range badVersionRequests {
			_, _ = fmt.Fprintln(toClient, request)
		}
	}()
	require.Eventually(t, func() bool { return strings.Count(sink.String(), "\n") == len(badVersionRequests) }, 2*time.Second, time.Millisecond)
	for i, line := range strings.Split(strings.TrimSpace(sink.String()), "\n") {
		var answer struct {
			ID    int             `json:"id"`
			Error json.RawMessage `json:"error"`
		}
		require.NoError(t, json.Unmarshal([]byte(line), &answer))
		assert.Equal(t, 11+i, answer.ID)
		assert.JSONEq(t, badVersionAnswer, string(answer.Error))
	}
}

// Only what the client answers is judged: a response without the member is
// still taken, so a server lax about it keeps working.
func TestStdio_AResponseWithoutAVersionIsTaken(t *testing.T) {
	serverOut, toClient := io.Pipe()
	t.Cleanup(func() { _ = toClient.Close() })
	stdio := NewIO(serverOut, answeringWriter{stdout: toClient, answer: `{"id":1,"result":{"ok":true}}`}, nil)
	require.NoError(t, stdio.Start(t.Context()))
	t.Cleanup(func() { _ = stdio.Close() })

	response, err := stdio.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(1)), Method: "ping"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":true}`, string(response.Result))
}

func TestStreamableHTTP_AServerRequestOfAnotherVersionIsInvalid(t *testing.T) {
	answers := make(chan string, len(badVersionRequests))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.Unmarshal(body, &message)
		if message.Method == "" {
			answers <- string(body)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, request := range badVersionRequests {
			_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", request)
		}
		_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{}}\n\n", message.ID)
	}))
	t.Cleanup(server.Close)
	trans, err := NewStreamableHTTP(server.URL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = trans.Close() })
	trans.SetRequestHandler(func(context.Context, JSONRPCRequest) (*JSONRPCResponse, error) {
		t.Error("a request of another version was served")
		return nil, nil
	})
	require.NoError(t, trans.Start(t.Context()))

	_, err = trans.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(1)), Method: "tools/list"})
	require.NoError(t, err)
	for i := range badVersionRequests {
		select {
		case body := <-answers:
			var answer struct {
				ID    int             `json:"id"`
				Error json.RawMessage `json:"error"`
			}
			require.NoError(t, json.Unmarshal([]byte(body), &answer))
			assert.Equal(t, 11+i, answer.ID)
			assert.JSONEq(t, badVersionAnswer, string(answer.Error))
		case <-time.After(2 * time.Second):
			t.Fatalf("no answer posted for %s", badVersionRequests[i])
		}
	}
}
