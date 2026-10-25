package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/client/transport"
	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
)

// capturingScript answers every request after server/discover with an empty
// tools list and hands each such request to seen.
func capturingScript(seen func(transport.JSONRPCRequest)) func(context.Context, int, transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	return modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		seen(request)
		return resultResponse(request, `{"tools":[]}`), nil
	})
}

// An integer progress token above 2^53 reaches the wire with its digits:
// the caller's _meta is carried as the bytes it encodes to, not re-read as
// floating point.
func TestRequestMetaKeepsAnIntegerProgressTokenExact(t *testing.T) {
	var sent json.RawMessage
	tr := &scriptedTransport{respond: capturingScript(func(request transport.JSONRPCRequest) {
		sent, _ = json.Marshal(request.Params)
	})}
	c := connectScripted(t, tr)

	request := mcp.ListToolsRequest{}
	request.Params.Meta = &mcp.Meta{ProgressToken: int64(9007199254740993)}
	_, err := c.ListToolsByPage(t.Context(), request)
	require.NoError(t, err)
	assert.Contains(t, string(sent), `"progressToken":9007199254740993`)
}

// A modern error on the probe other than an unsupported version is a modern
// server's answer: the client reports it rather than trying initialize
// (Transports.md rows 10 and 41).
func TestAModernErrorOnTheProbeIsNotALegacyServer(t *testing.T) {
	for name, code := range map[string]int{"header mismatch": mcp.HEADER_MISMATCH, "missing capability": mcp.MISSING_REQUIRED_CLIENT_CAPABILITY} {
		t.Run(name, func(t *testing.T) {
			tr := &scriptedTransport{respond: func(_ context.Context, _ int, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
				return transport.NewJSONRPCErrorResponse(request.ID, code, "refused", nil), nil
			}}
			c := NewClient(tr)
			require.NoError(t, c.Start(t.Context()))
			_, err := c.Initialize(t.Context(), mcp.InitializeRequest{})
			require.Error(t, err)
			assert.Equal(t, []string{string(mcp.MethodServerDiscover)}, tr.sentMethods(), "no initialize was tried")
		})
	}
}

// A server's version list is not copied into an error whole: the error
// names a few and the count.
func TestTheNoSharedVersionErrorIsBounded(t *testing.T) {
	versions := make([]string, 100000)
	for i := range versions {
		versions[i] = fmt.Sprintf("1999-01-%05d", i)
	}
	encoded, err := json.Marshal(map[string]any{"supportedVersions": versions, "capabilities": map[string]any{}})
	require.NoError(t, err)
	tr := &scriptedTransport{respond: func(_ context.Context, _ int, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		return resultResponse(request, string(encoded)), nil
	}}
	c := NewClient(tr)
	require.NoError(t, c.Start(t.Context()))
	_, err = c.Initialize(t.Context(), mcp.InitializeRequest{})
	require.Error(t, err)
	assert.Less(t, len(err.Error()), 1000, "the error text is bounded: %d bytes", len(err.Error()))
}

// A listing with invalid tools is warned about once, with the count, not
// once per tool.
func TestInvalidHeaderToolsAreWarnedAboutOncePerListing(t *testing.T) {
	var mu sync.Mutex
	var messages []string
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		messages = append(messages, string(p))
		return len(p), nil
	}), nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	tools := make([]map[string]any, 50)
	for i := range tools {
		tools[i] = map[string]any{
			"name":        fmt.Sprintf("t%d", i),
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "object", "x-mcp-header": "Nope"}}},
		}
	}
	listing, err := json.Marshal(map[string]any{"tools": tools})
	require.NoError(t, err)
	tr := &scriptedHTTPTransport{scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		return resultResponse(request, string(listing)), nil
	})}}
	c := NewClient(tr)
	require.NoError(t, c.Start(t.Context()))
	_, err = c.Initialize(t.Context(), mcp.InitializeRequest{})
	require.NoError(t, err)
	result, err := c.ListTools(t.Context(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	assert.Empty(t, result.Tools)

	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, messages, 1, "one warning for the listing: %v", messages)
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// scriptedHTTPTransport is a scripted transport that counts as an HTTP
// connection, so the header rules apply to it.
type scriptedHTTPTransport struct{ scriptedTransport }

func (*scriptedHTTPTransport) SetProtocolVersion(string) {}

// The headers given to Initialize go out on the server/discover probe, as
// they do on the initialize request.
func TestTheProbeCarriesTheCallersHeaders(t *testing.T) {
	var probeHeader http.Header
	tr := &scriptedTransport{respond: func(_ context.Context, _ int, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		if request.Method == string(mcp.MethodServerDiscover) {
			probeHeader = request.Header
			return resultResponse(request, discoverResult), nil
		}
		return resultResponse(request, `{"tools":[]}`), nil
	}}
	c := NewClient(tr)
	require.NoError(t, c.Start(t.Context()))
	_, err := c.Initialize(t.Context(), mcp.InitializeRequest{Header: http.Header{"X-Tenant": []string{"acme"}}})
	require.NoError(t, err)
	assert.Equal(t, "acme", probeHeader.Get("X-Tenant"))
}

// Discover works on a client that has not initialized: it asks at the
// newest version this client speaks, with that version's metadata.
func TestDiscoverWorksBeforeInitialize(t *testing.T) {
	s := server.NewMCPServer("test-server", "1.0.0", server.WithToolCapabilities(true))
	c, err := NewInProcessClientWithOptions(s)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Start(t.Context()))

	result, err := c.Discover(t.Context(), mcp.DiscoverRequest{})
	require.NoError(t, err)
	assert.Contains(t, result.SupportedVersions, mcp.LATEST_PROTOCOL_VERSION)
	assert.Empty(t, c.ProtocolVersion(), "a discover call negotiates nothing")
}

// Initialize beside live callers does not race on the era fields: the
// state a request reads is one snapshot, swapped once.
func TestReinitializeBesideRequestsDoesNotRace(t *testing.T) {
	s := server.NewMCPServer("test-server", "1.0.0", server.WithToolCapabilities(true))
	c := connectInProcess(t, s, "")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 20 {
			_, _ = c.Initialize(t.Context(), mcp.InitializeRequest{})
		}
	}()
	for range 200 {
		_ = c.ProtocolVersion()
		_, _ = c.ListTools(t.Context(), mcp.ListToolsRequest{})
		_ = c.GetServerCapabilities()
	}
	receive(t, done)
	assert.True(t, strings.HasPrefix(c.ProtocolVersion(), "20"))
}

// A request of a modern era over HTTP carries the version in its standard
// headers, the same value as its _meta.
func TestRequestsCarryTheProtocolVersionHeader(t *testing.T) {
	var header http.Header
	tr := &scriptedHTTPTransport{scriptedTransport{respond: capturingScript(func(request transport.JSONRPCRequest) {
		header = request.Header
	})}}
	c := NewClient(tr)
	require.NoError(t, c.Start(t.Context()))
	_, err := c.Initialize(t.Context(), mcp.InitializeRequest{})
	require.NoError(t, err)

	_, err = c.ListToolsByPage(t.Context(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	assert.Equal(t, c.ProtocolVersion(), header.Get(mcp.HeaderProtocolVersion))
}

// versionRecordingTransport records every version the client gives an HTTP
// connection as its own copy.
type versionRecordingTransport struct {
	scriptedTransport
	versions []string
}

func (t *versionRecordingTransport) SetProtocolVersion(version string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.versions = append(t.versions, version)
}

// A probe that fails leaves the transport's version copy as it was: the
// probe's version is set for the probe and taken back.
func TestAFailedProbeRestoresTheTransportsVersion(t *testing.T) {
	tr := &versionRecordingTransport{}
	tr.respond = func(context.Context, int, transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		return nil, errors.New("connection refused")
	}
	c := NewClient(tr)
	require.NoError(t, c.Start(t.Context()))
	_, err := c.Initialize(t.Context(), mcp.InitializeRequest{})
	require.Error(t, err)

	tr.mu.Lock()
	defer tr.mu.Unlock()
	require.NotEmpty(t, tr.versions, "the probe's version was set")
	assert.Equal(t, mcp.LATEST_PROTOCOL_VERSION, tr.versions[0])
	assert.Empty(t, tr.versions[len(tr.versions)-1], "the copy is taken back: %v", tr.versions)
}

// A request of a legacy era over HTTP carries its era's version in the
// header too, so the transport's own copy never stands in for it: a probe
// beside it sets that copy to the version it tries.
func TestALegacyRequestCarriesTheVersionHeaderOfItsEra(t *testing.T) {
	var header http.Header
	tr := &scriptedHTTPTransport{scriptedTransport{respond: func(_ context.Context, _ int, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		switch request.Method {
		case string(mcp.MethodServerDiscover):
			return transport.NewJSONRPCErrorResponse(request.ID, mcp.METHOD_NOT_FOUND, "method not found", nil), nil
		case string(mcp.MethodInitialize):
			return resultResponse(request, legacyInitializeResult), nil
		}
		header = request.Header
		return resultResponse(request, `{"tools":[]}`), nil
	}}}
	c := NewClient(tr)
	require.NoError(t, c.Start(t.Context()))
	_, err := c.Initialize(t.Context(), mcp.InitializeRequest{})
	require.NoError(t, err)
	require.Equal(t, mcp.LATEST_LEGACY_PROTOCOL_VERSION, c.ProtocolVersion())

	_, err = c.ListToolsByPage(t.Context(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	assert.Equal(t, mcp.LATEST_LEGACY_PROTOCOL_VERSION, header.Get(mcp.HeaderProtocolVersion))
}
