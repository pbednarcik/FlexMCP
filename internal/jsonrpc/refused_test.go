package jsonrpc

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMarkResponseRefusedKeepsTheErrorItMarks(t *testing.T) {
	cause := errors.New("cause")
	err := fmt.Errorf("failed to decode response: %w", cause)
	marked := MarkResponseRefused(err)

	assert.True(t, ResponseRefused(marked))
	assert.True(t, ResponseRefused(fmt.Errorf("outer: %w", marked)))
	assert.False(t, ResponseRefused(err))
	assert.False(t, ResponseRefused(nil))

	assert.Equal(t, err.Error(), marked.Error())
	assert.ErrorIs(t, marked, cause)
	assert.Equal(t, err, errors.Unwrap(marked))
}
