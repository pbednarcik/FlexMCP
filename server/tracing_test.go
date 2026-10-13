package server

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/tracing"
)

func TestWithTracer_NilFallsBackToNoop(t *testing.T) {
	s := NewMCPServer("trace-srv", "1.0", WithTracer(nil))
	require.NotNil(t, s.tracer)
}

func TestWithPropagator_NilFallsBackToNoop(t *testing.T) {
	s := NewMCPServer("trace-srv", "1.0", WithPropagator(nil))
	require.NotNil(t, s.propagator)
}

func TestWithMetaPropagator_NilFallsBackToNoop(t *testing.T) {
	s := NewMCPServer("trace-srv", "1.0", WithMetaPropagator(nil))
	require.NotNil(t, s.metaPropagator)
}

func TestWithMetaPropagator_ExtractCalledWithNilMeta(t *testing.T) {
	extracted := false
	mp := &stubMetaPropagator{onExtract: func() { extracted = true }}
	s := NewMCPServer("trace-srv", "1.0", WithMetaPropagator(mp))

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"t","version":"1"},"capabilities":{}}}`)
	resp := s.HandleMessage(t.Context(), body)
	require.NotNil(t, resp)
	assert.True(t, extracted, "extractMeta must be called even when _meta is absent")
}

type stubMetaPropagator struct {
	onExtract func()
}

func (p *stubMetaPropagator) InjectMeta(ctx context.Context, meta *mcp.Meta) *mcp.Meta {
	return meta
}

func (p *stubMetaPropagator) ExtractMeta(ctx context.Context, _ *mcp.Meta) context.Context {
	if p.onExtract != nil {
		p.onExtract()
	}
	return ctx
}

type recordingTracer struct {
	spans []string
}

func (r *recordingTracer) Start(ctx context.Context, name string, _ tracing.SpanKind, _ ...tracing.Attribute) (context.Context, tracing.Span) {
	r.spans = append(r.spans, name)
	return ctx, recordingSpan{}
}

type recordingSpan struct{}

func (recordingSpan) SetAttributes(...tracing.Attribute)   {}
func (recordingSpan) RecordError(error)                    {}
func (recordingSpan) SetStatus(tracing.StatusCode, string) {}
func (recordingSpan) End()                                 {}

func TestWithTracer_DispatcherStartsServerSpan(t *testing.T) {
	tr := &recordingTracer{}
	s := NewMCPServer("trace-srv", "1.0", WithTracer(tr))

	body := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + mcp.LATEST_PROTOCOL_VERSION + `","clientInfo":{"name":"test","version":"1"},"capabilities":{}}}`)
	resp := s.HandleMessage(t.Context(), body)
	require.NotNil(t, resp)
	assert.Contains(t, tr.spans, "mcp.initialize")
}

func TestWithTracer_RegistersToolMiddleware(t *testing.T) {
	tr := &recordingTracer{}
	s := NewMCPServer("trace-srv", "1.0", WithTracer(tr))
	s.AddTool(mcp.Tool{Name: "echo"}, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("ok"), nil
	})

	initBody := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + mcp.LATEST_PROTOCOL_VERSION + `","clientInfo":{"name":"test","version":"1"},"capabilities":{}}}`)
	require.NotNil(t, s.HandleMessage(t.Context(), initBody))

	callBody := []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	require.NotNil(t, s.HandleMessage(t.Context(), callBody))

	assert.Contains(t, tr.spans, "mcp.tools/call")
	assert.Contains(t, tr.spans, "tool.echo")
}

// A server with no tracer starts no span, so dispatching a message costs no
// allocation for tracing; WithTracer(nil) installs the no-op tracer, the same.
func TestMessageSpanWithoutTracerAllocatesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector's instrumentation allocates")
	}
	headers := http.Header{}
	headers.Set(HeaderKeyProtocolVersion, mcp.LATEST_PROTOCOL_VERSION)
	for name, s := range map[string]*MCPServer{
		"no tracer":  NewMCPServer("trace-srv", "1.0"),
		"nil tracer": NewMCPServer("trace-srv", "1.0", WithTracer(nil)),
	} {
		ctx := t.Context()
		allocs := testing.AllocsPerRun(1000, func() {
			_, end := s.startMessageSpan(ctx, headers, "tools/call")
			end(nil)
		})
		assert.Zero(t, allocs, name)
	}
}

type propagatedKey struct{}

type stubPropagator struct{}

func (stubPropagator) Inject(context.Context, http.Header) {}
func (stubPropagator) Extract(ctx context.Context, _ http.Header) context.Context {
	return context.WithValue(ctx, propagatedKey{}, "remote")
}

// The trace context of the request reaches the handlers even when the server
// records no spans of its own.
func TestMessageSpanWithoutTracerStillExtracts(t *testing.T) {
	s := NewMCPServer("trace-srv", "1.0", WithPropagator(stubPropagator{}))
	ctx, end := s.startMessageSpan(t.Context(), http.Header{}, "tools/call")
	end(nil)
	assert.Equal(t, "remote", ctx.Value(propagatedKey{}))
}
