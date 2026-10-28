package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A tools/call runs the tool its handler finds registered under the name,
// which need not be the registration whose x-mcp-header bindings dispatch
// checked the Mcp-Param-* headers against: a BeforeCallTool hook stands for
// an AddTool landing between the two reads. The headers must hold for the
// registration that runs, and a call that breaks a binding gets the answer
// dispatch gives the same call once that registration is in place: 400 and
// -32020 (Transports.md rows 34 and 36).
func TestParamHeaders_CheckedAgainstTheRegistrationThatRuns(t *testing.T) {
	plain := mcp.NewToolWithRawSchema("echo", "", json.RawMessage(`{"type":"object","properties":{"region":{"type":"string"}}}`))
	bound := mcp.NewToolWithRawSchema("echo", "", json.RawMessage(`{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}}`))

	// serve sends one modern tools/call of echo over HTTP, with region "eu" in
	// its arguments and header set as given, to a server that has atDispatch
	// registered, nil for none, until a hook before the call registers bound;
	// then it sends the same call again.
	serve := func(t *testing.T, atDispatch *mcp.Tool, header map[string]string, holdTheMessage bool) raceCall {
		t.Helper()
		var call raceCall
		ran := ""
		handler := func(label string) ToolHandlerFunc {
			return func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				ran = label
				return mcp.NewToolResultText(label), nil
			}
		}
		hooks := &Hooks{}
		var srv *MCPServer
		swapped := false
		hooks.AddBeforeCallTool(func(context.Context, any, *mcp.CallToolRequest) {
			if !swapped {
				swapped = true
				srv.AddTool(bound, handler("bound"))
			}
		})
		if holdTheMessage {
			// A hook that receives the message sends the checks to the path
			// that reads it again.
			hooks.AddOnRequestInitialization(func(context.Context, any, any) error { return nil })
		}
		srv = NewMCPServer("race", "1.0.0", WithToolCapabilities(false), WithHooks(hooks))
		if atDispatch != nil {
			srv.AddTool(*atDispatch, handler("at dispatch"))
		}
		httpSrv := NewStreamableHTTPServer(srv)
		body, h := modernToolCall(t, map[string]any{"region": "eu"})
		for name, value := range header {
			h.Set(name, value)
		}
		call.first = httpAnswerOf(t, serveOne(httpSrv, body, h).Result())
		require.True(t, swapped, "the hook registered the bound tool")
		call.ran = ran
		call.again = httpAnswerOf(t, serveOne(httpSrv, body, h).Result())
		return call
	}

	refused := func(t *testing.T, call raceCall) {
		t.Helper()
		assert.Empty(t, call.ran, "no registration runs on a call that breaks a bound header")
		require.Equal(t, http.StatusBadRequest, call.again.status, "dispatch refuses the call once bound is registered: %s", call.again.body)
		assert.Contains(t, call.again.body, `"code":-32020`)
		assert.Equal(t, call.again.status, call.first.status, "the call that raced the registration: %s", call.first.body)
		assert.JSONEq(t, call.again.body, call.first.body, "the call that raced the registration is answered as dispatch answers it")
	}

	t.Run("a binding the new registration adds is enforced", func(t *testing.T) {
		refused(t, serve(t, &plain, nil, false))
	})

	t.Run("a binding is enforced when the name had no tool at dispatch", func(t *testing.T) {
		refused(t, serve(t, nil, nil, false))
	})

	t.Run("a binding is enforced on the path that reads the message again", func(t *testing.T) {
		refused(t, serve(t, &plain, nil, true))
	})

	t.Run("a header that disagrees with the new binding is refused", func(t *testing.T) {
		call := serve(t, &plain, map[string]string{mcp.HeaderParamPrefix + "Region": "us"}, false)
		refused(t, call)
		assert.Contains(t, call.first.body, "does not match")
	})

	t.Run("a call whose headers hold for the new registration runs it", func(t *testing.T) {
		call := serve(t, &plain, map[string]string{mcp.HeaderParamPrefix + "Region": "eu"}, false)
		assert.Equal(t, "bound", call.ran)
		assert.Equal(t, http.StatusOK, call.first.status, call.first.body)
		assert.Equal(t, http.StatusOK, call.again.status, call.again.body)
	})
}

// raceCall is what TestParamHeaders_CheckedAgainstTheRegistrationThatRuns
// sees of one call: the answer to the call that raced the registration, the
// answer to the same call sent again, and the registration that ran on the
// first, "" for none.
type raceCall struct {
	first, again httpAnswer
	ran          string
}

// httpAnswer is the status and body of an HTTP response.
type httpAnswer struct {
	status int
	body   string
}

func httpAnswerOf(t *testing.T, response *http.Response) httpAnswer {
	t.Helper()
	defer response.Body.Close()
	var body json.RawMessage
	require.NoError(t, json.NewDecoder(response.Body).Decode(&body))
	return httpAnswer{status: response.StatusCode, body: string(body)}
}

// handleToolCall is [MCPServer.handleToolCallChecked] for a call no
// Mcp-Param-* check was made of, in the shape the tests that reach the
// handler past dispatch call it.
func (s *MCPServer) handleToolCall(ctx context.Context, id any, request mcp.CallToolRequest) (any, *requestError) {
	return s.handleToolCallChecked(ctx, id, request, paramHeaderCheck{})
}
