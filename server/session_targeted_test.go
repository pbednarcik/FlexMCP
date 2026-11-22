package server

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// targetedStdio is a stdio server on pipes and the one session it registered,
// for the sends that name a session.
type targetedStdio struct {
	server  *MCPServer
	send    func(line string)
	out     <-chan map[string]any
	session atomic.Pointer[ClientSession]
}

func newTargetedStdio(t *testing.T) *targetedStdio {
	t.Helper()
	ts := &targetedStdio{}
	hooks := &Hooks{}
	hooks.AddOnRegisterSession(func(_ context.Context, s ClientSession) { ts.session.Store(&s) })
	ts.server = NewMCPServer("test", "1.0.0", WithToolCapabilities(true), WithPromptCapabilities(true), WithHooks(hooks))
	ts.send, ts.out = stdioPipes(t, ts.server)
	return ts
}

// registered waits for the session to register and returns it.
func (ts *targetedStdio) registered(t *testing.T) ClientSession {
	t.Helper()
	require.Eventually(t, func() bool { return ts.session.Load() != nil }, 5*time.Second, time.Millisecond, "the stdio session registered")
	return *ts.session.Load()
}

// id waits for the session to register and returns its id.
func (ts *targetedStdio) id(t *testing.T) string {
	t.Helper()
	return ts.registered(t).SessionID()
}

// listen opens a tools-only listen stream with the given id and waits for its
// acknowledgement and for the session to hold the stream: the
// acknowledgement is queued before the stream is recorded.
func (ts *targetedStdio) listen(t *testing.T, id string) {
	t.Helper()
	ts.send(`{"jsonrpc":"2.0","id":"` + id + `","method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true},"_meta":` + modernMetaJSON + `}}`)
	awaitMessage(t, ts.out, func(m map[string]any) bool {
		return m["method"] == string(mcp.MethodNotificationSubscriptionsAcknowledged) && streamIDOf(m) == id
	}, "the acknowledgement of "+id)
	streams, ok := ts.registered(t).(SessionWithSubscriptionStreams)
	require.True(t, ok, "the stdio session holds listen streams")
	require.Eventually(t, func() bool {
		open, _ := streams.SubscriptionStreams()
		return slices.ContainsFunc(open, func(s SubscriptionStream) bool { return s.ID == id })
	}, 5*time.Second, time.Millisecond, "the session records stream %s", id)
}

// next returns the next notification the server writes.
func (ts *targetedStdio) next(t *testing.T) map[string]any {
	t.Helper()
	return awaitMessage(t, ts.out, isNotification, "a notification")
}

// quiet fails the test if a notification is written within 200 ms.
func (ts *targetedStdio) quiet(t *testing.T) {
	t.Helper()
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case msg := <-ts.out:
			if isNotification(msg) {
				t.Errorf("unexpected notification %v", msg)
			}
		case <-deadline:
			return
		}
	}
}

// copiesOf waits for a notification of method on each stream of ids, then
// for 200 ms more, and returns how many copies each stream received. Any
// other notification, or one on another stream, fails the test.
func (ts *targetedStdio) copiesOf(t *testing.T, method string, ids ...string) map[string]int {
	t.Helper()
	copies := make(map[string]int, len(ids))
	count := func(msg map[string]any) {
		id, _ := streamIDOf(msg).(string)
		if msg["method"] != method || !slices.Contains(ids, id) {
			t.Errorf("unexpected notification %v", msg)
			return
		}
		copies[id]++
	}
	for _, id := range ids {
		for copies[id] == 0 && !t.Failed() {
			count(ts.next(t))
		}
	}
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case msg := <-ts.out:
			if isNotification(msg) {
				count(msg)
			}
		case <-deadline:
			return copies
		}
	}
}

func isNotification(m map[string]any) bool {
	_, hasMethod := m["method"]
	_, hasID := m["id"]
	return hasMethod && !hasID
}

// streamIDOf is the subscription id in a message's _meta, or nil.
func streamIDOf(m map[string]any) any {
	params, _ := m["params"].(map[string]any)
	meta, _ := params["_meta"].(map[string]any)
	return meta[mcp.MetaKeySubscriptionID]
}

// errorLog is a log message at the threshold of a session that set none, so
// that only the streams decide whether it is delivered.
var errorLog = mcp.NewLoggingMessageNotification(mcp.LoggingLevelError, "test-logger", "boom")

// TestSendNotificationToSpecificClientHonoursListenStreams pins that a
// notification sent to one session outside a request passes the session's
// subscriptions/listen streams as a broadcast does (Patterns.md rows 14 and
// 17): a modern session receives only the types a stream asked for, one copy
// per such stream tagged with its id, and nothing without one; a
// handshake-era session receives it as before, untagged.
func TestSendNotificationToSpecificClientHonoursListenStreams(t *testing.T) {
	tools := string(mcp.MethodNotificationToolsListChanged)
	unrequested := map[string]func(s *MCPServer, id string) error{
		"a list change": func(s *MCPServer, id string) error {
			return s.SendNotificationToSpecificClient(id, string(mcp.MethodNotificationPromptsListChanged), nil)
		},
		"a custom method": func(s *MCPServer, id string) error {
			return s.SendNotificationToSpecificClient(id, "custom/x", nil)
		},
		"a log message": func(s *MCPServer, id string) error {
			return s.SendLogMessageToSpecificClient(id, errorLog)
		},
	}
	for name, sendUnrequested := range unrequested {
		t.Run(name+" the stream did not ask for is not delivered", func(t *testing.T) {
			ts := newTargetedStdio(t)
			ts.listen(t, "listen-1")
			id := ts.id(t)
			require.NoError(t, sendUnrequested(ts.server, id))
			require.NoError(t, ts.server.SendNotificationToSpecificClient(id, tools, nil))
			first := ts.next(t)
			assert.Equal(t, tools, first["method"], "only the requested type is delivered")
			assert.Equal(t, "listen-1", streamIDOf(first), "the delivery carries its stream's id")
		})
	}

	t.Run("two streams receive a tagged copy each", func(t *testing.T) {
		ts := newTargetedStdio(t)
		ts.listen(t, "listen-1")
		ts.listen(t, "listen-2")
		require.NoError(t, ts.server.SendNotificationToSpecificClient(ts.id(t), tools, nil))
		copies := ts.copiesOf(t, tools, "listen-1", "listen-2")
		// A listen still setting its stream up sends the session the list
		// change again once the stream is recorded, as the send may have
		// missed it (see listChangeCounts), and that copy reaches every
		// stream: each may receive one more per listen, never more.
		for id, n := range copies {
			assert.LessOrEqual(t, n, 3, "copies on %v", id)
		}
	})

	t.Run("a modern session without a stream receives nothing", func(t *testing.T) {
		ts := newTargetedStdio(t)
		ts.send(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":` + modernMetaJSON + `}}`)
		awaitMessage(t, ts.out, func(m map[string]any) bool { return m["id"] == float64(1) }, "the modern tools/list")
		id := ts.id(t)
		require.NoError(t, ts.server.SendNotificationToSpecificClient(id, tools, nil))
		require.NoError(t, ts.server.SendLogMessageToSpecificClient(id, errorLog))
		ts.quiet(t)
	})

	t.Run("a handshake-era session receives it untagged", func(t *testing.T) {
		ts := newTargetedStdio(t)
		ts.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
		awaitMessage(t, ts.out, func(m map[string]any) bool { return m["id"] == float64(1) }, "the initialize result")
		ts.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		id := ts.id(t)
		prompts := string(mcp.MethodNotificationPromptsListChanged)
		// The initialized notification is read asynchronously: retry until
		// the session takes targeted sends.
		deadline := time.Now().Add(5 * time.Second)
		err := ts.server.SendNotificationToSpecificClient(id, prompts, nil)
		for err != nil && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
			err = ts.server.SendNotificationToSpecificClient(id, prompts, nil)
		}
		require.NoError(t, err)
		first := ts.next(t)
		assert.Equal(t, prompts, first["method"])
		assert.Nil(t, streamIDOf(first), "a handshake-era notification is not tagged")
	})
}
