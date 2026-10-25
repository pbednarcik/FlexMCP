package server

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A handler may return the missing-capability error as a value or as a
// pointer, bare or wrapped: either way the client gets -32021 carrying the
// required capabilities and the error's own text, never an internal error.
func TestHandlerErrorMissingCapabilityPointer(t *testing.T) {
	missing := mcp.MissingRequiredClientCapabilityError{Required: mcp.ClientCapabilities{Roots: &struct {
		ListChanged bool `json:"listChanged,omitempty"`
	}{}}}
	// The pointer as an error value: vet's printf check rejects %w on a
	// pointer to a value-receiver error.
	var pointer error = &missing
	tests := []struct {
		name string
		err  error
	}{
		{name: "value", err: missing},
		{name: "wrapped value", err: fmt.Errorf("checking: %w", missing)},
		{name: "pointer", err: pointer},
		{name: "wrapped pointer", err: fmt.Errorf("checking: %w", pointer)},
	}
	want, err := json.Marshal(missing.JSONRPCError().Error.Data)
	require.NoError(t, err)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := handlerError(t.Context(), 1, tt.err)
			require.NotNil(t, got)
			assert.Equal(t, mcp.MISSING_REQUIRED_CLIENT_CAPABILITY, got.code)
			require.NotNil(t, got.message)
			assert.Equal(t, missing.JSONRPCError().Error.Message, *got.message)
			data, err := json.Marshal(got.data)
			require.NoError(t, err)
			assert.JSONEq(t, string(want), string(data))
			assert.Equal(t, missing.Error(), clientMessage(tt.err))
		})
	}
}
