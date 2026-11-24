package transport

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/pbednarcik/flexmcp/mcp"
)

// HTTPHeaderFunc is a function that extracts header entries from the given context
// and returns them as key-value pairs. This is typically used to add context values
// as HTTP headers in outgoing requests.
type HTTPHeaderFunc func(context.Context) map[string]string

// Interface for the transport layer.
//
// The stdio, SSE and Streamable HTTP transports read the server's messages
// leniently, as encoding/json reads them: a member whose name folds onto a
// defined one, such as "ID" or "Result", is read as that member, and of a
// member sent twice the last is kept. The server, unlike them, refuses a
// request spelled either way (upstream #1166). An HTTP status other than 200
// or 202 is the exception: Streamable HTTP takes its body for the answer only
// when it is a JSON-RPC error whose jsonrpc, error, code and message members
// are spelled exactly, and reports the status otherwise; SSE, whose answers
// come on its event stream, never takes such a body for one.
type Interface interface {
	// Start the connection. The client calls it once; the stdio, SSE and
	// in-process transports ignore a second call on a started transport.
	Start(ctx context.Context) error

	// SendRequest sends a json RPC request and returns the response synchronously.
	SendRequest(ctx context.Context, request JSONRPCRequest) (*JSONRPCResponse, error)

	// SendNotification sends a json RPC Notification to the server.
	SendNotification(ctx context.Context, notification mcp.JSONRPCNotification) error

	// SetNotificationHandler sets the handler for notifications.
	// Any notification before the handler is set will be discarded.
	SetNotificationHandler(handler func(notification mcp.JSONRPCNotification))

	// Close the connection.
	Close() error

	// GetSessionId returns the session ID of the transport.
	GetSessionId() string
}

// RequestHandler defines a function that handles incoming requests from the server.
type RequestHandler func(ctx context.Context, request JSONRPCRequest) (*JSONRPCResponse, error)

// BidirectionalInterface extends Interface to support incoming requests from the server.
// This is used for features like sampling where the server can send requests to the client.
type BidirectionalInterface interface {
	Interface

	// SetRequestHandler sets the handler for incoming requests from the server.
	// The handler should process the request and return a response.
	SetRequestHandler(handler RequestHandler)
}

// HTTPConnection is a Transport that runs over HTTP and supports
// protocol version headers.
type HTTPConnection interface {
	Interface
	SetProtocolVersion(version string)
}

type JSONRPCRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      mcp.RequestId `json:"id"`
	Method  string        `json:"method"`
	Params  any           `json:"params,omitempty"`
	Header  http.Header   `json:"-"`
}

// JSONRPCResponse represents a JSON-RPC 2.0 response message.
// Use NewJSONRPCResultResponse to create a JSONRPCResponse with a result.
// Use NewJSONRPCErrorResponse to create a JSONRPCResponse with an error.
type JSONRPCResponse struct {
	JSONRPC string                   `json:"jsonrpc"`
	ID      mcp.RequestId            `json:"id"`
	Result  json.RawMessage          `json:"result,omitempty"`
	Error   *mcp.JSONRPCErrorDetails `json:"error,omitempty"`
}
