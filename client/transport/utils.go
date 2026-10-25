package transport

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/pbednarcik/flexmcp/mcp"
)

// jsonrpcVersion is a message's jsonrpc member as decoded: whether it is
// "2.0", which every JSON-RPC 2.0 message MUST carry, keeping none of its
// bytes. An absent member, another string or another type is not. The client
// judges only the server requests it answers: one that fails is answered as
// invalid.
type jsonrpcVersion struct{ is20 bool }

// UnmarshalJSON records whether data is "2.0"; a string with escapes is
// decoded to tell.
func (v *jsonrpcVersion) UnmarshalJSON(data []byte) error {
	v.is20 = string(data) == `"2.0"`
	if !v.is20 && bytes.IndexByte(data, '\\') >= 0 {
		var s string
		v.is20 = json.Unmarshal(data, &s) == nil && s == mcp.JSONRPC_VERSION
	}
	return nil
}

// invalidVersionResponse answers a server request whose jsonrpc member is
// not "2.0".
func invalidVersionResponse(id mcp.RequestId) *JSONRPCResponse {
	return NewJSONRPCErrorResponse(id, mcp.INVALID_REQUEST, `Invalid request: jsonrpc must be "2.0"`, nil)
}

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
