package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stdioMessageByUnmarshal is how a line was read before it was only checked:
// decoded into a json.RawMessage.
func stdioMessageByUnmarshal(line string) (json.RawMessage, bool) {
	var message json.RawMessage
	if err := json.Unmarshal([]byte(line), &message); err != nil {
		return nil, false
	}
	return message, true
}

// A line is taken or refused as decoding it took or refused it, with the same
// bytes.
func FuzzStdioMessage(f *testing.F) {
	for _, line := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`, ` {"a":1} `, "\t[1]\r\n", `"x"`, `1`, `null`,
		`{"a":1,"a":2}`, "{\"s\":\"\xff\"}", `{"s":"\ud800"}`, `{`, `{} {}`, " {}", `{}` + "\u0085",
		``, ` `, `nul`, `[1,]`,
	} {
		f.Add(line)
	}
	f.Fuzz(func(t *testing.T, line string) {
		want, wantOK := stdioMessageByUnmarshal(line)
		got, gotOK := stdioMessage(line)
		require.Equal(t, wantOK, gotOK, "taken for %q", line)
		assert.Equal(t, string(want), string(got), "bytes for %q", line)
	})
}

// A line is copied once, and the copy is the message.
func TestStdioMessageAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	line := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{}}}`
	allocs := testing.AllocsPerRun(1000, func() { _, _ = stdioMessage(line) })
	assert.LessOrEqual(t, allocs, 1.0, "allocations per line read")
}

// writeCounter counts the writes made to it and keeps what they wrote.
type writeCounter struct {
	bytes.Buffer
	writes int
}

func (w *writeCounter) Write(p []byte) (int, error) {
	w.writes++
	return w.Buffer.Write(p)
}

// A response goes out as the encoded message and a newline, in one write: a
// write to stdout is a system call, and lines from concurrent requests must
// not interleave.
func TestStdioWriteResponseIsOneLineInOneWrite(t *testing.T) {
	response := createResponse(int64(1), mcp.NewToolResultText("<a & b>"))
	encoded, err := json.Marshal(response)
	require.NoError(t, err)

	var out writeCounter
	require.NoError(t, NewStdioServer(NewMCPServer("t", "1")).writeResponse(response, &out))
	assert.Equal(t, fmt.Sprintf("%s\n", encoded), out.String())
	assert.Equal(t, 1, out.writes)
}

func TestStdioWriteResponseAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	s := NewStdioServer(NewMCPServer("t", "1"))
	response := createResponse(int64(1), mcp.NewToolResultText("ok"))
	allocs := testing.AllocsPerRun(1000, func() { _ = s.writeResponse(response, io.Discard) })
	assert.LessOrEqual(t, allocs, 8.0, "allocations per response written")
}

// An event is written as it always was; one that cannot be encoded writes
// nothing.
func TestWriteSSEEventWritesTheEventOrNothing(t *testing.T) {
	notification := mcp.JSONRPCNotification{JSONRPC: mcp.JSONRPC_VERSION, Notification: mcp.Notification{Method: "notifications/message"}}
	encoded, err := json.Marshal(notification)
	require.NoError(t, err)

	var out bytes.Buffer
	require.NoError(t, writeSSEEvent(&out, notification))
	assert.Equal(t, fmt.Sprintf("event: message\ndata: %s\n\n", encoded), out.String())

	out.Reset()
	require.Error(t, writeSSEEvent(&out, func() {}))
	assert.Zero(t, out.Len())
}

func TestWriteSSEEventAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	notification := mcp.JSONRPCNotification{JSONRPC: mcp.JSONRPC_VERSION, Notification: mcp.Notification{Method: "notifications/message"}}
	allocs := testing.AllocsPerRun(1000, func() { _ = writeSSEEvent(io.Discard, notification) })
	assert.LessOrEqual(t, allocs, 3.0, "allocations per event written")
}
