package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// headerCheckSchema is a tool's input schema binding a string, an integer, a
// boolean and a nested string to headers; unannotated, the same schema binds
// none.
func headerCheckSchema(annotated bool) json.RawMessage {
	header := func(name string) string {
		if annotated {
			return `,"x-mcp-header":"` + name + `"`
		}
		return ""
	}
	return json.RawMessage(`{"type":"object","properties":{` +
		`"region":{"type":"string"` + header("Region") + `},` +
		`"count":{"type":"integer"` + header("Count") + `},` +
		`"dry":{"type":"boolean"` + header("Dry") + `},` +
		`"target":{"type":"object","properties":{"zone":{"type":"string"` + header("Zone") + `}}},` +
		`"sql":{"type":"string"}}}`)
}

// headerCheckBody is a modern tools/call of the tool headerCheckSchema
// describes.
const headerCheckBody = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query",` +
	`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientInfo":{"name":"c","version":"1"},"io.modelcontextprotocol/clientCapabilities":{}},` +
	`"arguments":{"region":"us-east-1","count":42,"dry":true,"target":{"zone":"b"},"sql":"select 1"}}}`

// headerCheckServer serves the tool headerCheckSchema describes, and the
// headers of a modern call of it: with the bound values when annotated.
func headerCheckServer(t testing.TB, annotated bool) (*MCPServer, http.Header) {
	t.Helper()
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	srv.AddTool(mcp.NewToolWithRawSchema("query", "runs a query", headerCheckSchema(annotated)),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})
	headers := http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"}}
	headers.Set(mcp.HeaderProtocolVersion, mcp.ProtocolVersion20260728)
	headers.Set(mcp.HeaderMethod, string(mcp.MethodToolsCall))
	headers.Set(mcp.HeaderName, "query")
	if annotated {
		headers.Set(mcp.HeaderParamPrefix+"Region", "us-east-1")
		headers.Set(mcp.HeaderParamPrefix+"Count", "42")
		headers.Set(mcp.HeaderParamPrefix+"Dry", "true")
		headers.Set(mcp.HeaderParamPrefix+"Zone", "b")
	}
	return srv, headers
}

// The Mcp-Param-* check of a modern tools/call reads the message for the
// bound values alone, so it allocates nothing: 36 allocations when it decoded
// params and a map of every argument (measured at fef75dd7).
func TestParamHeadersCheckAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	body := []byte(headerCheckBody)

	t.Run("the check", func(t *testing.T) {
		srv, headers := headerCheckServer(t, true)
		var envelope requestEnvelope
		require.NoError(t, readEnvelope(body, &envelope))
		check := func(headers http.Header) error {
			return srv.validateStandardHeadersOfRequest(headers, mcp.ProtocolVersion20260728, mcp.MethodToolsCall, &envelope.Params, body)
		}
		require.NoError(t, check(headers))
		wrong := headers.Clone()
		wrong.Set(mcp.HeaderParamPrefix+"Count", "41")
		require.Error(t, check(wrong), "the check must run")

		allocs := testing.AllocsPerRun(1000, func() { _ = check(headers) })
		assert.LessOrEqual(t, allocs, 0.0, "allocations in the Mcp-Param-* check of a tools/call")
	})

	t.Run("a bound call costs what the same call unbound does", func(t *testing.T) {
		call := func(annotated bool) float64 {
			srv, headers := headerCheckServer(t, annotated)
			h := NewStreamableHTTPServer(srv)
			request := func() *http.Request {
				r := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
				r.Header = headers
				return r
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, request())
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			return testing.AllocsPerRun(1000, func() { h.ServeHTTP(httptest.NewRecorder(), request()) })
		}
		unbound := call(false)
		bound := call(true)
		assert.LessOrEqual(t, bound-unbound, 0.0, "allocations a tools/call's four header bindings add over HTTP")
	})
}

// headerWalkBindings binds a string, an integer, a boolean, two members of
// one nested object and a value two objects deep.
func headerWalkBindings(t testing.TB) []headerBinding {
	t.Helper()
	bindings := keepHeaderBindings(&mcp.Tool{Name: "query", RawInputSchema: json.RawMessage(`{"type":"object","properties":{` +
		`"region":{"type":"string","x-mcp-header":"Region"},` +
		`"count":{"type":"integer","x-mcp-header":"Count"},` +
		`"dry":{"type":"boolean","x-mcp-header":"Dry"},` +
		`"target":{"type":"object","properties":{` +
		`"zone":{"type":"string","x-mcp-header":"Zone"},"rack":{"type":"string","x-mcp-header":"rack"}}},` +
		`"a":{"type":"object","properties":{"b":{"type":"object","properties":{"c":{"type":"string","x-mcp-header":"C"}}}}}}}`)})
	require.Len(t, bindings, 6)
	return bindings
}

// headerWalkHeaderSets are the Mcp-Param-* headers each message is checked
// with: none, the values the messages mostly carry, and values that differ
// by encoding, form, case or presence.
var headerWalkHeaderSets = []map[string][]string{
	{},
	{"Region": {"us-east-1"}, "Count": {"42"}, "Dry": {"true"}, "Zone": {"b"}, "rack": {"r1"}, "C": {"x"}},
	{"Region": {"=?base64?dXMtZWFzdC0x?="}, "Count": {"42.0"}, "Dry": {"false"}, "Zone": {""}, "C": {"=?base64?***?="}},
	{"Region": {""}, "Count": {"4.2e1"}, "Dry": {"TRUE"}, "Zone": {"=?base64?em9uw6k=?="}, "rack": {"r1", "r2"}},
	{"Region": {"us-east-2"}, "Count": {"NaN"}, "Zone": {"c"}, "C": {"y"}},
	{"Region": {"é"}, "Count": {"9007199254740993"}, "Dry": {"1"}, "Zone": {"zoné"}},
	{"Count": {"-0"}, "Region": {"us\"east"}, "C": {"=?base64?eA==?="}},
	// us-east-1 and U+FFFD, which json.Unmarshal reads invalid UTF-8 as.
	{"Region": {"=?base64?dXMtZWFzdC0x77+9?="}, "Count": {"42"}},
}

func headerWalkHeaders(set map[string][]string) http.Header {
	headers := http.Header{}
	for name, values := range set {
		headers[http.CanonicalHeaderKey(mcp.HeaderParamPrefix+name)] = values
	}
	return headers
}

// escapedJSON writes each ~ in s as a backslash, so a corpus entry spells a
// JSON escape without spelling it in the source.
func escapedJSON(s string) string {
	return strings.ReplaceAll(s, "~", string(rune(0x5c)))
}

// headerWalkMessage is a tools/call whose arguments are arguments.
func headerWalkMessage(arguments string) string {
	return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","arguments":` + arguments + `}}`
}

// headerWalkCorpus are messages the walk settles (settles) or leaves to
// encoding/json: values of every kind on bound and unbound paths, escapes,
// non-ASCII, duplicate names at every level, arrays on the path, arguments
// before and after _meta, and messages json.Unmarshal reads differently or
// not at all.
var headerWalkCorpus = []struct {
	name, message string
	settles       bool
}{
	{"all bound", headerWalkMessage(`{"region":"us-east-1","count":42,"dry":true,"target":{"zone":"b","rack":"r1"},"a":{"b":{"c":"x"}}}`), true},
	{"empty arguments", headerWalkMessage(`{}`), true},
	{"null arguments", headerWalkMessage(`null`), true},
	{"arguments an array", headerWalkMessage(`[{"region":"us-east-1"}]`), true},
	{"arguments a string", headerWalkMessage(`"region"`), true},
	{"arguments a number", headerWalkMessage(`42`), true},
	{"null values", headerWalkMessage(`{"region":null,"count":null,"dry":null,"target":null,"a":{"b":null}}`), true},
	{"string forms", headerWalkMessage(escapedJSON(`{"region":"","count":"42","dry":"true","target":{"zone":"zon~u00e9","rack":"r~u0031"},"a":{"b":{"c":"~u0078"}}}`)), true},
	{"escapes", headerWalkMessage(escapedJSON(`{"region":"us~u002deast-1","target":{"zone":"~"b~~~/"},"a":{"b":{"c":"~ud83d~ude00"}}}`)), true},
	{"escaped names", headerWalkMessage(escapedJSON(`{"re~u0067ion":"us-east-1","t~u0061rget":{"zon~u0065":"b"}}`)), true},
	{"quote in a string", headerWalkMessage(`{"region":"us\"east"}`), true},
	{"non-ASCII names", headerWalkMessage(`{"régión":"x","region":"é","ä":{"b":1}}`), true},
	{"numbers", headerWalkMessage(`{"count":42.0,"region":1e2,"dry":-0}`), true},
	{"number forms", headerWalkMessage(`{"count":4.2e1,"region":0.5,"dry":-9007199254740991}`), true},
	{"numbers past the safe range", headerWalkMessage(`{"count":9007199254740993,"region":-9007199254740992,"dry":1e400}`), true},
	{"a fraction", headerWalkMessage(`{"count":42.5}`), true},
	{"a large exponent", headerWalkMessage(`{"count":1e308,"region":1e-400}`), true},
	{"booleans", headerWalkMessage(`{"dry":false,"region":true,"count":true}`), true},
	{"objects and arrays", headerWalkMessage(`{"region":{"a":1},"count":[42],"dry":[],"target":{"zone":{},"rack":[ "r1" ]}}`), true},
	{"whitespace", headerWalkMessage(` { "region" : "us-east-1" , "count" : 42 , "target" : { "zone" : "b" } } `), true},
	{"duplicate value", headerWalkMessage(`{"region":"us-west-2","region":"us-east-1","count":1,"count":42}`), true},
	{"duplicate then null", headerWalkMessage(`{"region":"us-east-1","region":null}`), true},
	{"duplicate nested", headerWalkMessage(`{"target":{"zone":"a","zone":"b","rack":"r1"}}`), true},
	{"duplicate parent", headerWalkMessage(`{"target":{"zone":"b","rack":"r1"},"target":{"zone":"c"}}`), true},
	{"duplicate parent, the last not an object", headerWalkMessage(`{"target":{"zone":"b"},"target":5}`), true},
	{"duplicate deep parent", headerWalkMessage(`{"a":{"b":{"c":"x"}},"a":{"b":{"d":"x"},"b":{"c":"y"}}}`), true},
	{"parent null", headerWalkMessage(`{"target":null,"a":{"b":null}}`), true},
	{"parent a scalar", headerWalkMessage(`{"target":"zone","a":{"b":"c"}}`), true},
	{"arrays on the path", headerWalkMessage(`{"target":[{"zone":"b"}],"a":[{"b":{"c":"x"}}],"region":["us-east-1"]}`), true},
	{"unbound members", headerWalkMessage(`{"sql":"select 1","items":[{"region":"x"}],"nested":{"region":"y","target":{"zone":"z"}}}`), true},
	{"bound names in _meta", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"_meta":{"arguments":{"region":"x"},"progressToken":1},"name":"query","arguments":{"region":"us-east-1"}}}`, true},
	{"arguments before _meta", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"arguments":{"region":"us-east-1"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"},"name":"query"}}`, true},
	{"arguments after _meta", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"query","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"},"arguments":{"count":42}}}`, true},
	{"params first", `{"params":{"arguments":{"region":"us-east-1"},"name":"query"},"method":"tools/call","id":1,"jsonrpc":"2.0"}`, true},
	{"arguments outside params", `{"jsonrpc":"2.0","id":1,"method":"tools/call","arguments":{"region":"x"},"params":{"name":"query"}}`, true},
	{"no params", `{"jsonrpc":"2.0","id":1,"method":"tools/call"}`, true},
	{"params null", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":null}`, true},
	{"params an array", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":[{"arguments":{"region":"x"}}]}`, true},
	{"params a string", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"x"}`, true},
	{"near names", `{"param":{"arguments":{"region":"x"}},"params":{"argument":{"region":"y"},"argumentss":{"region":"z"}}}`, true},
	{"near names with dashes", `{"pa-ram":{},"params":{"arg_ument":{"region":"y"}}}`, true},
	{"a long message", headerWalkMessage(`{"region":"us-east-1","sql":"` + strings.Repeat("x", 70000) + `"}`), true},

	{"duplicate params", `{"params":{"arguments":{"region":"x"}},"params":{"arguments":{"region":"us-east-1"}}}`, false},
	{"duplicate params, the last not an object", `{"params":{"arguments":{"region":"x"}},"params":null}`, false},
	{"duplicate arguments", headerWalkMessage(`{"region":"x"},"arguments":{"count":42}`), false},
	{"duplicate arguments, the last null", headerWalkMessage(`{"region":"x"},"arguments":null`), false},
	{"params in another case", `{"Params":{"arguments":{"region":"x"}}}`, false},
	{"params in another case beside it", `{"params":{"arguments":{"region":"x"}},"PARAMS":{"arguments":{"region":"y"}}}`, false},
	{"params with an underscore", `{"para_ms":{"arguments":{"region":"x"}}}`, false},
	{"params with a long s", `{"paramſ":{"arguments":{"region":"x"}}}`, false},
	{"arguments in another case", `{"params":{"Arguments":{"region":"x"}}}`, false},
	{"arguments with a Kelvin sign", escapedJSON(`{"params":{"arguments":{"region":"y"},"~u212Aarguments":{"region":"x"}}}`), false},
	{"invalid UTF-8 in a value", headerWalkMessage("{\"region\":\"us-east-1\xff\"}"), false},
	{"invalid UTF-8 elsewhere", "{\"id\":\"\xff\",\"params\":{\"arguments\":{\"region\":\"us-east-1\"}}}", false},
	{"a lone surrogate", headerWalkMessage(escapedJSON(`{"region":"~ud800"}`)), false},
	{"trailing data", headerWalkMessage(`{"region":"us-east-1"}`) + ` x`, false},
	{"two messages", headerWalkMessage(`{"region":"us-east-1"}`) + headerWalkMessage(`{}`), false},
	{"a syntax error late", `{"params":{"arguments":{"region":"us-east-1"}},"id":}`, false},
	{"a syntax error in arguments", headerWalkMessage(`{"region":"us-east-1",}`), false},
	{"truncated", headerWalkMessage(`{"region":"us-east-1"`), false},
	{"an array", `[` + headerWalkMessage(`{"region":"us-east-1"}`) + `]`, false},
	{"null", `null`, false},
	{"empty", ``, false},
}

// headerWalkOracle is the check as it was before the walk: the params
// json.Unmarshal reads from the message, checked by mcp's own function.
func headerWalkOracle(headers http.Header, bindings []headerBinding, message []byte) error {
	var wrapper struct {
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(message, &wrapper); err != nil {
		return nil
	}
	plain := make([]mcp.ParamHeaderBinding, len(bindings))
	for i := range bindings {
		plain[i] = bindings[i].ParamHeaderBinding
	}
	return mcp.ValidateParamHeadersWithBindings(func(name string) (string, bool) {
		values := headers.Values(name)
		if len(values) == 0 {
			return "", false
		}
		return values[0], true
	}, plain, wrapper.Params)
}

// walkSettles reports whether the walk decides message itself.
func walkSettles(headers http.Header, bindings []headerBinding, message []byte) bool {
	w := paramHeaderWalkers.Get().(*paramHeaderWalker)
	defer paramHeaderWalkers.Put(w)
	defer w.finish()
	settled, _ := w.check(headers, bindings, message)
	return settled
}

// The walk accepts and refuses exactly the requests the check it replaced
// did, with the same error naming the same header, and settles every message
// encoding/json reads without a case-insensitive match, a merge or a
// replacement character.
func TestCheckParamHeadersMatchesTheDecodingCheck(t *testing.T) {
	bindings := headerWalkBindings(t)
	pairs, refused := 0, 0
	for _, tc := range headerWalkCorpus {
		t.Run(tc.name, func(t *testing.T) {
			pairs += len(headerWalkHeaderSets)
			message := []byte(tc.message)
			for i, set := range headerWalkHeaderSets {
				headers := headerWalkHeaders(set)
				want := headerWalkOracle(headers, bindings, message)
				assert.Equal(t, want, checkParamHeaders(headers, bindings, message), "header set %d", i)
				assert.Equal(t, tc.settles, walkSettles(headers, bindings, message), "header set %d", i)
				if want != nil {
					refused++
				}
			}
		})
	}
	// Both outcomes must be common, or the comparison proves little.
	if pairs < len(headerWalkCorpus)*len(headerWalkHeaderSets) {
		return
	}
	assert.Greater(t, refused, pairs/4, "refusals among %d pairs", pairs)
	assert.Less(t, refused, pairs*3/4, "refusals among %d pairs", pairs)
}

// FuzzCheckParamHeaders checks the walk against the check it replaced on any
// message and header values.
func FuzzCheckParamHeaders(f *testing.F) {
	for _, tc := range headerWalkCorpus {
		f.Add([]byte(tc.message), "us-east-1", "42", "true", "b", "x")
	}
	bindings := headerWalkBindings(f)
	f.Fuzz(func(t *testing.T, message []byte, region, count, dry, zone, c string) {
		headers := headerWalkHeaders(map[string][]string{
			"Region": {region}, "Count": {count}, "Dry": {dry}, "Zone": {zone}, "C": {c},
		})
		if region == "" {
			delete(headers, http.CanonicalHeaderKey(mcp.HeaderParamPrefix+"Region"))
		}
		want := headerWalkOracle(headers, bindings, message)
		if got := checkParamHeaders(headers, bindings, message); !assert.Equal(t, want, got) {
			t.Logf("message %q", message)
		}
	})
}

// A tool with more bindings than the walk tracks, one bit each, is checked by
// the check the walk replaced: a correct request is accepted and a wrong one
// refused as that check refuses it, whichever binding is wrong.
func TestCheckParamHeadersPastTheWalksBindings(t *testing.T) {
	const count = maxWalkBindings + 1
	properties := make([]string, count)
	arguments := make([]string, count)
	for i := range count {
		properties[i] = fmt.Sprintf(`"p%d":{"type":"string","x-mcp-header":"H%d"}`, i, i)
		arguments[i] = fmt.Sprintf(`"p%d":"v%d"`, i, i)
	}
	bindings := keepHeaderBindings(&mcp.Tool{Name: "many", RawInputSchema: json.RawMessage(
		`{"type":"object","properties":{` + strings.Join(properties, ",") + `}}`)})
	require.Len(t, bindings, count)
	message := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"many","arguments":{` +
		strings.Join(arguments, ",") + `}}}`)
	headers := func(wrong int) http.Header {
		h := http.Header{}
		for i := range count {
			value := fmt.Sprintf("v%d", i)
			if i == wrong {
				value = "wrong"
			}
			h.Set(fmt.Sprintf("%sH%d", mcp.HeaderParamPrefix, i), value)
		}
		return h
	}
	assert.False(t, walkSettles(headers(-1), bindings, message), "the walk settles %d bindings", count)
	assert.NoError(t, checkParamHeaders(headers(-1), bindings, message), "a correct request")
	for _, wrong := range []int{0, 9, maxWalkBindings - 1, maxWalkBindings} {
		want := headerWalkOracle(headers(wrong), bindings, message)
		require.Error(t, want, "the old check accepts p%d wrong", wrong)
		assert.Equal(t, want, checkParamHeaders(headers(wrong), bindings, message), "p%d wrong", wrong)
	}
}
