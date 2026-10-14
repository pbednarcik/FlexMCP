package server

import (
	"context"
	"errors"
	"reflect"

	"github.com/mark3labs/mcp-go/mcp"
)

// JSONRPCErrorProvider is an error with a JSON-RPC form of its own: the code,
// message and data a client should receive for it.
type JSONRPCErrorProvider interface {
	error
	JSONRPCError() mcp.JSONRPCError
}

// exposedHandlerError is a handler error that NewHandlerError marked as safe
// to send to the client in its JSON-RPC form.
type exposedHandlerError struct {
	provider JSONRPCErrorProvider
}

// Error returns the provider's error text.
func (e *exposedHandlerError) Error() string {
	return e.provider.Error()
}

// Unwrap returns the provider, so errors.Is and errors.As still find it.
func (e *exposedHandlerError) Unwrap() error {
	return e.provider
}

// NewHandlerError marks provider as safe to send to the client in its
// JSON-RPC form. A tool, prompt or resource handler that returns the result,
// directly or wrapped, answers the request with the provider's code, message
// and data instead of an internal error.
//
// Errors are not sent in their JSON-RPC form unless marked, because one may
// have come from another server, and its code, message and data were meant
// for that server's client.
//
// A provider whose code has a protocol meaning is still an internal error,
// because the client would act on it as that condition: every code from
// -32020 to -32099, which the MCP specification keeps for itself (among them
// mcp.HEADER_MISMATCH, mcp.MISSING_REQUIRED_CLIENT_CAPABILITY,
// mcp.UNSUPPORTED_PROTOCOL_VERSION and mcp.URL_ELICITATION_REQUIRED), and
// mcp.RESOURCE_NOT_FOUND and mcp.REQUEST_INTERRUPTED. To report a missing
// client capability or a required URL elicitation, return
// mcp.MissingRequiredClientCapabilityError or mcp.URLElicitationRequiredError
// themselves; the server sends those in the form the request's protocol
// version calls for. Standard JSON-RPC codes such as mcp.INVALID_PARAMS are
// sent as given.
//
// NewHandlerError returns nil for a nil provider, including a typed nil.
func NewHandlerError(provider JSONRPCErrorProvider) error {
	if provider == nil {
		return nil
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return nil
		}
	}
	return &exposedHandlerError{provider: provider}
}

// handlerError reports an error returned by a tool, prompt or resource
// handler, which is otherwise an internal error.
//
// A handler returns mcp.URLElicitationRequiredError when the request cannot
// proceed until the user completes a URL elicitation. Protocol version
// 2025-11-25 reports that as -32042, carrying the elicitations the client
// has to start. Protocol version 2026-07-28 reserves the code and asks for
// the elicitation through a multi round-trip request instead, so the code is
// chosen per request, as for resourceNotFoundCode.
//
// mcp.MissingRequiredClientCapabilityError, from resolveMultiRoundTrip or
// from a handler itself, is -32021 carrying the required capabilities.
//
// An error marked with NewHandlerError is sent in its own JSON-RPC form,
// unless its code is one reservedHandlerErrorCode keeps for the server.
func handlerError(ctx context.Context, id any, err error) *requestError {
	if required, ok := asURLElicitationRequired(err); ok {
		if !mcp.IsModernProtocol(RequestProtocolVersion(ctx)) {
			details := required.JSONRPCError().Error
			return &requestError{id: id, code: details.Code, err: err, data: details.Data}
		}
		// Marking it with NewHandlerError does not make -32042 an answer a
		// modern client can act on.
		return &requestError{id: id, code: mcp.INTERNAL_ERROR, err: err}
	}
	if missing, ok := errors.AsType[mcp.MissingRequiredClientCapabilityError](err); ok {
		details := missing.JSONRPCError().Error
		return &requestError{id: id, code: details.Code, err: err, data: details.Data}
	}
	if exposed, ok := errors.AsType[*exposedHandlerError](err); ok {
		details := exposed.provider.JSONRPCError().Error
		if reservedHandlerErrorCode(details.Code) {
			return &requestError{id: id, code: mcp.INTERNAL_ERROR, err: err}
		}
		return &requestError{
			id:      id,
			code:    details.Code,
			err:     err,
			message: &details.Message,
			data:    details.Data,
		}
	}
	return &requestError{id: id, code: mcp.INTERNAL_ERROR, err: err}
}

// reservedHandlerErrorCode reports whether code is one a handler error marked
// with NewHandlerError may not claim: the codes from -32020 to -32099, which
// belong to the MCP specification and are sent only with the meaning it gives
// them, and the retired resource-not-found and request-interrupted codes,
// which clients still act on. The server reports those conditions itself, in
// the form the request's protocol version calls for.
func reservedHandlerErrorCode(code int) bool {
	if code >= -32099 && code <= -32020 {
		return true
	}
	return code == mcp.RESOURCE_NOT_FOUND || code == mcp.REQUEST_INTERRUPTED
}

// asURLElicitationRequired finds a URLElicitationRequiredError in err's chain,
// whether the handler returned the value or a pointer to it.
func asURLElicitationRequired(err error) (mcp.URLElicitationRequiredError, bool) {
	var required mcp.URLElicitationRequiredError
	if errors.As(err, &required) {
		return required, true
	}
	var pointer *mcp.URLElicitationRequiredError
	if errors.As(err, &pointer) && pointer != nil {
		return *pointer, true
	}
	return required, false
}
