package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"weak"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A context func that returns a fresh context does not drop the checks of the
// request's headers: the header is added on top of what the func returns.
func TestStreamableHTTPContextFuncKeepsTheHeaderChecks(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	srv.AddTool(mcp.NewTool("echo"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ran echo"), nil
	})
	h := NewStreamableHTTPServer(srv, WithHTTPContextFunc(func(context.Context, *http.Request) context.Context {
		return t.Context()
	}))
	wrong, _ := mcp.EncodeHeaderValue("wrong")
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo",` + readOnceMeta + `}}`
	r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	r.Header = http.Header{
		"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"},
		"Mcp-Protocol-Version": {"2026-07-28"}, "Mcp-Method": {"tools/call"}, "Mcp-Name": {wrong},
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), `"code":-32020`)
	assert.NotContains(t, w.Body.String(), "ran echo")
}

// The supported versions a request sees are its own: changing them changes
// nothing for the next request.
func TestStreamableHTTPSupportedVersionsAreTheRequestsOwn(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0")
	var seen []string
	h := NewStreamableHTTPServer(srv, WithHTTPContextFunc(func(ctx context.Context, _ *http.Request) context.Context {
		versions := SupportedProtocolVersionsFromContext(ctx)
		seen = append(seen, versions[0])
		versions[0] = "corrupted"
		return ctx
	}))
	for range 2 {
		r := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		r.Header = http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"}, "Mcp-Protocol-Version": {"2025-11-25"}}
		h.ServeHTTP(httptest.NewRecorder(), r)
	}
	require.Len(t, seen, 2)
	assert.Equal(t, seen[0], seen[1], "the second request saw the first one's change")
	assert.NotEqual(t, "corrupted", mcp.ValidProtocolVersions[0])
}

// A handler that keeps its context after the request does not keep the
// response writer too.
func TestStreamableHTTPKeptContextReleasesTheWriter(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false))
	var kept context.Context
	srv.AddTool(mcp.NewTool("echo"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		kept = ctx
		return mcp.NewToolResultText("ok"), nil
	})
	h := NewStreamableHTTPServer(srv)
	w := newHTTPResponseWriterAdapter(httptest.NewRecorder())
	writer := weak.Make(w)
	h.Handle(w, &HTTPRequest{
		Method:  http.MethodPost,
		Header:  http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"}, "Mcp-Protocol-Version": {"2025-11-25"}},
		Body:    []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{}}}`),
		Context: t.Context(),
	})
	runtime.GC()
	runtime.GC()
	require.NotNil(t, kept)
	assert.Nil(t, writer.Value(), "the kept context holds the response writer")
	runtime.KeepAlive(kept)
}
