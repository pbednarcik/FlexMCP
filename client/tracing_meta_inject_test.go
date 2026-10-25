package client

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
	"github.com/pbednarcik/flexmcp/tracing"
)

// traceWriter is a MetaPropagator that writes a traceparent field into the
// meta it is given, as the otel adapter does.
type traceWriter struct{ value string }

func (p traceWriter) InjectMeta(_ context.Context, meta *mcp.Meta) *mcp.Meta {
	if meta == nil {
		meta = &mcp.Meta{}
	}
	if meta.AdditionalFields == nil {
		meta.AdditionalFields = map[string]any{}
	}
	meta.AdditionalFields["traceparent"] = p.value
	return meta
}

func (traceWriter) ExtractMeta(ctx context.Context, _ *mcp.Meta) context.Context { return ctx }

// tracedToolServer is a server with one tool that hands the Meta of every
// tools/call it receives to the returned channel.
func tracedToolServer(t *testing.T) (*server.MCPServer, <-chan *mcp.Meta) {
	t.Helper()
	hooks := &server.Hooks{}
	received := make(chan *mcp.Meta, 16)
	hooks.AddBeforeCallTool(func(_ context.Context, _ any, request *mcp.CallToolRequest) {
		received <- request.Params.Meta
	})
	s := server.NewMCPServer("test-server", "1.0.0", server.WithToolCapabilities(true), server.WithHooks(hooks))
	s.AddTool(mcp.NewTool("noop"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})
	return s, received
}

// The trace context goes to the server in a copy of the request's Meta: the
// caller's own Meta carries nothing it did not put there.
func TestMetaPropagatorLeavesTheCallersMetaAlone(t *testing.T) {
	s, received := tracedToolServer(t)
	c := connectInProcess(t, s, "", WithMetaPropagator(traceWriter{value: "00-abc"}))

	request := mcp.CallToolRequest{}
	request.Params.Name = "noop"
	request.Params.Meta = &mcp.Meta{ProgressToken: "p", AdditionalFields: map[string]any{"mine": "kept"}}
	_, err := c.CallTool(t.Context(), request)
	require.NoError(t, err)

	sent := receive(t, received)
	require.NotNil(t, sent)
	assert.Equal(t, "00-abc", sent.AdditionalFields["traceparent"], "the server received the trace context")
	assert.Equal(t, "kept", sent.AdditionalFields["mine"], "the caller's own field went along")
	assert.Equal(t, "p", sent.ProgressToken, "the caller's own meta went along")
	assert.Equal(t, map[string]any{"mine": "kept"}, request.Params.Meta.AdditionalFields, "the caller's Meta is untouched")
}

// A request without fields of its own is left without any.
func TestMetaPropagatorAddsNoFieldsToTheCallersMeta(t *testing.T) {
	s, received := tracedToolServer(t)
	c := connectInProcess(t, s, "", WithMetaPropagator(traceWriter{value: "00-abc"}))

	request := mcp.CallToolRequest{}
	request.Params.Name = "noop"
	request.Params.Meta = &mcp.Meta{ProgressToken: "p"}
	_, err := c.CallTool(t.Context(), request)
	require.NoError(t, err)

	require.NotNil(t, receive(t, received))
	assert.Nil(t, request.Params.Meta.AdditionalFields, "the caller's Meta is untouched")
}

// Callers sharing one request value across goroutines do not race on its
// Meta.
func TestMetaPropagatorOnASharedRequestDoesNotRace(t *testing.T) {
	s, _ := tracedToolServer(t)
	c := connectInProcess(t, s, "", WithMetaPropagator(traceWriter{value: "00-abc"}))

	request := mcp.CallToolRequest{}
	request.Params.Name = "noop"
	request.Params.Meta = &mcp.Meta{ProgressToken: "p"}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, err := c.CallTool(t.Context(), request)
			assert.NoError(t, err)
		})
	}
	wg.Wait()
}

// The no-op propagator, installed explicitly, injects nothing: a request
// without a Meta goes out without one.
func TestTheNoopMetaPropagatorSendsNoMeta(t *testing.T) {
	s, received := tracedToolServer(t)
	c := connectInProcess(t, s, mcp.LATEST_LEGACY_PROTOCOL_VERSION, WithMetaPropagator(tracing.NoopMetaPropagator()))

	request := mcp.CallToolRequest{}
	request.Params.Name = "noop"
	_, err := c.CallTool(t.Context(), request)
	require.NoError(t, err)

	assert.Nil(t, receive(t, received), "no _meta was sent")
}

// passthrough is a MetaPropagator with nothing to inject: it returns the
// meta it is given.
type passthrough struct{}

func (passthrough) InjectMeta(_ context.Context, meta *mcp.Meta) *mcp.Meta       { return meta }
func (passthrough) ExtractMeta(ctx context.Context, _ *mcp.Meta) context.Context { return ctx }

// A propagator that returns its input unchanged sends no _meta for a request
// that had none: there is nothing of the caller's to copy.
func TestAPropagatorWithNothingToInjectSendsNoMeta(t *testing.T) {
	s, received := tracedToolServer(t)
	c := connectInProcess(t, s, mcp.LATEST_LEGACY_PROTOCOL_VERSION, WithMetaPropagator(passthrough{}))

	request := mcp.CallToolRequest{}
	request.Params.Name = "noop"
	_, err := c.CallTool(t.Context(), request)
	require.NoError(t, err)

	assert.Nil(t, receive(t, received), "no _meta was sent")
}
