package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/tracing"
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

// tracedToolCall is a modern tools/call whose _meta carries a traceparent.
const tracedToolCall = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"a":"b"},"_meta":{` +
	`"io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"t","version":"1"},` +
	`"io.modelcontextprotocol/protocolVersion":"` + mcp.LATEST_PROTOCOL_VERSION + `",` +
	`"traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}}}`

// echoServer is a server with opts and one tool, echo, that answers "ok".
func echoServer(opts ...ServerOption) *MCPServer {
	s := NewMCPServer("trace-srv", "1.0", append([]ServerOption{WithToolCapabilities(false)}, opts...)...)
	s.AddTool(mcp.NewToolWithRawSchema("echo", "", json.RawMessage(`{"type":"object"}`)),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("ok"), nil
		})
	return s
}

// requireEchoAnswered fails t unless s answers tracedToolCall with "ok".
func requireEchoAnswered(t *testing.T, s *MCPServer) {
	t.Helper()
	got, err := json.Marshal(s.HandleMessage(t.Context(), json.RawMessage(tracedToolCall)))
	require.NoError(t, err)
	require.Contains(t, string(got), `"ok"`, "the call was not answered: %s", got)
}

// tracedCallAllocs is the fewest allocations of one tracedToolCall over ten
// measurements, on a server with opts.
func tracedCallAllocs(t *testing.T, opts ...ServerOption) float64 {
	t.Helper()
	s := echoServer(opts...)
	requireEchoAnswered(t, s)
	ctx := t.Context()
	message := json.RawMessage(tracedToolCall)
	fewest := testing.AllocsPerRun(200, func() { s.HandleMessage(ctx, message) })
	for range 9 {
		fewest = min(fewest, testing.AllocsPerRun(200, func() { s.HandleMessage(ctx, message) }))
	}
	return fewest
}

// WithMetaPropagator(nil) installs the no-op, which extracts nothing: a
// request costs what it costs with no propagator, its _meta not decoded.
func TestWithMetaPropagator_TheNoopDecodesNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector's instrumentation allocates")
	}
	none := tracedCallAllocs(t)
	for _, tt := range []struct {
		name string
		opt  ServerOption
	}{
		{"nil", WithMetaPropagator(nil)},
		{"explicit no-op", WithMetaPropagator(tracing.NoopMetaPropagator())},
	} {
		if got := tracedCallAllocs(t, tt.opt); got > none {
			t.Errorf("%s: %v allocations per call, want at most %v, as with no propagator", tt.name, got, none)
		}
	}
}

// uncomparablePropagator's type holds a slice, so comparing two of its values
// with == panics.
type uncomparablePropagator struct{ seen []string }

func (uncomparablePropagator) InjectMeta(_ context.Context, meta *mcp.Meta) *mcp.Meta { return meta }
func (uncomparablePropagator) ExtractMeta(ctx context.Context, _ *mcp.Meta) context.Context {
	return ctx
}

// Telling the no-op apart from the installed propagator must not panic on a
// propagator whose type cannot be compared.
func TestWithMetaPropagator_AnUncomparablePropagatorIsServed(t *testing.T) {
	s := echoServer(WithMetaPropagator(uncomparablePropagator{seen: []string{}}))
	requireEchoAnswered(t, s)
}

// traceparentPropagator records the traceparent of the _meta it extracts.
type traceparentPropagator struct{ seen *string }

func (traceparentPropagator) InjectMeta(_ context.Context, meta *mcp.Meta) *mcp.Meta { return meta }
func (p traceparentPropagator) ExtractMeta(ctx context.Context, meta *mcp.Meta) context.Context {
	if meta != nil {
		*p.seen, _ = meta.AdditionalFields["traceparent"].(string)
	}
	return ctx
}

// A propagator other than the no-op still sees the request's _meta.
func TestWithMetaPropagator_ARealOneStillExtracts(t *testing.T) {
	seen := ""
	s := echoServer(WithMetaPropagator(traceparentPropagator{seen: &seen}))
	requireEchoAnswered(t, s)
	assert.Equal(t, "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", seen)
}
