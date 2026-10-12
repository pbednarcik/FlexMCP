package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// Two JSON-RPC ids a float64 cannot tell apart: past 2^53 a float64 drops the
// last digit, so a request id must be decoded as the integer it is.
const (
	idTwoTo53     = "9007199254740992"
	idTwoTo53Plus = "9007199254740993"
)

// A response carries its request's id exactly as sent (JSON-RPC 2.0, section
// 5), however large an integer it is.
func TestHandleMessage_EchoesALargeIntegerIDExactly(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0")
	for name, message := range map[string]string{
		"result": `{"jsonrpc":"2.0","id":` + idTwoTo53Plus + `,"method":"ping"}`,
		"error":  `{"jsonrpc":"1.0","id":` + idTwoTo53Plus + `,"method":"ping"}`,
	} {
		t.Run(name, func(t *testing.T) {
			response, err := json.Marshal(srv.HandleMessage(t.Context(), json.RawMessage(message)))
			require.NoError(t, err)
			assert.Contains(t, string(response), `"id":`+idTwoTo53Plus)
		})
	}
}

// notifications/cancelled names the request it cancels by its exact id: a
// cancel for 2^53+1 leaves request 2^53 running.
func TestCancelledNotificationTellsLargeIDsApart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, started := blockingToolServer(t)
		ctx := srv.WithContext(t.Context(), cancelTestSession("cancel-session"))
		responses := make(chan mcp.JSONRPCMessage, 1)
		go func() {
			responses <- srv.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":`+idTwoTo53+`,"method":"tools/call","params":{"name":"block"}}`))
		}()
		<-started
		cancel := func(id string) json.RawMessage {
			return json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":` + id + `}}`)
		}

		require.Nil(t, srv.HandleMessage(ctx, cancel(idTwoTo53Plus)))
		synctest.Wait()
		select {
		case resp := <-responses:
			t.Fatalf("a cancel for another id ended the request: %v", resp)
		default:
		}

		require.Nil(t, srv.HandleMessage(ctx, cancel(idTwoTo53)))
		synctest.Wait()
		select {
		case <-responses:
		default:
			t.Fatal("the request did not end after its own cancel")
		}
	})
}

// The stdio transport keys its replies and cancellations by the exact id: a
// refusal carries it, and a cancel for 2^53+1 does not silence request 2^53.
func TestStdioServer_KeepsLargeIntegerIDsExact(t *testing.T) {
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
	call := func(id string) string {
		return `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"hold"}}`
	}

	_, err := fmt.Fprintln(input, call(idTwoTo53))
	require.NoError(t, err)
	<-started
	go func() {
		defer input.Close()
		for _, line := range []string{
			`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":` + idTwoTo53Plus + `}}`,
			call("1"),
			call(idTwoTo53Plus),
		} {
			if _, err := fmt.Fprintln(input, line); err != nil {
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
	assert.Contains(t, output.String(), `"id":`+idTwoTo53Plus+`,"error":{"code":-32603,"message":"server busy`, "the refusal lost its id")
	assert.Contains(t, output.String(), `"id":`+idTwoTo53+`,`, "a cancel for another id silenced the request")
}

// The Streamable HTTP server's own refusals carry the request's exact id.
func TestStreamableHTTP_RefusalEchoesALargeIntegerIDExactly(t *testing.T) {
	server := httptest.NewServer(NewStreamableHTTPServer(NewMCPServer("test", "1.0.0")))
	t.Cleanup(server.Close)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL,
		strings.NewReader(`{"jsonrpc":"2.0","id":`+idTwoTo53Plus+`,"method":"tools/list"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(mcp.HeaderProtocolVersion, "1999-01-01")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode, "%s", body)
	assert.Contains(t, string(body), `"id":`+idTwoTo53Plus)
}

// An id no number type can hold, such as 1e400, is not a valid request id: the
// request is refused as invalid with a null id, never run and answered as some
// other id (JSON-RPC 2.0, section 5).
func TestAnUnrepresentableIDIsRefused(t *testing.T) {
	const ran = `"result"`
	for _, id := range []string{"1e400", "-1e400"} {
		message := `{"jsonrpc":"2.0","id":` + id + `,"method":"ping"}`
		t.Run("handle message "+id, func(t *testing.T) {
			response, err := json.Marshal(NewMCPServer("test", "1.0.0").HandleMessage(t.Context(), json.RawMessage(message)))
			require.NoError(t, err)
			assert.NotContains(t, string(response), ran)
			assert.Contains(t, string(response), `"id":null`)
			assert.Contains(t, string(response), `"code":-32600`)
		})
		t.Run("stdio "+id, func(t *testing.T) {
			stdioServer := NewStdioServer(NewMCPServer("test", "1.0.0"))
			stdioServer.SetErrorLogger(log.New(io.Discard, "", 0))
			var output bytes.Buffer
			require.NoError(t, stdioServer.Listen(t.Context(), strings.NewReader(message+"\n"), &output))
			assert.NotContains(t, output.String(), ran)
			assert.Contains(t, output.String(), `"id":null`)
			assert.Contains(t, output.String(), `"code":-32600`)
		})
		t.Run("streamable http "+id, func(t *testing.T) {
			server := httptest.NewServer(NewStreamableHTTPServer(NewMCPServer("test", "1.0.0")))
			t.Cleanup(server.Close)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, strings.NewReader(message))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "%s", body)
			assert.NotContains(t, string(body), ran)
			assert.Contains(t, string(body), `"id":null`)
			assert.Contains(t, string(body), `"code":-32600`)
		})
	}
}
