package server

import (
	"bytes"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A final line without a newline is served: a client that writes its last
// request and closes stdin without a trailing newline still gets its answer.
func TestStdioServesAFinalLineWithoutNewline(t *testing.T) {
	var out bytes.Buffer
	s := NewStdioServer(NewMCPServer("test", "1.0.0"))
	s.SetErrorLogger(log.New(io.Discard, "", 0))
	line := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"_meta":` + modernMetaJSON + `}}`
	require.NoError(t, s.Listen(t.Context(), strings.NewReader(line), &out))
	require.Contains(t, out.String(), `"id":1`, "the last line was dropped with the end of input")
}
