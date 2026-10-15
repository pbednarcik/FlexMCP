package server

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stdioMembersByUnmarshal is how the read loop read a message before the core
// took its read: the members it needs, decoded on their own.
func stdioMembersByUnmarshal(message json.RawMessage) (members stdioMembers, ok bool) {
	var m struct {
		Method string          `json:"method"`
		RawID  json.RawMessage `json:"id,omitempty"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  json.RawMessage `json:"error,omitempty"`
		Params struct {
			RequestID json.RawMessage `json:"requestId"`
		} `json:"params"`
	}
	ok = json.Unmarshal(message, &m) == nil
	members.Method, members.RawID, members.Result, members.Error = m.Method, m.RawID, m.Result, m.Error
	members.Params.RequestID = m.Params.RequestID
	return members, ok
}

// The read loop sees what decoding the members on their own gave it, whether
// or not the core can take the read, and the core takes only a whole read.
func FuzzReadStdioMessage(f *testing.F) {
	for _, line := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":"a","method":"tools/call","params":{"name":"echo","arguments":{}}}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":7}}`,
		`{"jsonrpc":"2.0","id":1,"result":{}}`, `{"jsonrpc":"2.0","id":1,"error":{"code":1}}`,
		`{"jsonrpc":5,"id":1,"method":"ping"}`, `{"id":1,"method":"ping","method":"tools/list"}`,
		`{"id":1,"Method":"ping","method":"tools/list"}`, `{"id":1,"METHOD":"ping"}`,
		`{"id":1,"method":"ping","params":[1]}`, `{"id":1,"method":"ping","params":{"_meta":[]}}`,
		`{"id":1,"method":7}`, `{"id":1,"method":null}`, `[1]`, `"x"`, `null`, `{}`,
		`{"id":1,"method":"ping","params":{"requestId":1,"requestId":2}}`,
	} {
		f.Add(line)
	}
	f.Fuzz(func(t *testing.T, line string) {
		message, ok := stdioMessage(line)
		if !ok {
			t.Skip("not JSON")
		}
		want, wantOK := stdioMembersByUnmarshal(message)
		got, read, gotOK := readStdioMessage(message)
		require.Equal(t, wantOK, gotOK, "parsed %s", message)
		if !wantOK {
			return
		}
		assert.Equal(t, want, got, "members of %s", message)
		if read != nil {
			assert.NoError(t, readEnvelope(message, new(requestEnvelope)), "a read the core takes is whole: %s", message)
		}
	})
}

// An OnRequestInitialization hook holds the message and may change it; over
// stdio too, the checks after it read the message it left: here the hook
// changes the protocol version the request names.
func TestStdioReadOnceChecksReadWhatAnInitializationHookLeft(t *testing.T) {
	hooks := &Hooks{}
	hooks.AddOnRequestInitialization(func(_ context.Context, _ any, message any) error {
		if raw, ok := message.(json.RawMessage); ok {
			if i := bytes.Index(raw, []byte(`"2026-07-28"`)); i >= 0 {
				copy(raw[i:], `"2099-01-01"`)
			}
		}
		return nil
	})
	srv := NewMCPServer("test", "1.0.0", WithHooks(hooks))
	line := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{` + readOnceMeta + `}}` + "\n"
	var out bytes.Buffer
	require.NoError(t, NewStdioServer(srv).Listen(t.Context(), strings.NewReader(line), &out))
	assert.Contains(t, out.String(), `"code":-32022`, "the request now names 2099-01-01: %s", out.String())
}

// A request over stdio is read once: the core takes the read loop's read. 30
// allocations when the core read it again, 28 read once, 26 with the core's
// context layers made in one allocation, 23 once the response is queued for
// the writer rather than written here.
func TestStdioReadOnceAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	srv := NewMCPServer("test", "1.0.0")
	s := NewStdioServer(srv)
	// The response is queued for the writer, which Listen starts; here it
	// is taken off the queue instead.
	s.responses = make(chan stdioResponse, 1)
	ctx := srv.WithContext(t.Context(), s.session)
	line := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{` + readOnceMeta + `}}`
	allocs := testing.AllocsPerRun(1000, func() {
		_ = s.processMessage(ctx, line)
		s.requestWg.Wait()
		<-s.responses
	})
	assert.LessOrEqual(t, allocs, 23.0, "allocations per modern ping over stdio")
}
