package transport

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// echoResults answers every request line read from in with a result of a
// size-byte text on out, carrying the request's id. It allocates little, so
// the benchmark's figures are the client's.
func echoResults(in io.Reader, out io.Writer, size int) {
	prefix := []byte(`{"jsonrpc":"2.0","id":`)
	suffix := fmt.Appendf(nil, `,"result":{"content":[{"type":"text","text":%q}]}}`+"\n", strings.Repeat("x", size))
	reader := bufio.NewReader(in)
	var answer []byte
	for {
		line, err := reader.ReadSlice('\n')
		if err != nil {
			return
		}
		var request struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal(line, &request) != nil {
			return
		}
		answer = append(append(append(answer[:0], prefix...), request.ID...), suffix...)
		if _, err := out.Write(answer); err != nil {
			return
		}
	}
}

// BenchmarkStdioRoundTrip is a request answered through the stdio transport
// by an in-memory server: the client's cost per request of writing it, and
// reading, decoding and matching its answer, by the size of the result.
func BenchmarkStdioRoundTrip(b *testing.B) {
	for _, size := range []int{512, 16 << 10, 256 << 10} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			serverIn, clientOut := io.Pipe()
			clientIn, serverOut := io.Pipe()
			go echoResults(serverIn, serverOut, size)
			stdio := NewIO(clientIn, clientOut, nil)
			if err := stdio.Start(b.Context()); err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() {
				_ = stdio.Close()
				_ = serverOut.Close()
			})
			request := JSONRPCRequest{JSONRPC: "2.0", Method: "ping"}
			var id int64
			b.ReportAllocs()
			for b.Loop() {
				id++
				request.ID = mcp.NewRequestId(id)
				if _, err := stdio.SendRequest(b.Context(), request); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// A round trip costs a bounded number of allocations, the in-memory
// server's included: 13; 15 while the response map was keyed by the id's
// String, 16 while the response id was decoded through json.Unmarshal.
func TestStdioRoundTripAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	serverIn, clientOut := io.Pipe()
	clientIn, serverOut := io.Pipe()
	go echoResults(serverIn, serverOut, 512)
	stdio := NewIO(clientIn, clientOut, nil)
	if err := stdio.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdio.Close()
		_ = serverOut.Close()
	})
	request := JSONRPCRequest{JSONRPC: "2.0", Method: "ping"}
	var id int64
	allocs := testing.AllocsPerRun(500, func() {
		id++
		request.ID = mcp.NewRequestId(id)
		if _, err := stdio.SendRequest(t.Context(), request); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 13 {
		t.Errorf("%v allocations per round trip, want at most 13", allocs)
	}
}
