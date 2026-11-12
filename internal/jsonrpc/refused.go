package jsonrpc

import "errors"

// MarkResponseRefused marks err, which is not nil, as the failure of a
// request whose one response arrived and was refused, such as a JSON answer
// over the client's bound: the work it answers is finished. The error it
// returns reads as err and wraps it.
func MarkResponseRefused(err error) error {
	return &refusedResponseError{err: err}
}

// ResponseRefused reports whether err, or an error it wraps, was marked by
// MarkResponseRefused.
func ResponseRefused(err error) bool {
	var refused *refusedResponseError
	return errors.As(err, &refused)
}

// refusedResponseError is the error MarkResponseRefused returns.
type refusedResponseError struct {
	err error
}

func (e *refusedResponseError) Error() string { return e.err.Error() }

func (e *refusedResponseError) Unwrap() error { return e.err }
