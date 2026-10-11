package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors for common JSON-RPC error codes.
var (
	// ErrParseError indicates a JSON parsing error (code: PARSE_ERROR).
	ErrParseError = errors.New("parse error")

	// ErrInvalidRequest indicates an invalid JSON-RPC request (code: INVALID_REQUEST).
	ErrInvalidRequest = errors.New("invalid request")

	// ErrMethodNotFound indicates the requested method does not exist (code: METHOD_NOT_FOUND).
	ErrMethodNotFound = errors.New("method not found")

	// ErrInvalidParams indicates invalid method parameters (code: INVALID_PARAMS).
	ErrInvalidParams = errors.New("invalid params")

	// ErrInternalError indicates an internal JSON-RPC error (code: INTERNAL_ERROR).
	ErrInternalError = errors.New("internal error")

	// ErrRequestInterrupted indicates a request was cancelled or timed out (code: REQUEST_INTERRUPTED).
	ErrRequestInterrupted = errors.New("request interrupted")

	// ErrResourceNotFound indicates a requested resource was not found (code: RESOURCE_NOT_FOUND).
	ErrResourceNotFound = errors.New("resource not found")

	// ErrEmbeddedResourceMissingVariant indicates an embedded resource has neither text nor blob content.
	ErrEmbeddedResourceMissingVariant = errors.New("missing text or blob field")

	// ErrEmbeddedResourceMissingURI indicates an embedded resource content variant has no URI.
	ErrEmbeddedResourceMissingURI = errors.New("resource uri is missing")
)

// URLElicitationRequiredError is returned when the server requires URL elicitation to proceed.
type URLElicitationRequiredError struct {
	Elicitations []ElicitationParams `json:"elicitations"`
}

func (e URLElicitationRequiredError) Error() string {
	return fmt.Sprintf("URL elicitation required: %d elicitation(s) needed", len(e.Elicitations))
}

func (e URLElicitationRequiredError) JSONRPCError() JSONRPCError {
	return JSONRPCError{
		JSONRPC: JSONRPC_VERSION,
		Error: JSONRPCErrorDetails{
			Code:    URL_ELICITATION_REQUIRED,
			Message: e.Error(),
			Data: map[string]any{
				"elicitations": e.Elicitations,
			},
		},
	}
}

// UnsupportedProtocolVersionError is returned when a peer responds with, or
// rejects, a protocol version that this side does not support.
type UnsupportedProtocolVersionError struct {
	// Version is the protocol version that was rejected.
	Version string
	// Supported lists the versions the peer does support, when it told us.
	Supported []string
}

func (e UnsupportedProtocolVersionError) Error() string {
	if len(e.Supported) > 0 {
		return fmt.Sprintf("unsupported protocol version: %q (supported: %v)", e.Version, e.Supported)
	}
	return fmt.Sprintf("unsupported protocol version: %q", e.Version)
}

// UnsupportedProtocolVersionData is the JSON payload carried in the data field
// of an [UNSUPPORTED_PROTOCOL_VERSION] error.
type UnsupportedProtocolVersionData struct {
	// Supported lists the protocol versions the server implements.
	Supported []string `json:"supported"`
	// Requested is the version the rejected request declared.
	Requested string `json:"requested"`
}

// JSONRPCError renders the error as a JSON-RPC error response body.
func (e UnsupportedProtocolVersionError) JSONRPCError() JSONRPCError {
	return JSONRPCError{
		JSONRPC: JSONRPC_VERSION,
		Error: JSONRPCErrorDetails{
			Code:    UNSUPPORTED_PROTOCOL_VERSION,
			Message: e.Error(),
			Data: UnsupportedProtocolVersionData{
				Supported: e.Supported,
				Requested: e.Version,
			},
		},
	}
}

// HeaderMismatchError is returned when a standard MCP HTTP header is missing,
// malformed, or disagrees with the corresponding value in the request body
// (SEP-2243).
type HeaderMismatchError struct {
	// Header is the offending header name.
	Header string
	// Reason describes the mismatch.
	Reason string
}

func (e HeaderMismatchError) Error() string {
	if e.Header == "" {
		return fmt.Sprintf("header mismatch: %s", e.Reason)
	}
	return fmt.Sprintf("header mismatch: %s: %s", e.Header, e.Reason)
}

// Is implements the errors.Is interface for better error handling.
func (e HeaderMismatchError) Is(target error) bool {
	_, ok := target.(HeaderMismatchError)
	return ok
}

// MissingRequiredClientCapabilityError is returned when a request omits a
// client capability the server needs to serve it (SEP-2575): the server
// never relies on a capability the request did not declare, and answers
// -32021 naming the capabilities it would need.
type MissingRequiredClientCapabilityError struct {
	// Required holds the capabilities the request would have to declare,
	// as a ClientCapabilities object: elicitation (with url for URL mode),
	// sampling (with tools), roots.
	Required ClientCapabilities
}

func (e MissingRequiredClientCapabilityError) Error() string {
	names := requiredCapabilityNames(e.Required)
	if len(names) == 0 {
		return "missing required client capability"
	}
	return "missing required client capability: " + strings.Join(names, ", ")
}

// requiredCapabilityNames spells the capabilities a ClientCapabilities value
// declares, as dotted paths, for an error message.
func requiredCapabilityNames(c ClientCapabilities) []string {
	var names []string
	if c.Elicitation != nil {
		if c.Elicitation.URL != nil {
			names = append(names, "elicitation.url")
		} else {
			names = append(names, "elicitation")
		}
	}
	if c.Sampling != nil {
		switch {
		case c.Sampling.Tools != nil:
			names = append(names, "sampling.tools")
		case c.Sampling.Context != nil:
			names = append(names, "sampling.context")
		default:
			names = append(names, "sampling")
		}
	}
	if c.Roots != nil {
		names = append(names, "roots")
	}
	return names
}

// MissingRequiredClientCapabilityData is the data member of a
// [MISSING_REQUIRED_CLIENT_CAPABILITY] error.
type MissingRequiredClientCapabilityData struct {
	// RequiredCapabilities is the set the request would have to declare.
	RequiredCapabilities ClientCapabilities `json:"requiredCapabilities"`
}

// JSONRPCError renders the error as a JSON-RPC error response body.
func (e MissingRequiredClientCapabilityError) JSONRPCError() JSONRPCError {
	return JSONRPCError{
		JSONRPC: JSONRPC_VERSION,
		Error: JSONRPCErrorDetails{
			Code:    MISSING_REQUIRED_CLIENT_CAPABILITY,
			Message: e.Error(),
			Data:    MissingRequiredClientCapabilityData{RequiredCapabilities: e.Required},
		},
	}
}

// Is implements the errors.Is interface for better error handling.
func (e MissingRequiredClientCapabilityError) Is(target error) bool {
	_, ok := target.(MissingRequiredClientCapabilityError)
	return ok
}

// Is implements the errors.Is interface for better error handling
func (e URLElicitationRequiredError) Is(target error) bool {
	_, ok := target.(URLElicitationRequiredError)
	return ok
}

// Is implements the errors.Is interface for better error handling
func (e UnsupportedProtocolVersionError) Is(target error) bool {
	_, ok := target.(UnsupportedProtocolVersionError)
	return ok
}

// IsUnsupportedProtocolVersion checks if an error is an UnsupportedProtocolVersionError
func IsUnsupportedProtocolVersion(err error) bool {
	var target UnsupportedProtocolVersionError
	return errors.As(err, &target)
}

// IsHeaderMismatch checks if an error is a [HeaderMismatchError].
func IsHeaderMismatch(err error) bool {
	var target HeaderMismatchError
	return errors.As(err, &target)
}

// IsMissingRequiredClientCapability checks if an error is a
// [MissingRequiredClientCapabilityError].
func IsMissingRequiredClientCapability(err error) bool {
	var target MissingRequiredClientCapabilityError
	return errors.As(err, &target)
}

// AsError maps JSONRPCErrorDetails to a Go error.
// Returns sentinel errors wrapped with custom messages for known codes.
// Defaults to a generic error with the original message when the code is not mapped.
func (e *JSONRPCErrorDetails) AsError() error {
	var err error

	switch e.Code {
	case PARSE_ERROR:
		err = ErrParseError
	case INVALID_REQUEST:
		err = ErrInvalidRequest
	case METHOD_NOT_FOUND:
		err = ErrMethodNotFound
	case INVALID_PARAMS:
		err = ErrInvalidParams
	case INTERNAL_ERROR:
		err = ErrInternalError
	case REQUEST_INTERRUPTED:
		err = ErrRequestInterrupted
	case RESOURCE_NOT_FOUND:
		err = ErrResourceNotFound
	case HEADER_MISMATCH:
		return HeaderMismatchError{Reason: e.Message}
	case MISSING_REQUIRED_CLIENT_CAPABILITY:
		var data MissingRequiredClientCapabilityData
		if e.Data != nil {
			if dataBytes, marshalErr := json.Marshal(e.Data); marshalErr == nil {
				_ = json.Unmarshal(dataBytes, &data)
			}
		}
		return MissingRequiredClientCapabilityError{Required: data.RequiredCapabilities}
	case UNSUPPORTED_PROTOCOL_VERSION:
		var data UnsupportedProtocolVersionData
		if e.Data != nil {
			if dataBytes, marshalErr := json.Marshal(e.Data); marshalErr == nil {
				_ = json.Unmarshal(dataBytes, &data)
			}
		}
		return UnsupportedProtocolVersionError{
			Version:   data.Requested,
			Supported: data.Supported,
		}
	case URL_ELICITATION_REQUIRED:
		// Attempt to reconstruct URLElicitationRequiredError from Data
		if e.Data != nil {
			// Round-trip through JSON to parse into struct
			// This handles both map[string]any (from unmarshal) and other forms
			if dataBytes, marshalErr := json.Marshal(e.Data); marshalErr == nil {
				var data struct {
					Elicitations []ElicitationParams `json:"elicitations"`
				}
				if unmarshalErr := json.Unmarshal(dataBytes, &data); unmarshalErr == nil {
					return URLElicitationRequiredError{
						Elicitations: data.Elicitations,
					}
				}
			}
		}
		// Fallback if data is missing or invalid
		return URLElicitationRequiredError{}
	default:
		return errors.New(e.Message)
	}

	// Wrap the sentinel error with the custom message if it differs from the sentinel.
	if e.Message != "" && e.Message != err.Error() {
		return fmt.Errorf("%w: %s", err, e.Message)
	}

	return err
}
