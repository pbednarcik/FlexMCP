package mcp

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// errors.Is matches each protocol error type against a target given as a
// value or as a non-nil pointer, as the Is helpers find the error given
// either way; a nil pointer target matches nothing.
func TestProtocolErrorIsAcceptsPointerTarget(t *testing.T) {
	tests := []struct {
		name       string
		value      error
		pointer    error
		target     error
		nilTarget  error
		otherValue error
	}{
		{
			name:       "HeaderMismatchError",
			value:      HeaderMismatchError{Header: "MCP-Protocol-Version", Reason: "disagrees"},
			pointer:    &HeaderMismatchError{Header: "MCP-Protocol-Version", Reason: "disagrees"},
			target:     &HeaderMismatchError{},
			nilTarget:  (*HeaderMismatchError)(nil),
			otherValue: UnsupportedProtocolVersionError{Version: "2099-01-01"},
		},
		{
			name:       "MissingRequiredClientCapabilityError",
			value:      MissingRequiredClientCapabilityError{Required: ClientCapabilities{Elicitation: &ElicitationCapability{}}},
			pointer:    &MissingRequiredClientCapabilityError{Required: ClientCapabilities{Elicitation: &ElicitationCapability{}}},
			target:     &MissingRequiredClientCapabilityError{},
			nilTarget:  (*MissingRequiredClientCapabilityError)(nil),
			otherValue: HeaderMismatchError{Reason: "missing"},
		},
		{
			name:       "URLElicitationRequiredError",
			value:      URLElicitationRequiredError{Elicitations: []ElicitationParams{{ElicitationID: "auth-1"}}},
			pointer:    &URLElicitationRequiredError{Elicitations: []ElicitationParams{{ElicitationID: "auth-1"}}},
			target:     &URLElicitationRequiredError{},
			nilTarget:  (*URLElicitationRequiredError)(nil),
			otherValue: MissingRequiredClientCapabilityError{},
		},
		{
			name:       "UnsupportedProtocolVersionError",
			value:      UnsupportedProtocolVersionError{Version: "2099-01-01"},
			pointer:    &UnsupportedProtocolVersionError{Version: "2099-01-01"},
			target:     &UnsupportedProtocolVersionError{},
			nilTarget:  (*UnsupportedProtocolVersionError)(nil),
			otherValue: URLElicitationRequiredError{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.True(t, errors.Is(fmt.Errorf("serving: %w", tt.value), tt.target))
			assert.True(t, errors.Is(fmt.Errorf("serving: %w", tt.pointer), tt.target))
			assert.False(t, errors.Is(fmt.Errorf("serving: %w", tt.value), tt.nilTarget))
			assert.False(t, errors.Is(fmt.Errorf("serving: %w", tt.otherValue), tt.target))
		})
	}
}
