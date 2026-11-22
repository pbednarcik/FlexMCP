package client

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// listedTool is a tool called name as a client holds it after tools/list,
// decoded from the wire: an annotated region property and, for size > 0,
// plain ones until its schema is about size bytes.
func listedTool(t *testing.T, name string, size int) mcp.Tool {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"name":"` + name + `","inputSchema":{"type":"object","properties":{"region":{"type":"string","x-mcp-header":"Region"}`)
	for i := 0; b.Len() < size; i++ {
		fmt.Fprintf(&b, `,"p%03d":{"type":"string","description":"property number %03d"}`, i, i)
	}
	b.WriteString(`}}}`)
	var tool mcp.Tool
	require.NoError(t, json.Unmarshal([]byte(b.String()), &tool))
	return tool
}

// A tools/call mirrors a listed tool's headers from what the listing kept,
// without reading the tool's schema again: the call costs as many
// allocations for a tool with an 8 KB schema as for one with a single
// property, and still mirrors the header.
func TestCallHeadersDoNotReadTheListedSchemaAgain(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	c := NewClient(&recordingTransport{})
	c.updateEra(func(e *era) { e.version = mcp.LATEST_PROTOCOL_VERSION })
	c.knownTools = toolBindings([]mcp.Tool{listedTool(t, "small", 0), listedTool(t, "big", 8192)})
	e := c.currentEra()
	headersFor := func(name string) func() {
		given := mcp.CallToolParams{Name: name, Arguments: map[string]any{"region": "eu-west"}}
		params, err := json.Marshal(given)
		require.NoError(t, err)
		return func() {
			header := c.applyStandardHeaders(e, nil, string(mcp.MethodToolsCall), json.RawMessage(params), given)
			if header.Get("Mcp-Param-Region") != "eu-west" {
				t.Errorf("%s: Mcp-Param-Region is %q", name, header.Get("Mcp-Param-Region"))
			}
		}
	}
	small, big := headersFor("small"), headersFor("big")
	assert.Equal(t, testing.AllocsPerRun(100, small), testing.AllocsPerRun(100, big),
		"a call to the tool with the 8 KB schema allocates as one to the small tool does")
}
