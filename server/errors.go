package server

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pbednarcik/flexmcp/mcp"
)

var (
	// Common server errors
	ErrUnsupported      = errors.New("not supported")
	ErrResourceNotFound = errors.New("resource not found")
	ErrPromptNotFound   = errors.New("prompt not found")
	ErrToolNotFound     = errors.New("tool not found")

	// Session-related errors
	ErrSessionNotFound       = errors.New("session not found")
	ErrSessionExists         = errors.New("session already exists")
	ErrSessionNotInitialized = errors.New("session not properly initialized")

	// Notification-related errors
	ErrNotificationNotInitialized = errors.New("notification channel not initialized")
	ErrNotificationChannelBlocked = errors.New("notification channel queue is full - client may not be processing notifications fast enough")

	// Protocol-era errors

	// ErrRemovedInProtocolVersion indicates the request used a method that was
	// removed in protocol version 2026-07-28. The method remains available to
	// clients using an earlier protocol version.
	ErrRemovedInProtocolVersion = errors.New("was removed in protocol version " + mcp.ProtocolVersion20260728)

	// ErrRequiresModernProtocol indicates the request used a method that was
	// introduced in protocol version 2026-07-28 without declaring that version
	// in its _meta.
	ErrRequiresModernProtocol = errors.New("requires protocol version " + mcp.ProtocolVersion20260728 + " or later")

	// ErrInputRequiresModernClient is returned when a handler asks the client
	// for input through a multi round-trip result and the client predates
	// protocol version 2026-07-28: the server issues no server-initiated
	// requests, so the question cannot reach that client.
	ErrInputRequiresModernClient = errors.New(
		"the handler asked the client for input, which needs protocol version " + mcp.ProtocolVersion20260728 +
			" or later (multi round-trip requests, SEP-2322)")

	// ErrLoadShedding is returned when a handler sheds load, asking the client
	// to retry later through a multi round-trip result with no input requests,
	// and the client predates protocol version 2026-07-28. Callers may map it
	// to a backpressure response, such as HTTP 503 with Retry-After.
	ErrLoadShedding = errors.New("the server is busy, retry later")

	// ErrTaskInputUnsupported fails a task whose handler asked the client for
	// input: mid-task input goes through tasks/update (Tasks.md row 15),
	// which this server does not serve yet, so the question cannot be put.
	ErrTaskInputUnsupported = errors.New("the handler asked the client for input, which the server cannot do inside a task")

	// ErrRequestIDInFlight refuses a request whose id is still in flight in
	// its session (BaseProtocol.md row 4): the request in flight is left
	// alone, and the error hooks see the refusal.
	ErrRequestIDInFlight = errors.New("request id is in flight")
)

// errTaskHookPanicked fails a task a hook panicked on while it was created:
// its goroutine never starts, so nothing else would end it.
var errTaskHookPanicked = errors.New("a task hook panicked while the task was created")

// noResultError reports a handler that returned neither a result nor an
// error, which leaves the server nothing to send.
func noResultError(kind, name string) error {
	return fmt.Errorf("%s '%s' handler returned no result", kind, name)
}

// UnparsableMessageError is attached to the RequestError when json.Unmarshal
// fails on the request.
type UnparsableMessageError struct {
	message json.RawMessage
	method  mcp.MCPMethod
	err     error
}

func (e *UnparsableMessageError) Error() string {
	return fmt.Sprintf("unparsable %s request: %s", e.method, e.err)
}

func (e *UnparsableMessageError) Unwrap() error {
	return e.err
}

func (e *UnparsableMessageError) GetMessage() json.RawMessage {
	return e.message
}

func (e *UnparsableMessageError) GetMethod() mcp.MCPMethod {
	return e.method
}

// undecodableParamsMessage is what a client is told about params that could
// not be decoded: the decoder's own text names Go types and fields, so it
// stays on the server, for hooks and logs.
func undecodableParamsMessage(method mcp.MCPMethod) *string {
	message := "invalid params for " + string(method)
	return &message
}

// RequestError is an error that can be converted to a JSON-RPC error.
// Implements Unwrap() to allow inspecting the error chain.
type requestError struct {
	id   any
	code int
	err  error
	// message, when set, is sent instead of err.Error().
	message *string
	// data, when set, is sent as the data member of the JSON-RPC error.
	data any
}

func (e *requestError) Error() string {
	return fmt.Sprintf("request error: %s", e.err)
}

func (e *requestError) ToJSONRPCError() mcp.JSONRPCError {
	message := e.err.Error()
	if e.message != nil {
		message = *e.message
	}
	return mcp.JSONRPCError{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      mcp.NewRequestId(e.id),
		Error:   mcp.NewJSONRPCErrorDetails(e.code, message, e.data),
	}
}

func (e *requestError) Unwrap() error {
	return e.err
}
