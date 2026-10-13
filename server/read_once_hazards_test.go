package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An OnRequestInitialization hook holds the message itself and may change it.
// The checks after it read the message it left, as they did when each read
// the body on its own: here the hook renames the tool after the Mcp-Name
// header was sent, and the header no longer matches.
func TestReadOnceChecksReadWhatAnInitializationHookLeft(t *testing.T) {
	hooks := &Hooks{}
	hooks.AddOnRequestInitialization(func(_ context.Context, _ any, message any) error {
		if raw, ok := message.(json.RawMessage); ok {
			if i := bytes.Index(raw, []byte(`"name":"echo"`)); i >= 0 {
				copy(raw[i:], `"name":"evil"`)
			}
		}
		return nil
	})
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithHooks(hooks))
	for _, name := range []string{"echo", "evil"} {
		srv.AddTool(mcp.NewTool(name), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ran " + name), nil
		})
	}
	header, _ := mcp.EncodeHeaderValue("echo")
	ctx := context.WithValue(t.Context(), requestHeader, http.Header{
		"Mcp-Protocol-Version": {"2026-07-28"}, "Mcp-Method": {"tools/call"}, "Mcp-Name": {header},
	})
	message := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo",` + readOnceMeta + `}}`)
	out, err := json.Marshal(srv.HandleMessage(ctx, message))
	require.NoError(t, err)
	assert.Contains(t, string(out), `"code":-32020`, "the header names echo, the body now evil: %s", out)
	assert.NotContains(t, string(out), "ran evil")
}

// A body handed to Handle belongs to its caller, who may change it while the
// request is handled; the core reads the body it is given, as it did, rather
// than the transport's earlier read of it.
func TestReadOnceHandleReadsTheBodyAsItIsWhenTheCoreRuns(t *testing.T) {
	srv := NewMCPServer("test", "1.0.0")
	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	h := NewStreamableHTTPServer(srv, WithHTTPContextFunc(func(ctx context.Context, _ *http.Request) context.Context {
		copy(body[bytes.Index(body, []byte("ping")):], "noop")
		return ctx
	}))
	w := httptest.NewRecorder()
	h.Handle(newHTTPResponseWriterAdapter(w), &HTTPRequest{
		Method:  http.MethodPost,
		Header:  http.Header{"Content-Type": {"application/json"}, "Accept": {"application/json, text/event-stream"}, "Mcp-Protocol-Version": {"2025-11-25"}},
		Body:    body,
		Context: t.Context(),
	})
	assert.Contains(t, w.Body.String(), `"code":-32601`, "the body names noop by the time the core reads it: %s", w.Body.String())
}

// A hook that removes itself while it changes the message still held it: the
// checks read the message it left, though no hook is registered by then.
func TestReadOnceChecksReadWhatASelfRemovingHookLeft(t *testing.T) {
	hooks := &Hooks{}
	hooks.AddOnRequestInitialization(func(_ context.Context, _ any, message any) error {
		hooks.OnRequestInitialization = nil
		if raw, ok := message.(json.RawMessage); ok {
			if i := bytes.Index(raw, []byte(`"name":"echo"`)); i >= 0 {
				copy(raw[i:], `"name":"evil"`)
			}
		}
		return nil
	})
	srv := NewMCPServer("test", "1.0.0", WithToolCapabilities(false), WithHooks(hooks))
	for _, name := range []string{"echo", "evil"} {
		srv.AddTool(mcp.NewTool(name), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ran " + name), nil
		})
	}
	header, _ := mcp.EncodeHeaderValue("echo")
	ctx := context.WithValue(t.Context(), requestHeader, http.Header{
		"Mcp-Protocol-Version": {"2026-07-28"}, "Mcp-Method": {"tools/call"}, "Mcp-Name": {header},
	})
	message := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo",` + readOnceMeta + `}}`)
	out, err := json.Marshal(srv.HandleMessage(ctx, message))
	require.NoError(t, err)
	assert.Contains(t, string(out), `"code":-32020`, "the header names echo, the body now evil: %s", out)
	assert.NotContains(t, string(out), "ran evil")
}
