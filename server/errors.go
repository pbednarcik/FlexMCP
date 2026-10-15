package server

import (
	"errors"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
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
