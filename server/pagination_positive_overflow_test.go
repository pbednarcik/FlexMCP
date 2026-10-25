package server

import (
	"encoding/base64"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// TestPaginationLargePositiveLimitAfterCursor pins that a limit near MaxInt
// does not overflow once a cursor moves the start past zero.
func TestPaginationLargePositiveLimitAfterCursor(t *testing.T) {
	s := NewMCPServer("test", "1", WithPaginationLimit(math.MaxInt))
	items := []mcp.Prompt{{Name: "a"}, {Name: "b"}}
	cursor := mcp.Cursor(base64.StdEncoding.EncodeToString([]byte("a")))
	got, next, err := listByPagination(t.Context(), s, &cursor, items)
	require.NoError(t, err)
	require.Equal(t, []mcp.Prompt{{Name: "b"}}, got)
	require.Nil(t, next)
}
