package server

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// typedNilProtocolErrors are nil pointers to each protocol error type, as
// errors: a handler that declares one and returns it by mistake hands the
// server an error whose value-receiver methods panic when called through it.
var typedNilProtocolErrors = []struct {
	name string
	err  error
}{
	{name: "HeaderMismatchError", err: (*mcp.HeaderMismatchError)(nil)},
	{name: "MissingRequiredClientCapabilityError", err: (*mcp.MissingRequiredClientCapabilityError)(nil)},
	{name: "URLElicitationRequiredError", err: (*mcp.URLElicitationRequiredError)(nil)},
	{name: "UnsupportedProtocolVersionError", err: (*mcp.UnsupportedProtocolVersionError)(nil)},
}

// A handler error wrapping a nil pointer to a protocol error type carries
// no condition the client could act on, so it is an internal error with the
// fixed message, as is a chain joining one with a known error.
func TestHandlerErrorWrappingTypedNilProtocolError(t *testing.T) {
	for _, tt := range typedNilProtocolErrors {
		t.Run(tt.name, func(t *testing.T) {
			wrapped := fmt.Errorf("checking access: %w", tt.err)
			var got *requestError
			require.NotPanics(t, func() { got = handlerError(t.Context(), 1, wrapped) })
			require.NotNil(t, got)
			assert.Equal(t, mcp.INTERNAL_ERROR, got.code)
			require.NotNil(t, got.message)
			assert.Equal(t, internalErrorMessage, *got.message)
			assert.Nil(t, got.data)

			for _, known := range knownErrors {
				var message string
				require.NotPanics(t, func() { message = clientMessage(errors.Join(tt.err, fmt.Errorf("then: %w", known))) })
				assert.Equal(t, internalErrorMessage, message)
			}
		})
	}
}

// A tool handler that returns a wrapped nil pointer to a protocol error type
// gets a well-formed internal-error response carrying the request's id.
func TestToolHandlerReturningTypedNilProtocolError(t *testing.T) {
	srv := NewMCPServer("test-server", "1.0.0", WithToolCapabilities(true))
	for _, tt := range typedNilProtocolErrors {
		srv.AddTool(mcp.NewTool(tt.name), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, fmt.Errorf("checking access: %w", tt.err)
		})
	}
	for _, tt := range typedNilProtocolErrors {
		t.Run(tt.name, func(t *testing.T) {
			var errorResponse mcp.JSONRPCError
			require.NotPanics(t, func() {
				errorResponse = handleForError(t, srv, fmt.Sprintf(
					`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":%q}}`, tt.name))
			})
			assert.Equal(t, mcp.INTERNAL_ERROR, errorResponse.Error.Code)
			assert.Equal(t, internalErrorMessage, errorResponse.Error.Message)
			assert.Nil(t, errorResponse.Error.Data)
		})
	}
}
