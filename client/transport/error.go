package transport

import "fmt"

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
