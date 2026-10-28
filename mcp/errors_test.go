package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJSONRPCErrorDetails_AsError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		details         JSONRPCErrorDetails
		expectedType    error
		expectedMessage string
	}{
		{
			name: "parse error with custom message",
			details: JSONRPCErrorDetails{
				Code:    PARSE_ERROR,
				Message: "Custom parse error message",
			},
			expectedType:    ErrParseError,
			expectedMessage: "parse error: Custom parse error message",
		},
		{
			name: "parse error with standard message",
			details: JSONRPCErrorDetails{
				Code:    PARSE_ERROR,
				Message: "parse error",
			},
			expectedType:    ErrParseError,
			expectedMessage: "parse error",
		},
		{
			name: "method not found with custom message",
			details: JSONRPCErrorDetails{
				Code:    METHOD_NOT_FOUND,
				Message: "Custom method not found message",
			},
			expectedType:    ErrMethodNotFound,
			expectedMessage: "method not found: Custom method not found message",
		},
		{
			name: "method not found with standard message",
			details: JSONRPCErrorDetails{
				Code:    METHOD_NOT_FOUND,
				Message: "method not found",
			},
			expectedType:    ErrMethodNotFound,
			expectedMessage: "method not found",
		},
		{
			name: "request interrupted with custom message",
			details: JSONRPCErrorDetails{
				Code:    REQUEST_INTERRUPTED,
				Message: "request was cancelled",
			},
			expectedType:    ErrRequestInterrupted,
			expectedMessage: "request interrupted: request was cancelled",
		},
		{
			name: "resource not found with custom message",
			details: JSONRPCErrorDetails{
				Code:    RESOURCE_NOT_FOUND,
				Message: "resource 'foo' not found",
			},
			expectedType:    ErrResourceNotFound,
			expectedMessage: "resource not found: resource 'foo' not found",
		},
		{
			name: "unknown error code",
			details: JSONRPCErrorDetails{
				Code:    -99999,
				Message: "Unknown error occurred",
			},
			expectedType:    nil, // No sentinel error for unknown codes
			expectedMessage: "Unknown error occurred",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result := tc.details.AsError()

			// Check the full error message
			require.EqualError(t, result, tc.expectedMessage)

			// Check the error type (if expected)
			if tc.expectedType != nil {
				require.True(t, errors.Is(result, tc.expectedType),
					"Expected error to be of type %v", tc.expectedType)
			}
		})
	}
}

func TestJSONRPCErrorDetails_AsError_WithPointer(t *testing.T) {
	t.Parallel()

	details := &JSONRPCErrorDetails{
		Code:    METHOD_NOT_FOUND,
		Message: "Method not found",
		Data:    map[string]string{"extra": "info"},
	}

	result := details.AsError()
	require.True(t, errors.Is(result, ErrMethodNotFound))
}

func TestSentinelErrors(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		err error
		msg string
	}{
		"ErrParseError": {
			err: ErrParseError,
			msg: "parse error",
		},
		"ErrInvalidRequest": {
			err: ErrInvalidRequest,
			msg: "invalid request",
		},
		"ErrMethodNotFound": {
			err: ErrMethodNotFound,
			msg: "method not found",
		},
		"ErrInvalidParams": {
			err: ErrInvalidParams,
			msg: "invalid params",
		},
		"ErrInternalError": {
			err: ErrInternalError,
			msg: "internal error",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.msg, tc.err.Error())
		})
	}
}

func TestErrorChaining(t *testing.T) {
	t.Parallel()

	// Test that errors.Is works correctly with wrapped errors
	details := &JSONRPCErrorDetails{
		Code:    METHOD_NOT_FOUND,
		Message: "Method 'foo' not found",
	}

	err := details.AsError()
	wrappedErr := errors.New("failed to call method: " + err.Error())

	// The wrapped error should not match our sentinel error
	require.False(t, errors.Is(wrappedErr, ErrMethodNotFound))

	// But the original error should
	require.True(t, errors.Is(err, ErrMethodNotFound))
}

func TestURLElicitationRequiredError(t *testing.T) {
	t.Parallel()

	err := URLElicitationRequiredError{
		Elicitations: []ElicitationParams{
			{
				Mode:          ElicitationModeURL,
				ElicitationID: "123",
				URL:           "https://example.com/auth",
				Message:       "Auth required",
			},
		},
	}

	// Test Error() string
	expectedMsg := "URL elicitation required: 1 elicitation(s) needed"
	require.Equal(t, expectedMsg, err.Error())

	// Test JSONRPCError conversion
	jsonRPCError := err.JSONRPCError()
	require.Equal(t, URL_ELICITATION_REQUIRED, jsonRPCError.Error.Code)
	require.Equal(t, expectedMsg, jsonRPCError.Error.Message)

	dataMap, ok := jsonRPCError.Error.Data.(map[string]any)
	require.True(t, ok, "Expected Data to be map[string]any")

	elicitations, ok := dataMap["elicitations"].([]ElicitationParams)
	require.True(t, ok, "Expected elicitations in Data")

	require.Equal(t, 1, len(elicitations))
	require.Equal(t, "123", elicitations[0].ElicitationID)
}

func TestJSONRPCErrorDetails_AsError_URLElicitationRequired(t *testing.T) {
	t.Parallel()

	elicitations := []ElicitationParams{
		{
			Mode:          ElicitationModeURL,
			ElicitationID: "123",
			URL:           "https://example.com/auth",
		},
	}

	details := &JSONRPCErrorDetails{
		Code:    URL_ELICITATION_REQUIRED,
		Message: "URL elicitation required...",
		Data: map[string]any{
			"elicitations": elicitations,
		},
	}

	err := details.AsError()
	require.Error(t, err)

	var urlErr URLElicitationRequiredError
	require.True(t, errors.As(err, &urlErr), "Expected error to be URLElicitationRequiredError")
	require.Equal(t, 1, len(urlErr.Elicitations))
	require.Equal(t, "123", urlErr.Elicitations[0].ElicitationID)
	require.Equal(t, "https://example.com/auth", urlErr.Elicitations[0].URL)
}

func TestHeaderMismatchErrorRoundTripsThroughJSONRPC(t *testing.T) {
	err := HeaderMismatchError{Header: "MCP-Protocol-Version", Reason: "header 2025-11-25 disagrees with body 2026-07-28"}
	assert.EqualError(t, err, "header mismatch: MCP-Protocol-Version: header 2025-11-25 disagrees with body 2026-07-28")
	assert.EqualError(t, HeaderMismatchError{Reason: "missing"}, "header mismatch: missing")

	wrapped := fmt.Errorf("serving request: %w", err)
	assert.True(t, errors.Is(wrapped, HeaderMismatchError{}))
	assert.True(t, IsHeaderMismatch(wrapped))
	assert.False(t, IsHeaderMismatch(errors.New("other")))

	// The wire carries the header's name in data, so the client can act on
	// which header was wrong; a body without data still yields the typed error.
	wire := err.JSONRPCError().Error
	fromWire := wire.AsError()
	var mismatch HeaderMismatchError
	require.True(t, errors.As(fromWire, &mismatch))
	assert.Equal(t, "MCP-Protocol-Version", mismatch.Header)
	assert.Equal(t, err.Reason, mismatch.Reason)

	details := JSONRPCErrorDetails{Code: HEADER_MISMATCH, Message: err.Error()}
	bare := details.AsError()
	require.True(t, errors.As(bare, &mismatch))
	assert.Empty(t, mismatch.Header)
	assert.Equal(t, err.Error(), mismatch.Reason)
}

func TestMissingRequiredClientCapabilityErrorCarriesTheRequiredSetOverTheWire(t *testing.T) {
	required := ClientCapabilities{Elicitation: &ElicitationCapability{URL: &struct{}{}}}
	err := MissingRequiredClientCapabilityError{Required: required}
	assert.EqualError(t, err, "missing required client capability: elicitation.url")

	wrapped := fmt.Errorf("tools/call: %w", err)
	assert.True(t, errors.Is(wrapped, MissingRequiredClientCapabilityError{}))
	assert.True(t, IsMissingRequiredClientCapability(wrapped))
	assert.False(t, IsMissingRequiredClientCapability(HeaderMismatchError{}))

	// The JSON-RPC form carries the required set as data.requiredCapabilities
	// (BaseProtocol.md row 28); decoded on the far side it comes back typed.
	rendered := err.JSONRPCError()
	assert.Equal(t, MISSING_REQUIRED_CLIENT_CAPABILITY, rendered.Error.Code)
	wire, marshalErr := json.Marshal(rendered.Error)
	require.NoError(t, marshalErr)
	assert.Contains(t, string(wire), `"requiredCapabilities":{"elicitation":{"url":{}}}`)
	var details JSONRPCErrorDetails
	require.NoError(t, json.Unmarshal(wire, &details))
	var decoded MissingRequiredClientCapabilityError
	require.True(t, errors.As(details.AsError(), &decoded))
	require.NotNil(t, decoded.Required.Elicitation)
	assert.NotNil(t, decoded.Required.Elicitation.URL)

	// Without data the error is still typed, just anonymous.
	details.Data = nil
	decoded = MissingRequiredClientCapabilityError{Required: required}
	require.True(t, errors.As(details.AsError(), &decoded))
	assert.Equal(t, ClientCapabilities{}, decoded.Required)
}

func TestAsErrorURLElicitationRequiredWithoutData(t *testing.T) {
	details := JSONRPCErrorDetails{Code: URL_ELICITATION_REQUIRED, Message: "URL elicitation required"}
	err := details.AsError()

	assert.True(t, errors.Is(err, URLElicitationRequiredError{}))
	var typed URLElicitationRequiredError
	require.True(t, errors.As(err, &typed))
	assert.Empty(t, typed.Elicitations)
}

// TestMissingRequiredClientCapabilityErrorNamesExtensions pins that a
// required extension is named in the message, in id order, and carried as
// data.requiredCapabilities.extensions.
func TestMissingRequiredClientCapabilityErrorNamesExtensions(t *testing.T) {
	err := MissingRequiredClientCapabilityError{Required: ClientCapabilities{
		Roots: &struct {
			ListChanged bool `json:"listChanged,omitempty"`
		}{},
		Extensions: map[string]any{ExtensionTasks: map[string]any{}, "example.com/ext": map[string]any{}},
	}}
	assert.EqualError(t, err, "missing required client capability: roots, extension example.com/ext, extension io.modelcontextprotocol/tasks")

	wire, marshalErr := json.Marshal(err.JSONRPCError().Error)
	require.NoError(t, marshalErr)
	assert.Contains(t, string(wire), `"extensions":{"example.com/ext":{},"io.modelcontextprotocol/tasks":{}}`)
}

// A server's -32602 for a task id it does not know, or that is gone, carries
// the reason in data.reason; decoded, it is a TaskRefusedError that is still
// an ErrInvalidParams. A -32602 without a reason, or with another, is not.
func TestTaskRefusedErrorCarriesTheReasonOverTheWire(t *testing.T) {
	decode := func(t *testing.T, wire string) error {
		t.Helper()
		var details JSONRPCErrorDetails
		require.NoError(t, json.Unmarshal([]byte(wire), &details))
		return details.AsError()
	}

	for _, reason := range []string{"unknown", "gone"} {
		t.Run(reason, func(t *testing.T) {
			err := decode(t, `{"code":-32602,"message":"`+reason+`: the words","data":{"reason":"`+reason+`"}}`)

			var refused TaskRefusedError
			require.True(t, errors.As(err, &refused))
			assert.Equal(t, reason, refused.Reason)
			assert.Equal(t, reason+": the words", refused.Message)
			assert.EqualError(t, err, "invalid params: "+reason+": the words")
			assert.True(t, errors.Is(err, ErrInvalidParams), "still an invalid params error")
			assert.True(t, errors.Is(err, TaskRefusedError{}))
			assert.Equal(t, reason == "gone", IsTaskGone(err))
			assert.Equal(t, reason == "gone", IsTaskGone(fmt.Errorf("get task: %w", err)))
		})
	}

	for name, wire := range map[string]string{
		"no data":            `{"code":-32602,"message":"bad"}`,
		"data without it":    `{"code":-32602,"message":"bad","data":{"other":1}}`,
		"another reason":     `{"code":-32602,"message":"bad","data":{"reason":"rude"}}`,
		"data that is a str": `{"code":-32602,"message":"bad","data":"gone"}`,
	} {
		t.Run(name, func(t *testing.T) {
			err := decode(t, wire)

			var refused TaskRefusedError
			assert.False(t, errors.As(err, &refused))
			assert.True(t, errors.Is(err, ErrInvalidParams))
			assert.False(t, IsTaskGone(err))
			assert.EqualError(t, err, "invalid params: bad")
		})
	}
	assert.False(t, IsTaskGone(nil))
	assert.False(t, IsTaskGone(errors.New("gone")))
}
