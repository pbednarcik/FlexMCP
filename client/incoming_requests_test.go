package client

import (
	"context"
	"errors"
	"testing"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type failingSamplingHandler struct{ err error }

func (h failingSamplingHandler) CreateMessage(context.Context, mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
	return nil, h.err
}

// A server's requests are answered on the wire: ping with an empty result,
// and anything the client cannot serve with an error naming why.
func TestServerRequestsAreAnsweredOnTheWire(t *testing.T) {
	boom := errors.New("handler exploded")
	p := newPipePeer(t,
		WithSamplingHandler(failingSamplingHandler{err: boom}),
		WithElicitationHandler(acceptingElicitationHandler{}),
		WithRootsHandler(fixedRootsHandler{err: boom}),
	)

	tests := []struct {
		name    string
		request string
		wantErr string
	}{
		{
			name:    "ping",
			request: `{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		},
		{
			name:    "an unknown method",
			request: `{"jsonrpc":"2.0","id":2,"method":"nope"}`,
			wantErr: "unsupported request method: nope",
		},
		{
			name:    "sampling with malformed params",
			request: `{"jsonrpc":"2.0","id":3,"method":"sampling/createMessage","params":"x"}`,
			wantErr: "failed to unmarshal params",
		},
		{
			name:    "sampling with unknown content",
			request: `{"jsonrpc":"2.0","id":4,"method":"sampling/createMessage","params":{"maxTokens":1,"messages":[{"role":"user","content":{"type":"bogus"}}]}}`,
			wantErr: "failed to parse content for message 0",
		},
		{
			name:    "a failing sampling handler",
			request: `{"jsonrpc":"2.0","id":5,"method":"sampling/createMessage","params":{"maxTokens":1,"messages":[{"role":"user","content":{"type":"text","text":"hi"}}]}}`,
			wantErr: "handler exploded",
		},
		{
			name:    "elicitation with malformed params",
			request: `{"jsonrpc":"2.0","id":6,"method":"elicitation/create","params":"x"}`,
			wantErr: "failed to unmarshal params",
		},
		{
			name:    "elicitation without a schema",
			request: `{"jsonrpc":"2.0","id":7,"method":"elicitation/create","params":{"message":"Your name?"}}`,
			wantErr: "invalid elicitation params",
		},
		{
			name:    "a failing roots handler",
			request: `{"jsonrpc":"2.0","id":8,"method":"roots/list"}`,
			wantErr: "handler exploded",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := p.write.Write([]byte(tt.request + "\n"))
			require.NoError(t, err)

			response := p.next()
			if tt.wantErr != "" {
				errorBody, _ := response["error"].(map[string]any)
				require.NotNilf(t, errorBody, "expected an error response, got %v", response)
				assert.Contains(t, errorBody["message"], tt.wantErr)
				return
			}
			assert.Nil(t, response["error"])
			assert.Equal(t, map[string]any{}, response["result"])
		})
	}
}
