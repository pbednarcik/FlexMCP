package transport

import (
	"encoding/json"
	"errors"

	"github.com/mark3labs/mcp-go/mcp"
)

// handlerErrorCode is the JSON-RPC code answering a server request whose
// handler failed with err (JSON-RPC 2.0, section 5.1): METHOD_NOT_FOUND for
// an error wrapping mcp.ErrMethodNotFound, INVALID_PARAMS for one wrapping
// mcp.ErrInvalidParams, INTERNAL_ERROR for any other.
func handlerErrorCode(err error) int {
	switch {
	case errors.Is(err, mcp.ErrMethodNotFound):
		return mcp.METHOD_NOT_FOUND
	case errors.Is(err, mcp.ErrInvalidParams):
		return mcp.INVALID_PARAMS
	default:
		return mcp.INTERNAL_ERROR
	}
}

// NewJSONRPCErrorResponse creates a new JSONRPCResponse with an error.
func NewJSONRPCErrorResponse(id mcp.RequestId, code int, message string, data any) *JSONRPCResponse {
	details := mcp.NewJSONRPCErrorDetails(code, message, data)
	return &JSONRPCResponse{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      id,
		Error:   &details,
	}
}

// NewJSONRPCResultResponse creates a new JSONRPCResponse with a result.
func NewJSONRPCResultResponse(id mcp.RequestId, result json.RawMessage) *JSONRPCResponse {
	return &JSONRPCResponse{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      id,
		Result:  result,
	}
}
