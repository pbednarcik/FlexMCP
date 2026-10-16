package transport

import (
	"errors"
	"fmt"
)

// Error wraps a low-level transport error in a concrete type.
type Error struct {
	Err error
}

// Error returns the wrapped error's text, marked as a transport error.
func (e *Error) Error() string {
	return fmt.Sprintf("transport error: %v", e.Err)
}

// Unwrap returns the wrapped error.
func (e *Error) Unwrap() error {
	return e.Err
}

// NewError wraps err as a transport Error.
func NewError(err error) *Error {
	return &Error{
		Err: err,
	}
}

// ErrAuthorizationRequired is a sentinel error for authorization required (401)
var ErrAuthorizationRequired = errors.New("authorization required")

// AuthorizationRequiredError is returned when the server answers 401, or
// 403 with an insufficient_scope challenge. It carries what the
// WWW-Authenticate challenge said.
type AuthorizationRequiredError struct {
	// ResourceMetadataURL is the resource_metadata parameter (RFC 9728
	// §5.1), if present.
	ResourceMetadataURL string
	// Scopes is the scope parameter (RFC 6750 §3): the scopes the operation
	// needs, authoritative for it. When re-authorizing, keep the scopes
	// already granted alongside them.
	Scopes []string
	// ErrorCode is the error parameter, "insufficient_scope" on a 403 that
	// asks for a step-up; empty on a plain 401.
	ErrorCode string
	// ErrorDescription is the error_description parameter, if any.
	ErrorDescription string
}

func (e *AuthorizationRequiredError) Error() string {
	return ErrAuthorizationRequired.Error()
}

func (e *AuthorizationRequiredError) Unwrap() error {
	return ErrAuthorizationRequired
}
