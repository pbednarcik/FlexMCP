package server

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var urlElicitationRequired = mcp.URLElicitationRequiredError{
	Elicitations: []mcp.ElicitationParams{{
		Mode:          mcp.ElicitationModeURL,
		ElicitationID: "auth-1",
		URL:           "https://example.com/authorize?id=auth-1",
		Message:       "Authorization is required to access this resource.",
	}},
}

// The pointer form as an error value: vet's printf check rejects %w on a
// pointer to a value-receiver error, but the server must unwrap it too.
var urlElicitationRequiredPointer error = &urlElicitationRequired

// testHandlerJSONRPCError is an application error with a JSON-RPC form whose
// message differs from its Go error text, so a test can tell which was sent.
type testHandlerJSONRPCError struct{}

func (testHandlerJSONRPCError) Error() string {
	return "private diagnostic"
}

func (testHandlerJSONRPCError) JSONRPCError() mcp.JSONRPCError {
	return mcp.JSONRPCError{
		Error: mcp.NewJSONRPCErrorDetails(
			mcp.INVALID_PARAMS,
			"invalid customer ID",
			map[string]any{"field": "customer_id"},
		),
	}
}

// testReservedCodeError is an application error whose JSON-RPC form claims a
// code that the SDK gives a protocol meaning of its own.
type testReservedCodeError struct {
	code int
}

func (e testReservedCodeError) Error() string {
	return fmt.Sprintf("private diagnostic %d", e.code)
}

func (e testReservedCodeError) JSONRPCError() mcp.JSONRPCError {
	return mcp.JSONRPCError{
		Error: mcp.NewJSONRPCErrorDetails(e.code, "claimed", map[string]any{"claimed": true}),
	}
}

func newHandlerErrorServer() *MCPServer {
	srv := NewMCPServer("test-server", "1.0.0",
		WithToolCapabilities(true),
		WithPromptCapabilities(true),
		WithResourceCapabilities(false, false),
	)
	srv.AddTool(mcp.NewTool("protected_action"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Wrapped, as a handler that adds context to the error would.
		return nil, fmt.Errorf("checking access: %w", urlElicitationRequired)
	})
	srv.AddTool(mcp.NewTool("protected_pointer_action"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, fmt.Errorf("checking access: %w", urlElicitationRequiredPointer)
	})
	srv.AddTool(mcp.NewTool("marked_protected_action"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, NewHandlerError(urlElicitationRequired)
	})
	srv.AddTool(mcp.NewTool("public_error_action"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, fmt.Errorf("validating request: %w", NewHandlerError(testHandlerJSONRPCError{}))
	})
	srv.AddTool(mcp.NewTool("reserved_code_action", mcp.WithNumber("code")), func(_ context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, NewHandlerError(testReservedCodeError{code: request.GetInt("code", 0)})
	})
	srv.AddTool(mcp.NewTool("failing_action"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, errors.New("backend unavailable")
	})
	srv.AddTool(mcp.NewTool("gateway_action"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// An error from an upstream server that happens to have a JSON-RPC
		// form of its own is still this server's internal error.
		return nil, fmt.Errorf("upstream: %w", mcp.UnsupportedProtocolVersionError{Version: "2099-01-01"})
	})
	srv.AddPrompt(mcp.NewPrompt("protected_prompt"), func(context.Context, mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return nil, urlElicitationRequired
	})
	srv.AddResource(mcp.NewResource("test://protected", "protected"), func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		return nil, urlElicitationRequired
	})
	srv.AddResourceTemplate(mcp.NewResourceTemplate("test://protected/{id}", "protected items"), func(context.Context, mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
		return nil, urlElicitationRequired
	})
	return srv
}

func handleForError(t *testing.T, srv *MCPServer, request string) mcp.JSONRPCError {
	t.Helper()
	response := srv.HandleMessage(t.Context(), []byte(request))
	errorResponse, ok := response.(mcp.JSONRPCError)
	require.True(t, ok, "expected an error response, got %T", response)
	assert.Equal(t, mcp.NewRequestId(int64(7)), errorResponse.ID)
	return errorResponse
}

// A handler returns mcp.URLElicitationRequiredError to tell the client that it
// must complete a URL elicitation first. Protocol version 2025-11-25 reports
// that as -32042 carrying the elicitations; the client can act on nothing less.
func TestHandlerURLElicitationRequiredError(t *testing.T) {
	srv := newHandlerErrorServer()

	tests := []struct {
		name    string
		request string
		message string
	}{
		{
			// The message is the error's own, not the handler's wrapping.
			name:    "tools/call",
			request: `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"protected_action"}}`,
			message: "URL elicitation required: 1 elicitation(s) needed",
		},
		{
			name:    "tools/call returning a pointer",
			request: `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"protected_pointer_action"}}`,
			message: "URL elicitation required: 1 elicitation(s) needed",
		},
		{
			name:    "prompts/get",
			request: `{"jsonrpc":"2.0","id":7,"method":"prompts/get","params":{"name":"protected_prompt"}}`,
			message: "URL elicitation required: 1 elicitation(s) needed",
		},
		{
			name:    "resources/read",
			request: `{"jsonrpc":"2.0","id":7,"method":"resources/read","params":{"uri":"test://protected"}}`,
			message: "URL elicitation required: 1 elicitation(s) needed",
		},
		{
			name:    "resources/read through a template",
			request: `{"jsonrpc":"2.0","id":7,"method":"resources/read","params":{"uri":"test://protected/1"}}`,
			message: "URL elicitation required: 1 elicitation(s) needed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errorResponse := handleForError(t, srv, tt.request)
			assert.Equal(t, mcp.URL_ELICITATION_REQUIRED, errorResponse.Error.Code)
			assert.Equal(t, tt.message, errorResponse.Error.Message)

			// Decode it the way the client does.
			var got mcp.URLElicitationRequiredError
			require.ErrorAs(t, errorResponse.Error.AsError(), &got)
			assert.Equal(t, urlElicitationRequired.Elicitations, got.Elicitations)
		})
	}
}

// Protocol version 2026-07-28 reserves -32042 and asks for a URL elicitation
// through a multi round-trip request instead, so a modern client still gets
// an internal error.
// Marking the error for the client does not change that.
func TestHandlerURLElicitationRequiredErrorForModernClients(t *testing.T) {
	srv := newHandlerErrorServer()

	for _, name := range []string{"protected_action", "marked_protected_action"} {
		t.Run(name, func(t *testing.T) {
			errorResponse := handleForError(t, srv, fmt.Sprintf(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{
				"name":%q,
				"_meta":{
					"io.modelcontextprotocol/protocolVersion":"2026-07-28",
					"io.modelcontextprotocol/clientCapabilities":{}
				}
			}}`, name))
			assert.Equal(t, mcp.INTERNAL_ERROR, errorResponse.Error.Code)
			assert.Nil(t, errorResponse.Error.Data)
		})
	}
}

// A handler error marked with NewHandlerError goes to the client as the code,
// message and data of its JSON-RPC form, under either protocol version.
func TestMarkedHandlerJSONRPCError(t *testing.T) {
	srv := newHandlerErrorServer()

	tests := []struct {
		name    string
		request string
	}{
		{
			name:    "legacy protocol",
			request: `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"public_error_action"}}`,
		},
		{
			name: "modern protocol",
			request: `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{
				"name":"public_error_action",
				"_meta":{
					"io.modelcontextprotocol/protocolVersion":"2026-07-28",
					"io.modelcontextprotocol/clientCapabilities":{}
				}
			}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errorResponse := handleForError(t, srv, tt.request)
			assert.Equal(t, mcp.JSONRPC_VERSION, errorResponse.JSONRPC)
			assert.Equal(t, mcp.INVALID_PARAMS, errorResponse.Error.Code)
			assert.Equal(t, "invalid customer ID", errorResponse.Error.Message)
			assert.Equal(t, map[string]any{"field": "customer_id"}, errorResponse.Error.Data)
		})
	}
}

// A marked error cannot claim a code the SDK gives a protocol meaning of its
// own: the client would act on it as that condition. It is an internal error,
// as if it were not marked.
func TestMarkedHandlerErrorWithReservedCode(t *testing.T) {
	srv := newHandlerErrorServer()

	codes := []int{
		mcp.REQUEST_INTERRUPTED,
		mcp.RESOURCE_NOT_FOUND,
		mcp.HEADER_MISMATCH,
		mcp.MISSING_REQUIRED_CLIENT_CAPABILITY,
		mcp.UNSUPPORTED_PROTOCOL_VERSION,
		mcp.URL_ELICITATION_REQUIRED,
		-32050,
		-32099,
	}
	meta := map[string]string{
		"legacy protocol": "",
		"modern protocol": `,"_meta":{
			"io.modelcontextprotocol/protocolVersion":"2026-07-28",
			"io.modelcontextprotocol/clientCapabilities":{}
		}`,
	}
	for protocol, extra := range meta {
		for _, code := range codes {
			t.Run(fmt.Sprintf("%s/%d", protocol, code), func(t *testing.T) {
				errorResponse := handleForError(t, srv, fmt.Sprintf(
					`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"reserved_code_action","arguments":{"code":%d}%s}}`,
					code, extra))
				assert.Equal(t, mcp.INTERNAL_ERROR, errorResponse.Error.Code)
				assert.Equal(t, internalErrorMessage, errorResponse.Error.Message)
				assert.Nil(t, errorResponse.Error.Data)
			})
		}
	}
}

// A typed nil provider is no error, as a nil one is.
func TestNewHandlerErrorWithTypedNilProvider(t *testing.T) {
	var provider *testHandlerJSONRPCError

	assert.NoError(t, NewHandlerError(provider))
	assert.NoError(t, NewHandlerError(nil))
}

// Any other handler error is an internal error, including one that carries an
// unmarked JSON-RPC form of its own from somewhere else.
func TestHandlerErrorsAreOtherwiseInternal(t *testing.T) {
	srv := newHandlerErrorServer()

	for _, name := range []string{"failing_action", "gateway_action"} {
		t.Run(name, func(t *testing.T) {
			errorResponse := handleForError(t, srv, fmt.Sprintf(
				`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":%q}}`, name))
			assert.Equal(t, mcp.INTERNAL_ERROR, errorResponse.Error.Code)
			assert.Nil(t, errorResponse.Error.Data)
		})
	}
}

// A code outside the range the specification keeps, such as -32019 or
// -32100, is the handler's to send.
func TestMarkedHandlerErrorBesideTheReservedRange(t *testing.T) {
	srv := newHandlerErrorServer()
	for _, code := range []int{-32019, -32100} {
		errorResponse := handleForError(t, srv, fmt.Sprintf(
			`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"reserved_code_action","arguments":{"code":%d}}}`, code))
		assert.Equal(t, code, errorResponse.Error.Code)
	}
}

// The text of an unmarked handler error stays on the server: it may name
// hosts, paths or credentials, so the client gets a fixed message, and hooks
// and logs get the error.
func TestUnmarkedHandlerErrorTextStaysOnTheServer(t *testing.T) {
	const secret = "query: dial postgres://app:s3cret@db.internal:5432"
	srv := newHandlerErrorServer()
	srv.AddTool(mcp.NewTool("leaky_action"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, errors.New(secret)
	})
	var logged error
	srv.hooks = &Hooks{}
	srv.hooks.AddOnError(func(_ context.Context, _ any, _ mcp.MCPMethod, _ any, err error) { logged = err })

	errorResponse := handleForError(t, srv, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"leaky_action"}}`)
	assert.Equal(t, mcp.INTERNAL_ERROR, errorResponse.Error.Code)
	assert.Equal(t, internalErrorMessage, errorResponse.Error.Message)
	assert.ErrorContains(t, logged, secret)
}

// A failed task's status message is the same fixed message, and its hook
// still gets the error.
func TestFailedTaskStatusMessageStaysOnTheServer(t *testing.T) {
	const secret = "query: dial postgres://app:s3cret@db.internal:5432"
	failed := make(chan TaskMetrics, 1)
	hooks := &TaskHooks{}
	hooks.AddOnTaskFailed(func(_ context.Context, metrics TaskMetrics) { failed <- metrics })
	srv := NewMCPServer("t", "1", WithTaskHooks(hooks), WithTaskCapabilities(true, false, false))
	srv.AddTaskTool(mcp.Tool{Name: "leaky", Execution: &mcp.ToolExecution{TaskSupport: mcp.TaskSupportRequired}},
		func(context.Context, mcp.CallToolRequest) (*mcp.CreateTaskResult, error) {
			return nil, errors.New(secret)
		})

	result, reqErr := srv.handleTaskAugmentedToolCall(t.Context(), "id", mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "leaky", Task: &mcp.TaskParams{}}})
	require.Nil(t, reqErr)
	select {
	case metrics := <-failed:
		assert.ErrorContains(t, metrics.Error, secret)
	case <-time.After(5 * time.Second):
		t.Fatal("the task never failed")
	}
	srv.tasksMu.RLock()
	statusMessage := srv.tasks[result.Task.TaskId].task.StatusMessage
	srv.tasksMu.RUnlock()
	assert.Equal(t, internalErrorMessage, statusMessage)
}

// The message a failed task shows is the one a request would get: an error
// the server recognises keeps its own text, as a marked one does.
func TestClientMessageRecognisesProtocolErrors(t *testing.T) {
	missing := mcp.MissingRequiredClientCapabilityError{Required: mcp.ClientCapabilities{Roots: &struct {
		ListChanged bool `json:"listChanged,omitempty"`
	}{}}}
	assert.Equal(t, urlElicitationRequired.Error(), clientMessage(fmt.Errorf("checking: %w", urlElicitationRequired)))
	assert.Equal(t, missing.Error(), clientMessage(fmt.Errorf("checking: %w", missing)))
	assert.Equal(t, "invalid customer ID", clientMessage(fmt.Errorf("validating: %w", NewHandlerError(testHandlerJSONRPCError{}))))
	assert.Equal(t, ErrLoadShedding.Error(), clientMessage(fmt.Errorf("busy: %w", ErrLoadShedding)))
	assert.Equal(t, internalErrorMessage, clientMessage(errors.New("dial postgres://app:s3cret@db")))
}

// completionProvider answers every completion with err.
type completionProvider struct{ err error }

func (p completionProvider) CompletePromptArgument(context.Context, string, mcp.CompleteArgument, mcp.CompleteContext) (*mcp.Completion, error) {
	return nil, p.err
}

// A completion provider's error follows the handler rule: a marked error
// is sent in its own JSON-RPC form, an unmarked one as a fixed message.
func TestCompletionProviderErrorsFollowTheHandlerRule(t *testing.T) {
	const request = `{"jsonrpc":"2.0","id":7,"method":"completion/complete","params":{"ref":{"type":"ref/prompt","name":"p"},"argument":{"name":"a","value":"x"}}}`
	t.Run("marked", func(t *testing.T) {
		srv := NewMCPServer("t", "1", WithCompletions(), WithPromptCompletionProvider(completionProvider{err: NewHandlerError(testHandlerJSONRPCError{})}))
		errorResponse := handleForError(t, srv, request)
		assert.Equal(t, mcp.INVALID_PARAMS, errorResponse.Error.Code)
		assert.Equal(t, "invalid customer ID", errorResponse.Error.Message)
	})
	t.Run("unmarked", func(t *testing.T) {
		srv := NewMCPServer("t", "1", WithCompletions(), WithPromptCompletionProvider(completionProvider{err: errors.New("dial postgres://app:s3cret@db")}))
		errorResponse := handleForError(t, srv, request)
		assert.Equal(t, mcp.INTERNAL_ERROR, errorResponse.Error.Code)
		assert.Equal(t, internalErrorMessage, errorResponse.Error.Message)
	})
}
