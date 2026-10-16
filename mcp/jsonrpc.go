package mcp

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"strconv"
)

// RequestId is the id of a JSON-RPC message. A request's id is a string or an
// integer, never null (MCP base protocol); only an error answering a request
// whose id could not be read carries a null one. Decoded from the wire, its
// value is a string, an int64, or a json.Number for an integer past int64;
// any other id is refused.
type RequestId struct {
	value any
}

// NewRequestId creates a new RequestId with the given value
func NewRequestId(value any) RequestId {
	return RequestId{value: value}
}

// Value returns the underlying value of the RequestId
func (r RequestId) Value() any {
	return r.value
}

// String returns a string representation of the RequestId. Every Go integer
// type, and a json.Number holding an integer, gives the form the same id
// takes when decoded from the wire, so ids built in process match them.
func (r RequestId) String() string {
	switch v := r.value.(type) {
	case string:
		return "string:" + v
	case int64:
		return "int64:" + strconv.FormatInt(v, 10)
	case int:
		return "int64:" + strconv.FormatInt(int64(v), 10)
	case int32:
		return "int64:" + strconv.FormatInt(int64(v), 10)
	case int16:
		return "int64:" + strconv.FormatInt(int64(v), 10)
	case int8:
		return "int64:" + strconv.FormatInt(int64(v), 10)
	case uint:
		return unsignedIDString(uint64(v))
	case uint64:
		return unsignedIDString(v)
	case uint32:
		return unsignedIDString(uint64(v))
	case uint16:
		return unsignedIDString(uint64(v))
	case uint8:
		return unsignedIDString(uint64(v))
	case json.Number:
		if i, err := strconv.ParseInt(string(v), 10, 64); err == nil {
			return "int64:" + strconv.FormatInt(i, 10)
		}
		return "number:" + string(v)
	case float64:
		if i, ok := integralInt64(v); ok {
			return "int64:" + strconv.FormatInt(i, 10)
		}
		return "float64:" + strconv.FormatFloat(v, 'f', -1, 64)
	case nil:
		return "<nil>"
	default:
		return "unknown:" + fmt.Sprintf("%v", v)
	}
}

// IsNil returns true if the RequestId is nil
func (r RequestId) IsNil() bool {
	return r.value == nil
}

func (r RequestId) MarshalJSON() ([]byte, error) {
	return json.Marshal(r.value)
}

// MarshalJSONTo writes an integer id straight into the encoder; any other
// value, or any id for an encoder that canonicalizes raw integers, as
// MarshalJSON writes it.
func (r RequestId) MarshalJSONTo(enc *jsontext.Encoder) error {
	if i, ok := r.value.(int64); ok {
		if canonical, _ := jsonv2.GetOption(enc.Options(), jsontext.CanonicalizeRawInts); !canonical {
			return enc.WriteToken(jsontext.Int(i))
		}
	}
	encoded, err := r.MarshalJSON()
	if err != nil {
		return err
	}
	return enc.WriteValue(jsontext.Value(encoded))
}

func (r *RequestId) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		r.value = nil
		return nil
	}

	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return fmt.Errorf("invalid request id: %w", err)
		}
		r.value = s
		return nil
	}

	// A request id is a string or an integer (MCP base protocol), and an
	// integer is a plain literal: one with a fraction or an exponent is
	// refused, whole or not, so no id passes through a float64 and none
	// depends on its spelling or its size. One past int64 is kept as its
	// literal. The whole literal is checked first: ParseInt reports a range
	// error as soon as the digits overflow, before it reaches a fraction or an
	// exponent.
	if !isIntegerLiteral(data) {
		return fmt.Errorf("invalid request id: %s", string(data))
	}
	i, err := strconv.ParseInt(string(data), 10, 64)
	if err == nil {
		r.value = i
		return nil
	}
	if errors.Is(err, strconv.ErrRange) {
		r.value = json.Number(data)
		return nil
	}
	return fmt.Errorf("invalid request id: %s", string(data))
}

// isIntegerLiteral reports whether data is a JSON integer: an optional minus,
// then 0 or digits that do not start with 0.
func isIntegerLiteral(data []byte) bool {
	if len(data) > 0 && data[0] == '-' {
		data = data[1:]
	}
	if len(data) == 0 || (data[0] == '0' && len(data) > 1) {
		return false
	}
	for _, c := range data {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// unsignedIDString is String's form of an unsigned integer id: an int64's
// inside its range, the literal's past it, as decoding gives.
func unsignedIDString(u uint64) string {
	if u <= math.MaxInt64 {
		return "int64:" + strconv.FormatInt(int64(u), 10)
	}
	return "number:" + strconv.FormatUint(u, 10)
}

// integralInt64 returns f as an int64 when it is a whole number inside the
// int64 range; Go leaves a conversion outside that range to the platform.
func integralInt64(f float64) (int64, bool) {
	if f != math.Trunc(f) || f < -(1<<63) || f >= 1<<63 {
		return 0, false
	}
	return int64(f), true
}

// JSONRPCRequest represents a request that expects a response.
type JSONRPCRequest struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      RequestId `json:"id"`
	Params  any       `json:"params,omitempty"`
	Request
}

// JSONRPCNotification represents a notification which does not expect a response.
type JSONRPCNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Notification
}

// JSONRPCResponse represents a successful (non-error) response to a request.
type JSONRPCResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      RequestId `json:"id"`
	Result  any       `json:"result"`
}

// JSONRPCError represents a non-successful (error) response to a request.
type JSONRPCError struct {
	JSONRPC string              `json:"jsonrpc"`
	ID      RequestId           `json:"id"`
	Error   JSONRPCErrorDetails `json:"error"`
}

// JSONRPCErrorDetails represents a JSON-RPC error for Go error handling.
// This is separate from the JSONRPCError type which represents the full JSON-RPC error response structure.
type JSONRPCErrorDetails struct {
	// The error type that occurred.
	Code int `json:"code"`
	// A short description of the error. The message SHOULD be limited
	// to a concise single sentence.
	Message string `json:"message"`
	// Additional information about the error. The value of this member
	// is defined by the sender (e.g. detailed error information, nested errors etc.).
	Data any `json:"data,omitempty"`
}

// UnmarshalJSON handles both the standard JSON-RPC error object
// ({"code": -32600, "message": "..."}) and non-compliant servers that
// return the error as a plain string (e.g. "cursor_invalid").
func (e *JSONRPCErrorDetails) UnmarshalJSON(data []byte) error {
	// Try the spec-compliant object shape first.
	type plain JSONRPCErrorDetails
	if err := json.Unmarshal(data, (*plain)(e)); err == nil {
		return nil
	}
	// Some servers (e.g. Slack MCP) return a bare string.
	var msg string
	if err := json.Unmarshal(data, &msg); err != nil {
		return fmt.Errorf("error field is neither an object nor a string: %w", err)
	}
	e.Code = INTERNAL_ERROR
	e.Message = msg
	return nil
}

// Standard JSON-RPC error codes
const (
	// PARSE_ERROR indicates invalid JSON was received by the server.
	PARSE_ERROR = -32700

	// INVALID_REQUEST indicates the JSON sent is not a valid Request object.
	INVALID_REQUEST = -32600

	// METHOD_NOT_FOUND indicates the method does not exist/is not available.
	METHOD_NOT_FOUND = -32601

	// INVALID_PARAMS indicates invalid method parameter(s).
	INVALID_PARAMS = -32602

	// INTERNAL_ERROR indicates internal JSON-RPC error.
	INTERNAL_ERROR = -32603

	// REQUEST_INTERRUPTED indicates a request was cancelled or timed out.
	REQUEST_INTERRUPTED = -32800
)

// MCP error codes
//
// Protocol version 2026-07-28 partitions the JSON-RPC server-error range:
// -32000 to -32019 remains implementation-defined (existing SDK usage is
// grandfathered) and -32020 to -32099 is reserved for the MCP specification.
const (
	// RESOURCE_NOT_FOUND indicates that the requested resource was not found.
	//
	// Protocol version 2026-07-28 aligns this condition with JSON-RPC by using
	// [INVALID_PARAMS] instead. This code is still emitted to, and accepted
	// from, peers using an earlier version.
	RESOURCE_NOT_FOUND = -32002

	// URL_ELICITATION_REQUIRED is the error code for when URL elicitation is required.
	URL_ELICITATION_REQUIRED = -32042

	// HEADER_MISMATCH indicates that a standard MCP HTTP header is missing,
	// malformed, or does not match the corresponding value in the request body
	// (SEP-2243).
	HEADER_MISMATCH = -32020

	// MISSING_REQUIRED_CLIENT_CAPABILITY indicates the client did not declare a
	// capability the server requires to serve the request (SEP-2575).
	MISSING_REQUIRED_CLIENT_CAPABILITY = -32021

	// UNSUPPORTED_PROTOCOL_VERSION indicates the server does not implement the
	// protocol version the request declared (SEP-2575). The error data carries
	// an [UnsupportedProtocolVersionData].
	UNSUPPORTED_PROTOCOL_VERSION = -32022
)
