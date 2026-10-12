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
	idTwoTo63     = "9223372036854775808"
)

// A response carries its request's id exactly as sent (JSON-RPC 2.0, section
// 5), however large an integer it is.
func TestHandleMessage_EchoesALargeIntegerIDExactly(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0")
	for name, message := range map[string]string{
		"result":            `{"jsonrpc":"2.0","id":` + idTwoTo53Plus + `,"method":"ping"}`,
		"error":             `{"jsonrpc":"1.0","id":` + idTwoTo53Plus + `,"method":"ping"}`,
		"result past int64": `{"jsonrpc":"2.0","id":` + idTwoTo63 + `,"method":"ping"}`,
		"error past int64":  `{"jsonrpc":"1.0","id":` + idTwoTo63 + `,"method":"ping"}`,
	} {
		t.Run(name, func(t *testing.T) {
			response, err := json.Marshal(srv.HandleMessage(t.Context(), json.RawMessage(message)))
			require.NoError(t, err)
			sent := struct {
				ID json.RawMessage `json:"id"`
			}{}
			require.NoError(t, json.Unmarshal([]byte(message), &sent))
			assert.Contains(t, string(response), `"id":`+string(sent.ID)+`,`)
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

// A request id is a string or an integer (MCP base protocol). Anything else,
// a fraction, a boolean, an object, an array or a number no type can hold such
// as 1e400, is refused as an invalid request with a null id, never run and
// never answered as some other id (JSON-RPC 2.0, section 5).
func TestAnInvalidIDIsRefused(t *testing.T) {
	const ran = `"result"`
	for _, id := range []string{"null", "1e400", "-1e400", "1.5", "1.0", "1e3", "true", "{}", "[1]"} {
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

// The stdio server tracks a request whose id is past int64 like any other, so
// a cancel for it is honoured.
func TestStdioCancelsAnIDPastInt64(t *testing.T) {
	var requests cancelledRequests
	id, ok := requestIDValue(json.RawMessage(idTwoTo63))
	require.True(t, ok)
	requests.start(id)
	requests.cancel(id)
	assert.True(t, requests.finish(id))
}

// A response is never answered (JSON-RPC 2.0, section 5), whatever its id: a
// null id is how a peer answers a request whose id it could not read, and an
// invalid one is still no request to refuse.
func TestAResponseIsNeverAnswered(t *testing.T) {
	for _, id := range []string{"1", "null", "1.5", "true", "{}"} {
		for _, body := range []string{`"result":{}`, `"error":{"code":-32600,"message":"Invalid request id"}`} {
			message := `{"jsonrpc":"2.0","id":` + id + `,` + body + `}`
			t.Run("handle message "+message, func(t *testing.T) {
				assert.Nil(t, NewMCPServer("test", "1.0.0").HandleMessage(t.Context(), json.RawMessage(message)))
			})
			t.Run("stdio "+message, func(t *testing.T) {
				stdioServer := NewStdioServer(NewMCPServer("test", "1.0.0"))
				stdioServer.SetErrorLogger(log.New(io.Discard, "", 0))
				var output bytes.Buffer
				require.NoError(t, stdioServer.Listen(t.Context(), strings.NewReader(message+"\n"), &output))
				assert.Empty(t, output.String())
			})
		}
	}
	t.Run("streamable http accepts an error with a null id", func(t *testing.T) {
		server := httptest.NewServer(NewStreamableHTTPServer(NewMCPServer("test", "1.0.0")))
		t.Cleanup(server.Close)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL,
			strings.NewReader(`{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request id"}}`))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, http.StatusAccepted, resp.StatusCode, "%s", body)
	})
}

// A string id and an integer id with the same digits are two ids: a cancel for
// the integer 7 leaves request "7" running.
func TestCancelledNotificationTellsAStringIDFromAnInteger(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, started := blockingToolServer(t)
		ctx := srv.WithContext(t.Context(), cancelTestSession("cancel-session"))
		responses := make(chan mcp.JSONRPCMessage, 1)
		go func() {
			responses <- srv.HandleMessage(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":"7","method":"tools/call","params":{"name":"block"}}`))
		}()
		<-started
		cancel := func(id string) json.RawMessage {
			return json.RawMessage(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":` + id + `}}`)
		}

		require.Nil(t, srv.HandleMessage(ctx, cancel(`7`)))
		synctest.Wait()
		select {
		case resp := <-responses:
			t.Fatalf("a cancel for the integer 7 ended request \"7\": %v", resp)
		default:
		}

		require.Nil(t, srv.HandleMessage(ctx, cancel(`"7"`)))
		synctest.Wait()
		select {
		case <-responses:
		default:
			t.Fatal("the request did not end after its own cancel")
		}
	})
}

// -32700 is for text that is not JSON; JSON that is not a request object is an
// invalid request, -32600 (JSON-RPC 2.0, section 5.1). The error repeats the
// request's id when it could be read, and is null only when it could not.
func TestOnlyTextThatIsNotJSONIsAParseError(t *testing.T) {
	type answer struct{ code, id string }
	for message, want := range map[string]answer{
		`{`:                                   {"-32700", "null"},
		`5`:                                   {"-32600", "null"},
		`"ping"`:                              {"-32600", "null"},
		`{"jsonrpc":"2.0","id":1,"method":5}`: {"-32600", "1"},
		`{"jsonrpc":"2.0","method":["ping"],"id":"a"}`: {"-32600", `"a"`},
		`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`:   {"-32600", "null"},
	} {
		code := want.code
		t.Run("handle message "+message, func(t *testing.T) {
			response, err := json.Marshal(NewMCPServer("test", "1.0.0").HandleMessage(t.Context(), json.RawMessage(message)))
			require.NoError(t, err)
			assert.Contains(t, string(response), `"id":`+want.id)
			assert.Contains(t, string(response), `"code":`+code)
		})
		t.Run("stdio "+message, func(t *testing.T) {
			stdioServer := NewStdioServer(NewMCPServer("test", "1.0.0"))
			stdioServer.SetErrorLogger(log.New(io.Discard, "", 0))
			var output bytes.Buffer
			require.NoError(t, stdioServer.Listen(t.Context(), strings.NewReader(message+"\n"), &output))
			assert.Contains(t, output.String(), `"id":`+want.id)
			assert.Contains(t, output.String(), `"code":`+code)
		})
		t.Run("streamable http "+message, func(t *testing.T) {
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
			assert.Contains(t, string(body), `"id":`+want.id)
			assert.Contains(t, string(body), `"code":`+code)
		})
	}
}
