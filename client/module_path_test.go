package client

import (
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// modulePath is the module this SDK is built as: the test binary's own build
// information names it as the main module.
func TestModulePathIsTheBuiltModule(t *testing.T) {
	info, ok := debug.ReadBuildInfo()
	require.True(t, ok, "the test binary carries build information")
	assert.Equal(t, info.Main.Path, modulePath)
}
