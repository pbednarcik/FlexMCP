package client

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/pbednarcik/flexmcp/client/transport"
	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingTransport is encodingTransport keeping every request it is sent,
// encoded as the wire carries it.
type recordingTransport struct {
	encodingTransport
	mu       sync.Mutex
	requests []transport.JSONRPCRequest
}

func (t *recordingTransport) SendRequest(ctx context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	t.mu.Lock()
	t.requests = append(t.requests, request)
	t.mu.Unlock()
	return t.encodingTransport.SendRequest(ctx, request)
}

func (t *recordingTransport) last() transport.JSONRPCRequest {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.requests[len(t.requests)-1]
}

// TestClient_AModernToolCallsHeadersNameWhatItsBodyNames pins the Mcp-Method
// and Mcp-Name headers of a modern tools/call to the name its encoded body
// carries, as the server checks them (SEP-2243): a name that is safe ASCII, one
// that is Base64 encoded, and one that is not valid UTF-8, which encoding
// changes. It pins the x-mcp-header mirroring of a tool the client knows too.
func TestClient_AModernToolCallsHeadersNameWhatItsBodyNames(t *testing.T) {
	var region mcp.Tool
	require.NoError(t, json.Unmarshal([]byte(`{"name":"q","inputSchema":{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}}}}`), &region))

	tests := []struct {
		name     string
		toolName string
		region   string
	}{
		{name: "ascii", toolName: "echo"},
		{name: "non-ascii", toolName: "naïve"},
		{name: "invalid utf-8", toolName: "a\xffb"},
		{name: "known tool", toolName: "q", region: "eu-west"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := &recordingTransport{result: json.RawMessage(`{"content":[],"resultType":"complete"}`)}
			c := NewClient(tr)
			c.updateEra(func(e *era) { e.version = mcp.LATEST_PROTOCOL_VERSION })
			c.initialized.Store(true)
			c.knownTools = map[string]mcp.Tool{"q": region}

			var request mcp.CallToolRequest
			request.Params.Name = tt.toolName
			request.Params.Arguments = map[string]any{"region": "eu-west"}
			_, err := c.CallTool(t.Context(), request)
			require.NoError(t, err)

			sent := tr.last()
			encoded, err := json.Marshal(sent.Params)
			require.NoError(t, err)
			var body struct {
				Name string `json:"name"`
			}
			require.NoError(t, json.Unmarshal(encoded, &body))

			assert.Equal(t, string(mcp.MethodToolsCall), sent.Header.Get(mcp.HeaderMethod))
			decoded, ok := mcp.DecodeHeaderValue(sent.Header.Get(mcp.HeaderName))
			require.True(t, ok, "Mcp-Name %q decodes", sent.Header.Get(mcp.HeaderName))
			assert.Equal(t, body.Name, decoded, "Mcp-Name names what the body names")
			assert.Equal(t, tt.region, sent.Header.Get(mcp.HeaderParamPrefix+"Region"))
		})
	}
}
