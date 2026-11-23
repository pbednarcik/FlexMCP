package server

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/pbednarcik/flexmcp/mcp"
)

// WithInputValidationInsideMiddleware turns input validation on, as
// WithInputSchemaValidation does, and runs it, and the input validation
// handler, as the innermost link of the tool handler middleware chain: every
// middleware sees a refused call as it sees any other, its refusal as the
// result, its context as the context the handler would have had.
//
// Validation reads the call the chain hands it. A middleware that hands down
// a new map in Params.Arguments has that map validated, read as a hook before
// the call has its own. A change made in place to the decoded map is
// validated only when every number the client sent was exact; when one was
// not, written with an exponent or over 15 digits, the bytes the client sent
// are validated and the change reaches the handler unchecked. To change the
// arguments, hand down a new map. A middleware that hands down new
// RawArguments has the Arguments it left validated, not the new bytes, so
// BindArguments then reads bytes no one validated, as under such a hook. A
// middleware that answers without calling the rest of the chain leaves the
// call unvalidated, since no handler runs. A fault of the input validation
// handler reaches the middleware as an error, which the client gets as an
// internal error unless a middleware answers it.
//
// The middleware now sees calls whose arguments fail the schema, so one that
// reads Params.Arguments must expect any value there. The task path of the
// 2025-11-25 protocol version is unchanged: a task-augmented call is still
// validated before its task is created, and before the middleware. Its order
// among the options doesn't matter.
func WithInputValidationInsideMiddleware() ServerOption {
	return func(s *MCPServer) {
		s.validateInChain = true
		WithInputSchemaValidation()(s)
	}
}

// validatingHandler returns next behind input validation, for a call of the
// registered tool whose schema registration kept (schema, kept): the request
// the chain hands it is validated and, when it fails, answered as the
// dispatcher answers it without the option, the refusal returned as the
// result and a fault of the input validation handler as the error. sentRaw
// and sentArguments are the call's RawArguments and Arguments as the
// dispatcher held them: a request that still holds both is validated as the
// dispatcher would have validated it, one whose middleware replaced either as
// a round trip of its Arguments, as under a hook before the call.
//
// Every parameter is a word or a few, never reassigned, so the closure holds
// them by value: building it is the one allocation the option adds to a call.
func (s *MCPServer) validatingHandler(registered *ServerTool, schema *cachedToolSchema, kept bool, sentRaw json.RawMessage, sentArguments any, next ToolHandlerFunc) ToolHandlerFunc {
	return func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		roundTrip := hooksSeeCallArguments(s.hooks) ||
			!sameBytes(request.Params.RawArguments, sentRaw) ||
			!sameArguments(request.Params.Arguments, sentArguments)
		ran, err := s.inputValidator.validateRegistered(registered.Tool, schema, kept, argumentsToValidateAs(request, registered.ArgumentsOnDemand, roundTrip))
		if ran {
			markRoundTripAs(&request.Params, roundTrip)
		}
		if err != nil {
			refusal, proceed, fault := s.answerInvalidInput(ctx, registered.Tool, request, err)
			if fault != nil {
				return nil, fault
			}
			if !proceed {
				return refusal, nil
			}
		}
		return next(ctx, request)
	}
}

// sameBytes reports whether a and b are the same slice of the same bytes.
func sameBytes(a, b []byte) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}

// sameArguments reports whether seen is sent itself, sent being a call's
// Arguments as its decode left them: the same map or slice, or an equal
// scalar. Any other value, a map of another type among them, is not.
func sameArguments(seen, sent any) bool {
	switch sent := sent.(type) {
	case nil:
		return seen == nil
	case map[string]any:
		m, ok := seen.(map[string]any)
		return ok && reflect.ValueOf(m).UnsafePointer() == reflect.ValueOf(sent).UnsafePointer()
	case []any:
		l, ok := seen.([]any)
		return ok && len(l) == len(sent) && (len(l) == 0 || &l[0] == &sent[0])
	case string, float64, bool:
		// sent's type is comparable, so the comparison cannot panic.
		return seen == any(sent)
	}
	return false
}
