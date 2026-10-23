package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
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
// bytes. The validity check is skipped for a message the envelope reads, so
// the seeds hide invalid JSON where that read could pass over it.
func FuzzStdioMessage(f *testing.F) {
	for _, line := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`, ` {"a":1} `, "\t[1]\r\n", `"x"`, `1`, `null`,
		`{"a":1,"a":2}`, "{\"s\":\"\xff\"}", `{"s":"\ud800"}`, `{`, `{} {}`, " {}", `{}` + "\u0085",
		``, ` `, `nul`, `[1,]`,
		`{"id":1,"method":"ping","params":{"x":[1,}}`, `{"id":1,"method":"ping","params":{"x":tru}}`,
		`{"id":1,"method":"ping"}x`, `{"id":1,"method":"ping","x":"a` + "\x01" + `"}`, `{"id":1,"x":"\q"}`,
		`{"id":1,"x":NaN}`, `{"id":1,"x":01}`, `{"id":1,"x":1.}`, `{"id":1,"x":-}`, `{"id":1,/**/"x":1}`,
		`{'id':1}`, "\ufeff{}", `{"id":1,"result":{"a":}}`, `{"method":"ping","params":{"_meta":{"a":[}}}`,
	} {
		f.Add(line)
	}
	f.Fuzz(func(t *testing.T, line string) {
		want, wantOK := stdioMessageByUnmarshal(line)
		got, _, _, _, gotOK := stdioMessage([]byte(line))
		require.Equal(t, wantOK, gotOK, "taken for %q", line)
		assert.Equal(t, string(want), string(got), "bytes for %q", line)
	})
}

// The read loop takes each line as it came, newline included, however long,
// in bytes of its own; a last line without a newline comes with io.EOF.
func TestReadInputLine(t *testing.T) {
	long := strings.Repeat("x", 3*4096+7)
	r := bufio.NewReaderSize(strings.NewReader("a\n"+long+"\r\n\nlast"), 16)
	var buf, line []byte
	var lines [][]byte
	var err error
	for err == nil {
		line, buf, err = readInputLine(r, buf)
		lines = append(lines, line)
	}
	require.ErrorIs(t, err, io.EOF)
	want := []string{"a\n", long + "\r\n", "\n", "last"}
	require.Len(t, lines, len(want))
	for i := range want {
		assert.Equal(t, want[i], string(lines[i]), "line %d", i)
	}
}

// The buffer a line is read into is kept for the next one, unless the line
// was longer than the read loop keeps a buffer for.
func TestReadInputLineKeepsABufferUpToTheCap(t *testing.T) {
	for _, tt := range []struct {
		name string
		size int
		kept bool
	}{
		{"a long line", 64 << 10, true},
		{"a line past the cap", maxRetainedInputLine + 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(strings.Repeat("x", tt.size) + "\n"))
			line, buf, err := readInputLine(r, nil)
			require.NoError(t, err)
			assert.Len(t, line, tt.size+1)
			assert.Equal(t, tt.kept, buf != nil)
		})
	}
}

// repeatReader reads line over and over, without allocating.
type repeatReader struct {
	line []byte
	off  int
}

func (r *repeatReader) Read(p []byte) (int, error) {
	n := copy(p, r.line[r.off:])
	r.off = (r.off + n) % len(r.line)
	return n, nil
}

// A line is copied once, however many reads it takes, and the copy is the
// line the read loop hands on.
func TestReadInputLineAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	for _, size := range []int{100, 16 << 10} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			r := bufio.NewReader(&repeatReader{line: []byte(strings.Repeat("x", size) + "\n")})
			var buf []byte
			allocs := testing.AllocsPerRun(1000, func() { _, buf, _ = readInputLine(r, buf) })
			assert.LessOrEqual(t, allocs, 1.0, "allocations per line read")
		})
	}
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

// A response is encoded straight into the write: a large result is not
// copied into a buffer of its own, nor again to add the newline.
func TestStdioWriteResponseAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	s := NewStdioServer(NewMCPServer("t", "1"))
	for _, size := range []int{2, 256 << 10} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			response := createResponse(int64(1), mcp.NewToolResultText(strings.Repeat("z", size)))
			write := func() { _ = s.writeResponse(response, io.Discard) }
			allocs := testing.AllocsPerRun(100, write)
			assert.LessOrEqual(t, allocs, 4.0, "allocations per response written")
			perWrite := bytesPerRun(50, write)
			assert.Less(t, perWrite, uint64(4<<10), "bytes allocated per response written")
		})
	}
}

// bytesPerRun returns the bytes f allocates per call, averaged over runs
// calls after one that warms it up. Like testing.AllocsPerRun, it measures on
// one P: a buffer f takes from a sync.Pool and puts back is kept for the P it
// ran on, so with more Ps a goroutine the scheduler moves between two calls
// finds no buffer and allocates one, the size of the whole response.
func bytesPerRun(runs int, f func()) uint64 {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	f()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		f()
	}
	runtime.ReadMemStats(&after)
	return (after.TotalAlloc - before.TotalAlloc) / uint64(runs)
}

// An event is written as it always was; one that cannot be encoded writes
// nothing.
func TestWriteSSEEventWritesTheEventOrNothing(t *testing.T) {
	notification := mcp.JSONRPCNotification{JSONRPC: mcp.JSONRPC_VERSION, Method: "notifications/message"}
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
	notification := mcp.JSONRPCNotification{JSONRPC: mcp.JSONRPC_VERSION, Method: "notifications/message"}
	allocs := testing.AllocsPerRun(1000, func() { _ = writeSSEEvent(io.Discard, notification) })
	assert.LessOrEqual(t, allocs, 3.0, "allocations per event written")
}
