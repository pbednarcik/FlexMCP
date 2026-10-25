package transport

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
)

// An in-process request's answer is built from the server's typed response,
// not marshalled and parsed again: the allocations of a ping round trip
// stay within this pin. The marshal-and-parse round trip measured 31 with
// the Close cancellation of each request in place, and 24 before it
// (df25d1e): the cancellation costs more than the typed answer saves.
func TestInProcessRoundTripAllocationsHold(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	const pin = 28

	s := server.NewMCPServer("test-server", "1.0.0")
	tr := NewInProcessTransport(s)
	require.NoError(t, tr.Start(t.Context()))
	t.Cleanup(func() { _ = tr.Close() })
	ctx := t.Context()
	var id int64

	ping := func() {
		id++
		response, err := tr.SendRequest(ctx, JSONRPCRequest{
			JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(id), Method: string(mcp.MethodPing),
		})
		require.NoError(t, err)
		require.Nil(t, response.Error)
	}
	ping()

	allocs := testing.AllocsPerRun(200, ping)
	require.LessOrEqualf(t, allocs, float64(pin), "an in-process ping round trip allocates %.0f times, the pin is %d", allocs, pin)
}
