package transport

import (
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

// The answer to a POST is the answer to the one request it carried: an error
// with a null id is the server saying it could not read that request's id
// (JSON-RPC 2.0, section 5), a response on its stream to another id is not
// its answer, and a server request on it whose id cannot be read is answered
// as invalid with a null id.
func TestStreamableHTTP_TheAnswerToAPostIsItsOwn(t *testing.T) {
	const refusal = `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request id"}}`
	request := JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Method: "tools/list"}
	serve := func(t *testing.T, contentType string, events ...string) (*StreamableHTTP, <-chan string) {
		posted := make(chan string, 4)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"method"`) {
				posted <- string(body)
				w.WriteHeader(http.StatusAccepted)
				return
			}
			w.Header().Set("Content-Type", contentType)
			for _, event := range events {
				if contentType == "application/json" {
					_, _ = io.WriteString(w, event)
					continue
				}
				_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", event)
			}
		}))
		t.Cleanup(server.Close)
		trans, err := NewStreamableHTTP(server.URL)
		require.NoError(t, err)
		t.Cleanup(func() { _ = trans.Close() })
		return trans, posted
	}

	t.Run("a json error with a null id", func(t *testing.T) {
		trans, _ := serve(t, "application/json", refusal)
		response, err := trans.SendRequest(t.Context(), request)
		require.NoError(t, err)
		require.NotNil(t, response.Error)
		assert.Equal(t, mcp.INVALID_REQUEST, response.Error.Code)
	})
	t.Run("a json response to another id", func(t *testing.T) {
		trans, _ := serve(t, "application/json", `{"jsonrpc":"2.0","id":2,"result":{"answer":0}}`)
		_, err := trans.SendRequest(t.Context(), request)
		require.ErrorContains(t, err, "answers request 2, not 1")
	})
	t.Run("a stream error with a null id", func(t *testing.T) {
		trans, _ := serve(t, "text/event-stream", refusal)
		response, err := trans.SendRequest(t.Context(), request)
		require.NoError(t, err)
		require.NotNil(t, response.Error)
		assert.Equal(t, mcp.INVALID_REQUEST, response.Error.Code)
	})
	t.Run("a stream response to another id", func(t *testing.T) {
		trans, _ := serve(t, "text/event-stream",
			`{"jsonrpc":"2.0","id":2,"result":{"answer":0}}`,
			`{"jsonrpc":"2.0","id":1,"result":{"answer":42}}`)
		response, err := trans.SendRequest(t.Context(), request)
		require.NoError(t, err)
		assert.JSONEq(t, `{"answer":42}`, string(response.Result))
	})
	t.Run("a stream server request with an empty or non-string method", func(t *testing.T) {
		trans, posted := serve(t, "text/event-stream",
			`{"jsonrpc":"2.0","id":1,"method":""}`,
			`{"jsonrpc":"2.0","id":1,"method":null,"result":{}}`,
			`{"jsonrpc":"2.0","id":1,"result":{"answer":42}}`)
		response, err := trans.SendRequest(t.Context(), request)
		require.NoError(t, err)
		assert.JSONEq(t, `{"answer":42}`, string(response.Result))
		codes := map[string]bool{}
		for range 2 {
			select {
			case reply := <-posted:
				var answer struct {
					ID    json.RawMessage `json:"id"`
					Error struct {
						Code int `json:"code"`
					} `json:"error"`
				}
				require.NoError(t, json.Unmarshal([]byte(reply), &answer), reply)
				assert.Equal(t, "1", string(answer.ID), reply)
				codes[fmt.Sprint(answer.Error.Code)] = true
			case <-time.After(2 * time.Second):
				t.Fatal("a server request was not answered")
			}
		}
		assert.Equal(t, map[string]bool{"-32601": true, "-32600": true}, codes)
	})
	t.Run("a stream server request with an unreadable id", func(t *testing.T) {
		trans, posted := serve(t, "text/event-stream",
			`{"jsonrpc":"2.0","id":1.5,"method":"ping"}`,
			`{"jsonrpc":"2.0","id":1,"result":{"answer":42}}`)
		response, err := trans.SendRequest(t.Context(), request)
		require.NoError(t, err)
		assert.JSONEq(t, `{"answer":42}`, string(response.Result))
		select {
		case reply := <-posted:
			assert.JSONEq(t, refusal, reply)
		case <-time.After(2 * time.Second):
			t.Fatal("a server request with an unreadable id was not answered")
		}
	})
}

// An error answer on a failure status is still checked against its request: an
// error to another id is no answer, and an initialize that gets none of its
// own gives back the session ID its headers carried.
func TestStreamableHTTP_AnErrorStatusAnswerIsCheckedToo(t *testing.T) {
	serve := func(t *testing.T, status int, contentType, body string) *StreamableHTTP {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set(HeaderKeySessionID, "minted")
			w.Header().Set("Content-Type", contentType)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
		t.Cleanup(server.Close)
		trans, err := NewStreamableHTTP(server.URL)
		require.NoError(t, err)
		t.Cleanup(func() { _ = trans.Close() })
		return trans
	}
	list := JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Method: "tools/list"}
	initialize := JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Method: string(mcp.MethodInitialize)}

	t.Run("a failure status error to another id", func(t *testing.T) {
		trans := serve(t, http.StatusBadRequest, "application/json", `{"jsonrpc":"2.0","id":2,"error":{"code":-32602,"message":"bad"}}`)
		response, err := trans.SendRequest(t.Context(), list)
		require.Error(t, err)
		assert.Nil(t, response)
	})
	t.Run("a failure status error with a null id", func(t *testing.T) {
		trans := serve(t, http.StatusBadRequest, "application/json", `{"jsonrpc":"2.0","id":null,"error":{"code":-32600,"message":"Invalid request id"}}`)
		response, err := trans.SendRequest(t.Context(), list)
		require.NoError(t, err)
		require.NotNil(t, response.Error)
		assert.Equal(t, mcp.INVALID_REQUEST, response.Error.Code)
	})
	t.Run("an initialize answered to another id", func(t *testing.T) {
		trans := serve(t, http.StatusOK, "application/json", `{"jsonrpc":"2.0","id":2,"result":{}}`)
		_, err := trans.SendRequest(t.Context(), initialize)
		require.Error(t, err)
		assert.Empty(t, trans.GetSessionId())
	})
	t.Run("an initialize answered with no json", func(t *testing.T) {
		trans := serve(t, http.StatusOK, "application/json", `not json`)
		_, err := trans.SendRequest(t.Context(), initialize)
		require.Error(t, err)
		assert.Empty(t, trans.GetSessionId())
	})
	t.Run("an initialize whose stream ends unanswered", func(t *testing.T) {
		trans := serve(t, http.StatusOK, "text/event-stream", "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{}}\n\n")
		_, _ = trans.SendRequest(t.Context(), initialize)
		assert.Empty(t, trans.GetSessionId())
	})
}
