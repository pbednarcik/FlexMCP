package client

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// modernClient is a client whose era in effect is protocol version
// 2026-07-28, published as Initialize publishes one.
func modernClient(t *testing.T, options ...ClientOption) *Client {
	t.Helper()
	c := NewClient(nil, options...)
	c.updateEra(func(e *era) { e.version = mcp.LATEST_PROTOCOL_VERSION })
	return c
}

// sentParams is the JSON object params go on the wire as, after the
// client's _meta was applied in era e.
func sentParams(t *testing.T, c *Client, e *era, params any) map[string]json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(c.applyRequestMeta(e, params))
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &fields))
	return fields
}

func sentMeta(t *testing.T, fields map[string]json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var meta map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(fields["_meta"], &meta))
	return meta
}

// A request carries the _meta of the era it is sent in, even an era that was
// never published: Initialize's probe is a copy of the era in effect with
// its own version and identity, and must not declare the published one's.
func TestRequestMetaIsTheEraItIsSentIn(t *testing.T) {
	c := modernClient(t)
	c.updateEra(func(e *era) { e.clientInfo = mcp.Implementation{Name: "published", Version: "1"} })
	_ = sentParams(t, c, c.currentEra(), nil)

	probe := *c.currentEra()
	probe.clientInfo = mcp.Implementation{Name: "probe", Version: "2"}
	meta := sentMeta(t, sentParams(t, c, &probe, map[string]any{"name": "tool"}))
	assert.JSONEq(t, `{"name":"probe","version":"2"}`, string(meta[mcp.MetaKeyClientInfo]))
}

// An era published again, with other capabilities, is declared as it now
// is: nothing of the one before it is kept.
func TestRequestMetaFollowsTheEraPublishedLast(t *testing.T) {
	c := modernClient(t)
	_ = sentParams(t, c, c.currentEra(), nil)
	c.updateEra(func(e *era) {
		e.clientCapabilities = mcp.ClientCapabilities{Experimental: map[string]any{"x": map[string]any{}}}
	})
	meta := sentMeta(t, sentParams(t, c, c.currentEra(), nil))
	assert.JSONEq(t, `{"experimental":{"x":{}}}`, string(meta[mcp.MetaKeyClientCapabilities]))
}

// Every request on protocol version 2026-07-28 carries the version, the
// capabilities and the identity in _meta beside its own members, whether
// it has _meta of its own (kept, the client's entries added), none, no
// params at all, a value that merely mentions "_meta", or a log level to
// declare.
func TestRequestMetaKeepsTheRequestsOwnMembers(t *testing.T) {
	want := func(t *testing.T, c *Client, meta map[string]json.RawMessage) {
		t.Helper()
		assert.JSONEq(t, `"`+mcp.LATEST_PROTOCOL_VERSION+`"`, string(meta[mcp.MetaKeyProtocolVersion]))
		assert.JSONEq(t, `{}`, string(meta[mcp.MetaKeyClientCapabilities]))
		info, err := json.Marshal(c.currentEra().clientInfo)
		require.NoError(t, err)
		assert.JSONEq(t, string(info), string(meta[mcp.MetaKeyClientInfo]))
	}
	c := modernClient(t)

	t.Run("no _meta", func(t *testing.T) {
		fields := sentParams(t, c, c.currentEra(), map[string]any{"name": "tool", "arguments": map[string]any{"a": 1}})
		assert.JSONEq(t, `"tool"`, string(fields["name"]))
		assert.JSONEq(t, `{"a":1}`, string(fields["arguments"]))
		assert.Len(t, fields, 3)
		meta := sentMeta(t, fields)
		want(t, c, meta)
		assert.Len(t, meta, 3)
	})
	t.Run("no params", func(t *testing.T) {
		fields := sentParams(t, c, c.currentEra(), nil)
		assert.Len(t, fields, 1)
		want(t, c, sentMeta(t, fields))
	})
	t.Run("empty params", func(t *testing.T) {
		fields := sentParams(t, c, c.currentEra(), map[string]any{})
		assert.Len(t, fields, 1)
		want(t, c, sentMeta(t, fields))
	})
	t.Run("own _meta", func(t *testing.T) {
		fields := sentParams(t, c, c.currentEra(), map[string]any{"name": "tool", "_meta": map[string]any{"progressToken": int64(12345678901234567)}})
		assert.JSONEq(t, `"tool"`, string(fields["name"]))
		meta := sentMeta(t, fields)
		want(t, c, meta)
		assert.Equal(t, "12345678901234567", string(meta["progressToken"]))
	})
	t.Run("a value mentioning _meta", func(t *testing.T) {
		fields := sentParams(t, c, c.currentEra(), map[string]any{"name": `"_meta"`})
		assert.JSONEq(t, `"\"_meta\""`, string(fields["name"]))
		want(t, c, sentMeta(t, fields))
	})
	t.Run("log level", func(t *testing.T) {
		leveled := modernClient(t)
		leveled.logLevel = mcp.LoggingLevelDebug
		meta := sentMeta(t, sentParams(t, leveled, leveled.currentEra(), map[string]any{"name": "tool"}))
		want(t, leveled, meta)
		assert.JSONEq(t, `"debug"`, string(meta[mcp.MetaKeyLogLevel]))
	})
}
