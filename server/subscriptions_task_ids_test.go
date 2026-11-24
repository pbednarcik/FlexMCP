package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// listenTaskIDsCase is one subscriptions/listen asking for task pushes: whether
// the server serves Tasks, and whether the request declares the extension.
type listenTaskIDsCase struct {
	name     string
	serves   bool
	declared bool
}

var listenTaskIDsCases = []listenTaskIDsCase{
	{name: "tasks served, extension not declared", serves: true},
	{name: "tasks served, extension declared", serves: true, declared: true},
	{name: "tasks not served, extension not declared"},
	{name: "tasks not served, extension declared", declared: true},
}

// listenTaskIDsBody is a subscriptions/listen request with the given id asking
// for task pushes, declaring the Tasks extension or no capability at all.
func listenTaskIDsBody(id string, declared bool) string {
	return modernRequestBody(id, "subscriptions/listen", `"notifications":{"taskIds":["t1"]}`, declared)
}

// modernRequestBody is a modern request with the given id, method and params
// members, declaring the Tasks extension or no capability at all.
func modernRequestBody(id, method, params string, declared bool) string {
	capabilities := `{}`
	if declared {
		capabilities = `{"extensions":{"` + mcp.ExtensionTasks + `":{}}}`
	}
	return `{"jsonrpc":"2.0","id":` + id + `,"method":"` + method + `","params":{` + params + `,` +
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"c","version":"1"},` +
		`"io.modelcontextprotocol/clientCapabilities":` + capabilities + `}}}`
}

// stdioAnswer sends line on stdio and returns the messages up to and with the
// response whose id is id.
func stdioAnswer(t *testing.T, send func(string), out <-chan map[string]any, line string, id any) []map[string]any {
	t.Helper()
	send(line)
	var messages []map[string]any
	for {
		message := awaitMessage(t, out, func(map[string]any) bool { return true }, "the answer")
		messages = append(messages, message)
		if message["id"] == id {
			return messages
		}
	}
}

func listenTaskIDsServer(serves bool) *MCPServer {
	opts := []ServerOption{WithToolCapabilities(true)}
	if serves {
		opts = append(opts, WithTaskHandler(stubTaskHandler{}))
	}
	return NewMCPServer("test", "1.0.0", opts...)
}

// checkListenTaskIDs checks the messages a subscriptions/listen asking for
// task pushes got, in order, ending with its response: a server serving Tasks
// refuses a request that did not declare the extension with -32021 naming it,
// before any acknowledgement (Tasks.md row 22); every other request is
// acknowledged without taskIds, since FlexMCP sends no task push (Patterns.md
// row 16).
func checkListenTaskIDs(t *testing.T, c listenTaskIDsCase, messages []map[string]any) {
	t.Helper()
	require.NotEmpty(t, messages, "no message answered the listen")
	response := messages[len(messages)-1]
	if c.serves && !c.declared {
		require.Len(t, messages, 1, "a refused listen is acknowledged first: %v", messages)
		rpcErr, ok := response["error"].(map[string]any)
		require.True(t, ok, "expected an error, got %v", response)
		assert.Equal(t, float64(mcp.MISSING_REQUIRED_CLIENT_CAPABILITY), rpcErr["code"])
		assert.Equal(t, map[string]any{"requiredCapabilities": map[string]any{
			"extensions": map[string]any{mcp.ExtensionTasks: map[string]any{}},
		}}, rpcErr["data"])
		return
	}
	require.Len(t, messages, 2, "expected an acknowledgement and a result: %v", messages)
	ack := messages[0]
	require.Equal(t, string(mcp.MethodNotificationSubscriptionsAcknowledged), ack["method"])
	params, _ := ack["params"].(map[string]any)
	assert.Equal(t, map[string]any{}, params["notifications"], "the acknowledgement grants no task push")
	assert.Contains(t, response, "result")
}

// TestSubscriptionsListen_TaskIDsNeedTheTasksExtension pins how a
// subscriptions/listen asking for task status notifications is answered on
// stdio and Streamable HTTP: refused with -32021 by a server serving Tasks when
// the request does not declare the extension, acknowledged without taskIds
// otherwise.
func TestSubscriptionsListen_TaskIDsNeedTheTasksExtension(t *testing.T) {
	for _, c := range listenTaskIDsCases {
		t.Run("stdio/"+c.name, func(t *testing.T) {
			send, out := stdioPipes(t, listenTaskIDsServer(c.serves))
			checkListenTaskIDs(t, c, stdioAnswer(t, send, out, listenTaskIDsBody(`"listen-1"`, c.declared), "listen-1"))
		})
		t.Run("streamable http/"+c.name, func(t *testing.T) {
			httpServer := httptest.NewServer(NewStreamableHTTPServer(listenTaskIDsServer(c.serves)))
			defer httpServer.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, httpServer.URL,
				bytes.NewReader([]byte(listenTaskIDsBody(`7`, c.declared))))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set(mcp.HeaderProtocolVersion, mcp.ProtocolVersion20260728)
			req.Header.Set(mcp.HeaderMethod, string(mcp.MethodSubscriptionsListen))
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			// -32021 is answered 400, as protocol version 2026-07-28 prescribes.
			wantStatus := http.StatusOK
			if c.serves && !c.declared {
				wantStatus = http.StatusBadRequest
			}
			assert.Equal(t, wantStatus, resp.StatusCode)
			var messages []map[string]any
			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				line := strings.TrimPrefix(scanner.Text(), "data: ")
				if !strings.HasPrefix(line, "{") {
					continue
				}
				var message map[string]any
				require.NoError(t, json.Unmarshal([]byte(line), &message))
				messages = append(messages, message)
			}
			require.NoError(t, scanner.Err())
			checkListenTaskIDs(t, c, messages)
		})
	}
}

// TestSubscriptionsListen_AnEmptyTaskIDsListAsksForNothing pins that an empty
// taskIds asks for no task push: a server serving Tasks acknowledges it from a
// request that does not declare the extension, and closes the stream at once.
func TestSubscriptionsListen_AnEmptyTaskIDsListAsksForNothing(t *testing.T) {
	send, out := stdioPipes(t, listenTaskIDsServer(true))
	messages := stdioAnswer(t, send, out,
		modernRequestBody(`"listen-1"`, "subscriptions/listen", `"notifications":{"taskIds":[]}`, false), "listen-1")
	checkListenTaskIDs(t, listenTaskIDsCase{declared: true}, messages)
}

// TestSubscriptionsListen_TheRefusalSharesNoState pins that the -32021 a listen
// asking for task pushes gets carries capabilities of its own: an OnError hook
// that changes them does not change what a task method requires.
func TestSubscriptionsListen_TheRefusalSharesNoState(t *testing.T) {
	hooks := &Hooks{}
	hooks.AddOnError(func(_ context.Context, _ any, _ mcp.MCPMethod, _ any, err error) {
		if missing, ok := errors.AsType[mcp.MissingRequiredClientCapabilityError](err); ok {
			clear(missing.Required.Extensions)
		}
	})
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(true), WithTaskHandler(stubTaskHandler{}), WithHooks(hooks))
	send, out := stdioPipes(t, srv)
	// The hook runs before the refusal is written, so its data is what the
	// hook left; only its code is checked here.
	for _, request := range []struct{ id, body string }{
		{"listen-1", listenTaskIDsBody(`"listen-1"`, false)},
		{"get-1", modernRequestBody(`"get-1"`, "tasks/get", `"taskId":"t1"`, false)},
	} {
		answer := stdioAnswer(t, send, out, request.body, request.id)
		rpcErr, ok := answer[len(answer)-1]["error"].(map[string]any)
		require.True(t, ok, "%s from an undeclared client answered %v", request.id, answer)
		assert.Equal(t, float64(mcp.MISSING_REQUIRED_CLIENT_CAPABILITY), rpcErr["code"], request.id)
	}
}
