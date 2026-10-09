package server

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// TestRequestProtocolInfoReadsMetaByItsKeys pins how a request's _meta is
// read: the four well-known keys decide the era and the identity, each on
// its own, so a malformed optional value is ignored while the capabilities
// are the one value a modern request cannot do without.
func TestRequestProtocolInfoReadsMetaByItsKeys(t *testing.T) {
	const modern = `"io.modelcontextprotocol/protocolVersion":"2026-07-28"`
	const caps = `"io.modelcontextprotocol/clientCapabilities":{}`

	tests := []struct {
		name    string
		params  string
		want    *RequestProtocolInfo
		wantErr string
	}{
		{"no _meta is legacy", `{}`, &RequestProtocolInfo{}, ""},
		{"_meta null is legacy", `{"_meta":null}`, &RequestProtocolInfo{}, ""},
		{"_meta that is not an object is legacy", `{"_meta":"x"}`, &RequestProtocolInfo{}, ""},
		{"a handshake-era version is legacy and kept", `{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-11-25"}}`,
			&RequestProtocolInfo{ProtocolVersion: "2025-11-25"}, ""},
		{"a version that is not a string is legacy", `{"_meta":{"io.modelcontextprotocol/protocolVersion":2026}}`, &RequestProtocolInfo{}, ""},
		{"modern with empty capabilities", `{"_meta":{` + modern + `,` + caps + `}}`,
			&RequestProtocolInfo{Modern: true, ProtocolVersion: "2026-07-28", ClientCapabilities: &mcp.ClientCapabilities{}}, ""},
		{"modern with client info and log level", `{"_meta":{` + modern + `,` + caps + `,"io.modelcontextprotocol/clientInfo":{"name":"c","version":"1"},"io.modelcontextprotocol/logLevel":"debug"}}`,
			&RequestProtocolInfo{Modern: true, ProtocolVersion: "2026-07-28", ClientCapabilities: &mcp.ClientCapabilities{},
				ClientInfo: &mcp.Implementation{Name: "c", Version: "1"}, LogLevel: mcp.LoggingLevelDebug}, ""},
		{"malformed client info is ignored", `{"_meta":{` + modern + `,` + caps + `,"io.modelcontextprotocol/clientInfo":"c"}}`,
			&RequestProtocolInfo{Modern: true, ProtocolVersion: "2026-07-28", ClientCapabilities: &mcp.ClientCapabilities{}}, ""},
		{"malformed log level is ignored", `{"_meta":{` + modern + `,` + caps + `,"io.modelcontextprotocol/logLevel":5}}`,
			&RequestProtocolInfo{Modern: true, ProtocolVersion: "2026-07-28", ClientCapabilities: &mcp.ClientCapabilities{}}, ""},
		{"key case is not significant, as encoding/json matches field tags", `{"_meta":{"IO.modelcontextprotocol/protocolversion":"2026-07-28","io.modelcontextprotocol/CLIENTCAPABILITIES":{}}}`,
			&RequestProtocolInfo{Modern: true, ProtocolVersion: "2026-07-28", ClientCapabilities: &mcp.ClientCapabilities{}}, ""},
		{"modern without capabilities is an error", `{"_meta":{` + modern + `}}`, nil, "missing or invalid _meta field io.modelcontextprotocol/clientCapabilities"},
		{"modern with null capabilities is an error", `{"_meta":{` + modern + `,"io.modelcontextprotocol/clientCapabilities":null}}`, nil, "missing or invalid _meta field io.modelcontextprotocol/clientCapabilities"},
		{"modern with malformed capabilities is an error", `{"_meta":{` + modern + `,"io.modelcontextprotocol/clientCapabilities":5}}`, nil, "missing or invalid _meta field io.modelcontextprotocol/clientCapabilities"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			message := json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":` + tt.params + `}`)

			info, err := extractRequestProtocolInfo(message)

			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				assert.Nil(t, info)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, info)
		})
	}
}
