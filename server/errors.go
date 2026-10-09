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
	ErrSessionNotFound                        = errors.New("session not found")
	ErrSessionExists                          = errors.New("session already exists")
	ErrSessionNotInitialized                  = errors.New("session not properly initialized")
	ErrSessionDoesNotSupportTools             = errors.New("session does not support per-session tools")
	ErrSessionDoesNotSupportResources         = errors.New("session does not support per-session resources")
	ErrSessionDoesNotSupportResourceTemplates = errors.New("session does not support resource templates")

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
)

// ErrDynamicPathConfig is returned when attempting to use static path methods with dynamic path configuration
type ErrDynamicPathConfig struct {
	Method string
}

func (e *ErrDynamicPathConfig) Error() string {
	return fmt.Sprintf("%s cannot be used with WithDynamicBasePath. Use dynamic path logic in your router.", e.Method)
}

// noResultError reports a handler that returned neither a result nor an
// error, which leaves the server nothing to send.
func noResultError(kind, name string) error {
	return fmt.Errorf("%s '%s' handler returned no result", kind, name)
}
