package client

import (
	"context"
	"fmt"
	"testing"

	"github.com/pbednarcik/flexmcp/client/transport"
	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A transport of the caller's may report a header mismatch as a value or as
// a pointer, bare or wrapped: either way a mismatch about an Mcp-Param-*
// header drops the tool's definition, lists the tools again and retries the
// call once.
func TestCallToolRefreshesOnPointerHeaderMismatch(t *testing.T) {
	mismatch := mcp.HeaderMismatchError{Header: "Mcp-Param-Region", Reason: "changed"}
	// The pointer as an error value: vet's printf check rejects %w on a
	// pointer to a value-receiver error.
	var pointer error = &mismatch
	tests := []struct {
		name string
		err  error
	}{
		{name: "value", err: mismatch},
		{name: "wrapped value", err: fmt.Errorf("sending: %w", mismatch)},
		{name: "pointer", err: pointer},
		{name: "wrapped pointer", err: fmt.Errorf("sending: %w", pointer)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lists, calls := 0, 0
			tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
				switch request.Method {
				case string(mcp.MethodToolsList):
					lists++
					return resultResponse(request, `{"tools":[{"name":"t","inputSchema":{"type":"object"}}]}`), nil
				case string(mcp.MethodToolsCall):
					calls++
					if calls == 1 {
						return nil, tt.err
					}
					return resultResponse(request, `{"content":[{"type":"text","text":"ok"}]}`), nil
				}
				return nil, fmt.Errorf("unexpected %s", request.Method)
			})}
			c := connectScripted(t, tr)
			result, err := c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "t"}})
			require.NoError(t, err)
			require.NotNil(t, result)
			assert.Equal(t, 2, calls)
			assert.Equal(t, 1, lists)
		})
	}
}

// A mismatch about any other header is not the tool's definition changing:
// the error is returned as is, value or pointer, with no listing and no retry.
func TestCallToolKeepsOtherPointerHeaderMismatch(t *testing.T) {
	mismatch := mcp.HeaderMismatchError{Header: "Mcp-Method", Reason: "changed"}
	var pointer error = &mismatch
	for name, sent := range map[string]error{"value": mismatch, "pointer": pointer} {
		t.Run(name, func(t *testing.T) {
			lists, calls := 0, 0
			tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
				switch request.Method {
				case string(mcp.MethodToolsList):
					lists++
					return resultResponse(request, `{"tools":[]}`), nil
				case string(mcp.MethodToolsCall):
					calls++
					return nil, sent
				}
				return nil, fmt.Errorf("unexpected %s", request.Method)
			})}
			c := connectScripted(t, tr)
			_, err := c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "t"}})
			require.ErrorIs(t, err, sent)
			assert.Equal(t, 1, calls)
			assert.Equal(t, 0, lists)
		})
	}
}
