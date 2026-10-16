package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// handlerErrors are the errors a request handler returns and the JSON-RPC
// code each is answered with (JSON-RPC 2.0, section 5.1).
var handlerErrors = map[string]struct {
	err  error
	code int
}{
	"method not found": {fmt.Errorf("%w: no/such", mcp.ErrMethodNotFound), mcp.METHOD_NOT_FOUND},
	"invalid params":   {fmt.Errorf("%w: bad", mcp.ErrInvalidParams), mcp.INVALID_PARAMS},
	"any other":        {errors.New("broke"), mcp.INTERNAL_ERROR},
}

// A request handler's error is answered with the code its kind calls for,
// not -32603 for all: an unknown method is -32601, bad params -32602.
func TestStdio_AHandlerErrorIsAnsweredWithItsCode(t *testing.T) {
	for name, tt := range handlerErrors {
		t.Run(name, func(t *testing.T) {
			stdio := newBareStdio()
			stdio.ctx = t.Context()
			sink := &concurrentBuffer{}
			stdio.stdin = sink
			stdio.SetRequestHandler(func(context.Context, JSONRPCRequest) (*JSONRPCResponse, error) {
				return nil, tt.err
			})

			stdio.handleIncomingRequest(JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(1)), Method: "no/such"})
			require.Eventually(t, func() bool { return strings.Contains(sink.String(), `"code"`) }, 5*time.Second, time.Millisecond)
			var response JSONRPCResponse
			require.NoError(t, json.Unmarshal([]byte(sink.String()), &response))
			require.NotNil(t, response.Error)
			assert.Equal(t, tt.code, response.Error.Code)
		})
	}
}

func TestStreamableHTTP_AHandlerErrorIsAnsweredWithItsCode(t *testing.T) {
	for name, tt := range handlerErrors {
		t.Run(name, func(t *testing.T) {
			codes := make(chan int, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Error *struct {
						Code int `json:"code"`
					} `json:"error"`
				}
				if r.Method == http.MethodPost && json.NewDecoder(r.Body).Decode(&body) == nil && body.Error != nil {
					codes <- body.Error.Code
				}
				w.WriteHeader(http.StatusAccepted)
			}))
			t.Cleanup(server.Close)
			client, err := NewStreamableHTTP(server.URL)
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close() })
			client.SetRequestHandler(func(context.Context, JSONRPCRequest) (*JSONRPCResponse, error) {
				return nil, tt.err
			})
			require.NoError(t, client.Start(t.Context()))

			client.handleIncomingRequest(t.Context(), JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(1)), Method: "no/such"})
			select {
			case code := <-codes:
				assert.Equal(t, tt.code, code)
			case <-time.After(5 * time.Second):
				t.Fatal("no error response was posted")
			}
		})
	}
}

// A request handler that panics fails its request, answered as an internal
// error, as on Streamable HTTP; the panic does not end the process.
func TestStdio_AHandlerPanicIsAnsweredNotFatal(t *testing.T) {
	stdio := newBareStdio()
	stdio.ctx = t.Context()
	sink := &concurrentBuffer{}
	stdio.stdin = sink
	stdio.SetRequestHandler(func(context.Context, JSONRPCRequest) (*JSONRPCResponse, error) {
		panic("handler broke")
	})

	stdio.handleIncomingRequest(JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(1)), Method: "roots/list"})
	require.Eventually(t, func() bool { return strings.Contains(sink.String(), `"code"`) }, 5*time.Second, time.Millisecond)
	var response JSONRPCResponse
	require.NoError(t, json.Unmarshal([]byte(sink.String()), &response))
	require.NotNil(t, response.Error)
	assert.Equal(t, mcp.INTERNAL_ERROR, response.Error.Code)
}
