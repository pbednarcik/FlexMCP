package client

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The transport path answers a server's request with what the handler
// returns. A handler that returned no result used to be answered with a null
// result, which servers reject; like the multi round-trip path, it is an
// error now.
func TestHandleIncomingRequestRejectsNilHandlerResult(t *testing.T) {
	tests := []struct {
		name    string
		client  *Client
		method  mcp.MCPMethod
		params  any
		wantErr string
	}{
		{
			name:    "sampling",
			client:  &Client{samplingHandler: nilSamplingHandler{}},
			method:  mcp.MethodSamplingCreateMessage,
			params:  mcp.CreateMessageParams{MaxTokens: 1},
			wantErr: "sampling handler returned no result",
		},
		{
			name:   "elicitation",
			client: &Client{elicitationHandler: nilElicitationHandler{}},
			method: mcp.MethodElicitationCreate,
			params: mcp.ElicitationParams{
				Message:         "hi",
				RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			},
			wantErr: "elicitation handler returned no result",
		},
		{
			name:    "roots",
			client:  &Client{rootsHandler: nilRootsHandler{}},
			method:  mcp.MethodListRoots,
			wantErr: "roots handler returned no result",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response, err := tt.client.handleIncomingRequest(t.Context(), transport.JSONRPCRequest{
				JSONRPC: mcp.JSONRPC_VERSION,
				ID:      mcp.NewRequestId(1),
				Method:  string(tt.method),
				Params:  tt.params,
			})
			assert.Nil(t, response)
			require.EqualError(t, err, tt.wantErr)
		})
	}
}

type noRootsHandler struct{}

func (noRootsHandler) ListRoots(context.Context, mcp.ListRootsRequest) (*mcp.ListRootsResult, error) {
	return &mcp.ListRootsResult{}, nil
}

// A client without roots answers with an empty array, which the schema
// requires, rather than null.
func TestListRootsWithoutRootsSendsEmptyArray(t *testing.T) {
	client := &Client{rootsHandler: noRootsHandler{}}

	response, err := client.handleIncomingRequest(t.Context(), transport.JSONRPCRequest{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      mcp.NewRequestId(1),
		Method:  string(mcp.MethodListRoots),
	})
	require.NoError(t, err)
	assert.JSONEq(t, `{"roots": []}`, string(response.Result))

	// The same goes for the answer to a roots input request.
	inputResponse, err := client.fulfillInputRequest(t.Context(), mcp.NewRootsInputRequest())
	require.NoError(t, err)
	encoded, err := json.Marshal(inputResponse)
	require.NoError(t, err)
	assert.JSONEq(t, `{"roots": []}`, string(encoded))
}
