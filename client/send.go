package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/pbednarcik/flexmcp/client/transport"
	"github.com/pbednarcik/flexmcp/mcp"
)

// sendRequest sends a JSON-RPC request to the server and waits for a response.
// Returns the raw JSON response message or an error if the request fails.
func (c *Client) sendRequest(
	ctx context.Context,
	method string,
	params any,
	header http.Header,
) (*json.RawMessage, error) {
	return c.sendRequestIn(ctx, c.currentEra(), method, params, header)
}

// sendRequestIn sends a request as a request of era e: the era in effect,
// or the one a probe tries.
func (c *Client) sendRequestIn(
	ctx context.Context,
	e *era,
	method string,
	params any,
	header http.Header,
) (*json.RawMessage, error) {
	if !c.initialized.Load() && method != "initialize" && method != string(mcp.MethodServerDiscover) {
		return nil, errors.New("client not initialized")
	}

	// Protocol version 2026-07-28 carries the protocol version, client
	// identity, and client capabilities in the _meta of every request, and
	// mirrors the method and name into HTTP headers (SEP-2575, SEP-2243).
	// Both are no-ops on legacy connections.
	given := params
	params = c.applyRequestMeta(e, params)
	header = c.applyStandardHeaders(e, header, method, params, given)

	id := c.requestID.Add(1)

	ctx, header, span := c.startSendSpan(ctx, method, header)

	request := transport.JSONRPCRequest{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      mcp.NewRequestId(id),
		Method:  method,
		Params:  params,
		Header:  header,
	}

	// A request whose context has already ended isn't sent, so there is
	// nothing to cancel.
	if err := ctx.Err(); err != nil {
		err = transport.NewError(err)
		endSendSpan(span, err)
		return nil, err
	}

	response, err := c.transport.SendRequest(ctx, request)
	if err != nil {
		// A transport's own timeout ends the wait as the context's deadline
		// would, and is cancelled the same way.
		reason := ctx.Err()
		if reason == nil && errors.Is(err, context.DeadlineExceeded) {
			reason = context.DeadlineExceeded
		}
		if reason != nil {
			c.cancelRequest(ctx, request, reason)
		}
		err = transport.NewError(err)
		endSendSpan(span, err)
		return nil, err
	}

	if response.Error != nil {
		err := response.Error.AsError()
		endSendSpan(span, err)
		return nil, err
	}

	endSendSpan(span, nil)
	return &response.Result, nil
}

// cancelRequest tells the server to stop working on a request whose context
// ended, or that timed out, before the response arrived, with a
// notifications/cancelled that references it and gives reason. Over stdio that is the only way to cancel a request, and
// protocol version 2026-07-28 requires it there.
//
// Over HTTP on protocol version 2026-07-28 nothing is sent: the transport
// ending the request's stream is the cancellation. Before that version a
// closed stream is not one, so the notification goes out there too. The
// handshake is left alone, since initialize must not be cancelled and
// nothing else is sent before it completes, and so are task-augmented
// requests, which are cancelled with tasks/cancel instead.
func (c *Client) cancelRequest(ctx context.Context, request transport.JSONRPCRequest, reason error) {
	if !c.initialized.Load() ||
		request.Method == string(mcp.MethodInitialize) ||
		request.Method == string(mcp.MethodServerDiscover) ||
		isTaskAugmented(request.Params) {
		return
	}
	if _, ok := c.transport.(transport.HTTPConnection); ok && c.isModern() {
		return
	}
	notification := mcp.JSONRPCNotification{
		JSONRPC: mcp.JSONRPC_VERSION,
		Method:  string(mcp.MethodNotificationCancelled),
		Params: mcp.NotificationParams{
			AdditionalFields: map[string]any{"requestId": request.ID, "reason": reason.Error()},
		},
	}
	// The request's context may have ended, so send with one of our own, and off
	// the caller's path: the caller already has its error, and a peer that
	// doesn't read must not hold it up. The notification is best effort.
	ctx = context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(ctx, cancelNotificationTimeout)
		defer cancel()
		_ = c.transport.SendNotification(ctx, notification)
	}()
}

// isTaskAugmented reports whether params ask for the request to run as a
// task.
func isTaskAugmented(params any) bool {
	encoded, err := json.Marshal(params)
	if err != nil {
		return false
	}
	var fields struct {
		Task json.RawMessage `json:"task"`
	}
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return false
	}
	return len(fields.Task) > 0 && string(fields.Task) != "null"
}

// cancelNotificationTimeout bounds how long cancelRequest waits to send a
// notifications/cancelled.
const cancelNotificationTimeout = 5 * time.Second

func outboundHeader(header http.Header, requestMethod string) http.Header {
	// A typed request with Method set was decoded from an inbound JSON-RPC
	// message. Its Header contains received HTTP metadata, not opt-in outbound
	// headers for a new client call.
	if requestMethod != "" {
		return nil
	}
	return header
}

// checkResultType refuses a resultType the client does not accept: the
// core values are accepted; "task" belongs to the Tasks extension, which
// answers tools/call alone (see checkToolResultType); any other value is
// invalid rather than complete.
func (c *Client) checkResultType(resultType mcp.ResultType) error {
	return checkResultType(resultType, false, false)
}

// checkInputResultType is checkResultType for prompts/get and
// resources/read, which the multi round-trip pattern may answer with
// "input_required".
func (c *Client) checkInputResultType(resultType mcp.ResultType) error {
	return checkResultType(resultType, false, true)
}

// checkToolResultType is checkResultType for the answer to tools/call, the
// one request the Tasks extension may answer with a task: "task" is accepted
// when this connection declared the extension.
func (c *Client) checkToolResultType(resultType mcp.ResultType) error {
	return checkResultType(resultType, c.acceptsTasks(), true)
}

// errUnacceptedResultType marks a result whose resultType this request may
// not be answered with: a modern server's answer, however wrong.
var errUnacceptedResultType = errors.New("unaccepted resultType")

func checkResultType(resultType mcp.ResultType, tasks, input bool) error {
	if resultType.Accepted(tasks, input) {
		return nil
	}
	return fmt.Errorf("%w %q in the result", errUnacceptedResultType, resultType)
}

// acceptsTasks reports whether this connection declared the Tasks extension:
// extensions["io.modelcontextprotocol/tasks"] in the capabilities every
// modern request carries. Before 2026-07-28 there was no resultType, and the
// tasks capability of that era is the client's own, for the requests it
// receives, so a handshake-era server is never answered with a task.
func (c *Client) acceptsTasks() bool {
	e := c.currentEra()
	if !mcp.IsModernProtocol(e.version) {
		return false
	}
	_, ok := e.clientCapabilities.Extensions[mcp.ExtensionTasks]
	return ok
}
