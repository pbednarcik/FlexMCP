package transport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// fakeOptionalTransport implements the optional interfaces the client asks
// a transport for beyond the two the decorator mirrored.
type fakeOptionalTransport struct {
	fakeTransport
	lost   func(error)
	stderr io.Reader
}

func (f *fakeOptionalTransport) RequiresLegacyProtocol() bool           { return true }
func (f *fakeOptionalTransport) SetConnectionLostHandler(h func(error)) { f.lost = h }
func (f *fakeOptionalTransport) Stderr() io.Reader                      { return f.stderr }

// The decorator forwards the optional methods the client looks for: the
// legacy pin, the connection-lost hook and the stderr reader.
func TestLoggingForwardsTheOptionalInterfaces(t *testing.T) {
	inner := &fakeOptionalTransport{stderr: strings.NewReader("log")}
	wrapped := NewLogging(inner, slog.New(newCaptureHandler()))

	legacy, ok := wrapped.(interface{ RequiresLegacyProtocol() bool })
	require.True(t, ok, "the legacy pin is reachable through the decorator")
	assert.True(t, legacy.RequiresLegacyProtocol())

	setter, ok := wrapped.(interface{ SetConnectionLostHandler(func(error)) })
	require.True(t, ok, "the connection-lost hook is reachable through the decorator")
	setter.SetConnectionLostHandler(func(error) {})
	assert.NotNil(t, inner.lost, "the hook reached the inner transport")

	stderrer, ok := wrapped.(interface{ Stderr() io.Reader })
	require.True(t, ok, "the stderr reader is reachable through the decorator")
	assert.Same(t, inner.stderr, stderrer.Stderr())
}

// A transport that lacks the optional methods is not given them: the
// decorator answers the way the client would read a missing method.
func TestLoggingAnswersForAnInnerWithoutTheOptionalMethods(t *testing.T) {
	wrapped := NewLogging(&fakeTransport{}, slog.New(newCaptureHandler()))

	legacy, ok := wrapped.(interface{ RequiresLegacyProtocol() bool })
	if ok {
		assert.False(t, legacy.RequiresLegacyProtocol())
	}
	stderrer, ok := wrapped.(interface{ Stderr() io.Reader })
	if ok {
		assert.Nil(t, stderrer.Stderr())
	}
}

// With payloads off, the transport's error text is not logged: it carries
// the HTTP response body, which may hold anything the server sent.
func TestLoggingPayloadsOffHidesTheTransportErrorText(t *testing.T) {
	body := `request failed with status 400: {"detail":"the password is hunter2"}`
	inner := &fakeTransport{requestErr: errors.New(body), notificationErr: errors.New(body)}
	handler := newCaptureHandler()
	wrapped := NewLogging(inner, slog.New(handler), WithLoggingPayloads(false))

	_, err := wrapped.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Method: "tools/call"})
	require.Error(t, err)
	require.Error(t, wrapped.SendNotification(t.Context(), mcp.JSONRPCNotification{JSONRPC: mcp.JSONRPC_VERSION, Method: "notifications/initialized"}))

	for _, record := range handler.snapshot() {
		for key, value := range attrsOf(record) {
			assert.NotContains(t, value.String(), "hunter2", "record %q attribute %q", record.Message, key)
		}
	}
}

// Payloads are off unless asked for: tool arguments, results and the user's
// answers do not reach a log by default.
func TestLoggingPayloadsAreOffByDefault(t *testing.T) {
	inner := &fakeTransport{requestResp: &JSONRPCResponse{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Result: json.RawMessage(`{"secret":1}`)}}
	handler := newCaptureHandler()
	wrapped := NewLogging(inner, slog.New(handler))

	_, err := wrapped.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Method: "tools/call", Params: map[string]any{"token": "x"}})
	require.NoError(t, err)

	records := handler.snapshot()
	require.Len(t, records, 2)
	assert.NotContains(t, attrsOf(records[0]), "params")
	assert.NotContains(t, attrsOf(records[1]), "result")
}

// silentTransport answers a fixed response with no bookkeeping, so the
// decorator's own allocations can be counted.
type silentTransport struct {
	fakeTransport
	response *JSONRPCResponse
}

func (s *silentTransport) SendRequest(context.Context, JSONRPCRequest) (*JSONRPCResponse, error) {
	return s.response, nil
}

// A record the logger will not write costs nothing to prepare: no copy of
// the result, no encoding of the params.
func TestLoggingDoesNoWorkWhenTheLoggerIsDisabled(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	inner := &silentTransport{response: &JSONRPCResponse{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Result: json.RawMessage(strings.Repeat("x", 64<<10))}}
	handler := newCaptureHandler()
	handler.level = slog.LevelInfo
	wrapped := NewLogging(inner, slog.New(handler), WithLoggingPayloads(true))
	ctx := t.Context()
	request := JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Method: "tools/call", Params: map[string]any{"a": 1}}
	notification := mcp.JSONRPCNotification{JSONRPC: mcp.JSONRPC_VERSION, Method: "notifications/initialized"}

	assert.Zero(t, testing.AllocsPerRun(20, func() { _, _ = wrapped.SendRequest(ctx, request) }), "allocations per request")
	assert.Zero(t, testing.AllocsPerRun(20, func() { _ = wrapped.SendNotification(ctx, notification) }), "allocations per notification")
	assert.Zero(t, testing.AllocsPerRun(20, func() { inner.notifyHandler(notification) }), "allocations per incoming notification")

	bidir := &fakeBidirTransport{}
	response := &JSONRPCResponse{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(2)), Result: json.RawMessage(`{}`)}
	NewLogging(bidir, slog.New(handler), WithLoggingPayloads(true)).(BidirectionalInterface).SetRequestHandler(func(context.Context, JSONRPCRequest) (*JSONRPCResponse, error) {
		return response, nil
	})
	assert.Zero(t, testing.AllocsPerRun(20, func() { _, _ = bidir.requestHandler(ctx, request) }), "allocations per server request")
	assert.Empty(t, handler.snapshot())
}

// With payloads off, the text of a server request's failure (the handler's
// error, or the error it answered with) is not logged on the bidirectional
// path either.
func TestLoggingPayloadsOffHidesTheHandlerErrorText(t *testing.T) {
	body := `the password is hunter2`
	inner := &fakeBidirTransport{}
	handler := newCaptureHandler()
	wrapped := NewLogging(inner, slog.New(handler), WithLoggingPayloads(false)).(BidirectionalInterface)
	request := JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Method: "sampling/createMessage"}

	wrapped.SetRequestHandler(func(context.Context, JSONRPCRequest) (*JSONRPCResponse, error) {
		return nil, errors.New(body)
	})
	_, err := inner.requestHandler(t.Context(), request)
	require.Error(t, err)

	wrapped.SetRequestHandler(func(context.Context, JSONRPCRequest) (*JSONRPCResponse, error) {
		return NewJSONRPCErrorResponse(request.ID, mcp.INTERNAL_ERROR, body, nil), nil
	})
	_, err = inner.requestHandler(t.Context(), request)
	require.NoError(t, err)

	records := handler.snapshot()
	require.Len(t, records, 4)
	for _, record := range records {
		for key, value := range attrsOf(record) {
			assert.NotContains(t, value.String(), "hunter2", "record %q attribute %q", record.Message, key)
		}
	}
}
