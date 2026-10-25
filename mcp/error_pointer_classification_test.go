package mcp

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestErrorClassificationRecognizesPointers pins that the classification
// helpers answer true for a pointer to the error type too, bare, wrapped or
// joined, as they do for the value.
func TestErrorClassificationRecognizesPointers(t *testing.T) {
	tests := []struct {
		name           string
		value, pointer error
		check          func(error) bool
	}{
		{"protocol", UnsupportedProtocolVersionError{}, &UnsupportedProtocolVersionError{}, IsUnsupportedProtocolVersion},
		{"header", HeaderMismatchError{}, &HeaderMismatchError{}, IsHeaderMismatch},
		{"capability", MissingRequiredClientCapabilityError{}, &MissingRequiredClientCapabilityError{}, IsMissingRequiredClientCapability},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.True(t, tt.check(tt.value))
			assert.True(t, tt.check(tt.pointer))
			assert.True(t, tt.check(fmt.Errorf("wrapped: %w", tt.pointer)))
			assert.True(t, tt.check(errors.Join(errors.New("other"), tt.pointer)))
			assert.False(t, tt.check(nil))
			assert.False(t, tt.check(errors.New("unrelated")))
		})
	}
}
