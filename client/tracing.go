package client

import (
	"context"
	"maps"
	"net/http"
	"reflect"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/tracing"
)

const attrMethod = "mcp.method"

// WithTracer installs a tracer on the client. The tracer starts a client-kind
// span around every outgoing JSON-RPC method ("mcp.<method>"). For end-to-end
// propagation install a Propagator with WithPropagator as well. A nil tracer
// is treated as a no-op.
func WithTracer(tracer tracing.Tracer) ClientOption {
	if tracer == nil {
		tracer = tracing.NoopTracer()
	}
	return func(c *Client) {
		c.tracer = tracer
	}
}

// WithPropagator installs a propagator that injects trace context into
// outgoing request headers. A nil propagator is treated as a no-op.
func WithPropagator(p tracing.Propagator) ClientOption {
	if p == nil {
		p = tracing.NoopPropagator()
	}
	return func(c *Client) {
		c.propagator = p
	}
}

// WithMetaPropagator installs a propagator that injects trace context into
// the MCP _meta property bag of outgoing requests (per SEP-414). It covers
// all transports including stdio, where HTTP headers are unavailable.
// A nil propagator is treated as a no-op.
func WithMetaPropagator(p tracing.MetaPropagator) ClientOption {
	if p == nil {
		p = tracing.NoopMetaPropagator()
	}
	// The no-op, given or substituted, injects nothing: a request without
	// a Meta goes out without one.
	injects := reflect.TypeOf(p) != reflect.TypeOf(tracing.NoopMetaPropagator())
	return func(c *Client) {
		c.metaPropagator = p
		c.metaInjects = injects
	}
}

// injectMeta hands the propagator a copy of the request's Meta: the caller
// keeps its own, so a request value reused or shared across goroutines
// carries no trace context from an earlier call and is never written to. A
// request without a Meta is handed none; the propagator makes one if it has
// something to inject. Without a propagator the Meta goes out as it is.
func (c *Client) injectMeta(ctx context.Context, meta *mcp.Meta) *mcp.Meta {
	if !c.metaInjects {
		return meta
	}
	var own *mcp.Meta
	if meta != nil {
		own = &mcp.Meta{ProgressToken: meta.ProgressToken, AdditionalFields: maps.Clone(meta.AdditionalFields)}
	}
	return c.metaPropagator.InjectMeta(ctx, own)
}

func (c *Client) startSendSpan(
	ctx context.Context,
	method string,
	header http.Header,
) (context.Context, http.Header, tracing.Span) {
	tracer := c.tracer
	if tracer == nil {
		tracer = tracing.NoopTracer()
	}
	propagator := c.propagator
	if propagator == nil {
		propagator = tracing.NoopPropagator()
	}

	ctx, span := tracer.Start(ctx, "mcp."+method, tracing.SpanKindClient,
		tracing.String(attrMethod, method),
	)

	if header == nil {
		header = make(http.Header)
	} else {
		header = header.Clone()
	}
	propagator.Inject(ctx, header)

	return ctx, header, span
}

func endSendSpan(span tracing.Span, err error) {
	if err != nil {
		span.SetStatus(tracing.StatusError, err.Error())
		span.RecordError(err)
	}
	span.End()
}
