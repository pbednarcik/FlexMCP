package client

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// stubElicitation answers every elicitation; the test never reaches it.
type stubElicitation struct{}

func (stubElicitation) Elicit(context.Context, mcp.ElicitationRequest) (*mcp.ElicitationResult, error) {
	return &mcp.ElicitationResult{}, nil
}

// A server request the client cannot serve fails with the error kind the
// transport answers with the matching JSON-RPC code: a method it does not
// know, or a feature it has no handler for, is mcp.ErrMethodNotFound (-32601);
// params that do not decode are mcp.ErrInvalidParams (-32602).
func TestClient_ServerRequestErrorsCarryTheirKind(t *testing.T) {
	c := NewClient(nil, WithElicitationHandler(stubElicitation{}))
	tests := map[string]struct {
		request transport.JSONRPCRequest
		want    error
	}{
		"an unknown method":     {transport.JSONRPCRequest{Method: "no/such"}, mcp.ErrMethodNotFound},
		"no sampling handler":   {transport.JSONRPCRequest{Method: string(mcp.MethodSamplingCreateMessage)}, mcp.ErrMethodNotFound},
		"no roots handler":      {transport.JSONRPCRequest{Method: string(mcp.MethodListRoots)}, mcp.ErrMethodNotFound},
		"undecodable params":    {transport.JSONRPCRequest{Method: string(mcp.MethodElicitationCreate), Params: map[string]any{"message": 5}}, mcp.ErrInvalidParams},
		"params that are wrong": {transport.JSONRPCRequest{Method: string(mcp.MethodElicitationCreate), Params: map[string]any{}}, mcp.ErrInvalidParams},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := c.handleIncomingRequest(t.Context(), tt.request)
			require.ErrorIs(t, err, tt.want)
		})
	}
}

// stubSampling answers every sampling request; the test never reaches it.
type stubSampling struct{}

func (stubSampling) CreateMessage(context.Context, mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
	return &mcp.CreateMessageResult{}, nil
}

// Sampling content that does not parse is mcp.ErrInvalidParams (-32602),
// whether it is one block or an array of them.
func TestClient_InvalidSamplingContentIsInvalidParams(t *testing.T) {
	c := NewClient(nil, WithSamplingHandler(stubSampling{}))
	image := map[string]any{"type": "image"}
	tests := map[string]any{
		"one block":           image,
		"a block in an array": []any{map[string]any{"type": "text", "text": "hi"}, image},
		"a non-object item":   []any{"text"},
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			request := transport.JSONRPCRequest{
				Method: string(mcp.MethodSamplingCreateMessage),
				Params: map[string]any{
					"messages":  []any{map[string]any{"role": "user", "content": content}},
					"maxTokens": 1,
				},
			}
			_, err := c.handleIncomingRequest(t.Context(), request)
			require.ErrorIs(t, err, mcp.ErrInvalidParams)
		})
	}
}
