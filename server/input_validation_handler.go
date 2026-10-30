package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pbednarcik/flexmcp/mcp"
)

// InputValidationHandler answers a tools/call whose arguments failed input
// validation. It runs only then, before the middleware, with the request's
// context; it may run concurrently and must be safe for concurrent use. With
// refuse true, result answers the call as a tool execution error, IsError
// forced, limited by WithToolResultSizeLimit; nil answers the default text. A
// result setting Task, InputRequests, RequestState or ResultType is a fault,
// as is a panic, recovered even without WithRecovery: both answer -32603,
// which the error hooks see. With refuse false, result is ignored and the call
// runs as if it had passed, and the tool handler takes on the duty to validate.
type InputValidationHandler func(ctx context.Context, tool mcp.Tool, args CallArguments, failure InputValidationFailure) (result *mcp.CallToolResult, refuse bool)

// CallArguments are a failing call's arguments as the hook sees them; read-only.
type CallArguments struct {
	// Effective is the object validation checked, after any before-call or
	// before-any hook: {} for null or none, nil for a non-object. A number is
	// a float64, or a json.Number under such a hook, in process, or when any
	// number sent has an exponent or over 15 digits: accept both.
	Effective map[string]any
	// Raw is the client's bytes, shared with the request; nil in process.
	Raw json.RawMessage
}

// WithInputValidationHandler enables validation as WithInputSchemaValidation
// does and answers its failures with h; a nil h answers the default refusal.
func WithInputValidationHandler(h InputValidationHandler) ServerOption {
	return func(s *MCPServer) {
		s.inputValidationHandler = h
		WithInputSchemaValidation()(s)
	}
}

// answerInvalidInput answers err: a refusal, proceed, or a fault for -32603.
func (s *MCPServer) answerInvalidInput(ctx context.Context, tool mcp.Tool, request mcp.CallToolRequest, err error) (result *mcp.CallToolResult, proceed bool, fault error) {
	inv, ok := errors.AsType[*inputInvalid](err)
	if s.inputValidationHandler == nil || !ok {
		return validationToolResult(err), false, nil
	}
	result, refuse, fault := callInputValidationHandler(ctx, s.inputValidationHandler, tool, inv, request.Params.RawArguments)
	switch {
	case fault != nil:
		return nil, false, fault
	case !refuse:
		return nil, true, nil
	case result == nil:
		return validationToolResult(err), false, nil
	}
	if field := resultAnswerField(result); field != "" {
		return nil, false, fmt.Errorf("input validation handler of tool %s refused with a result that sets %s", tool.Name, field)
	}
	if !result.IsError {
		// The handler's result may be one it shares, so it is not written to.
		refusal := *result
		refusal.IsError = true
		result = &refusal
	}
	return result, false, nil
}

// callInputValidationHandler calls h on inv, any panic returned as fault.
func callInputValidationHandler(ctx context.Context, h InputValidationHandler, tool mcp.Tool, inv *inputInvalid, raw json.RawMessage) (result *mcp.CallToolResult, refuse bool, fault error) {
	defer func() {
		if r := recover(); r != nil {
			result, refuse, fault = nil, false, fmt.Errorf("panic recovered answering the input validation failure of tool %s: %v", tool.Name, r)
		}
	}()
	effective, _ := inv.instance.(map[string]any)
	result, refuse = h(ctx, tool, CallArguments{Effective: effective, Raw: raw}, projectFailure(inv))
	return result, refuse, nil
}

// resultAnswerField names a task or input-required field result sets, or "".
func resultAnswerField(result *mcp.CallToolResult) string {
	switch {
	case result.Task != nil:
		return "Task"
	case result.InputRequests != nil:
		return "InputRequests"
	case result.RequestState != "":
		return "RequestState"
	case result.ResultType != "":
		return "ResultType"
	}
	return ""
}
