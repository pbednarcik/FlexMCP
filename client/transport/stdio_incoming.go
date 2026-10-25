package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/pbednarcik/flexmcp/internal/jsonrpc"
	"github.com/pbednarcik/flexmcp/mcp"
)

// readResponses continuously reads and processes responses from the server's stdout.
// It handles both responses to requests and notifications, routing them appropriately.
// Runs until the done channel is closed or an error occurs reading from stdout.
// The ready channel, if non-nil, is closed once the read loop is entered, signaling
// to Start() that the transport is actively processing responses.
func (c *Stdio) readResponses(ready chan struct{}) {
	// Each line is read into buf, reused from line to line: every decode
	// below copies what it keeps. The envelope each line is first decoded
	// into is made once and emptied for every line, as json.Unmarshal would
	// otherwise make it on the heap for each.
	var buf []byte
	type envelope struct {
		JSONRPC jsonrpcVersion  `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Method  jsonrpc.Method  `json:"method"`
		Result  json.RawMessage `json:"result,omitempty"`
		Error   json.RawMessage `json:"error,omitempty"`
	}
	baseMessage := new(envelope)
	for {
		// Signal readiness on the first iteration, inside the loop, so that
		// Start() only unblocks after the reader is actively processing.
		if ready != nil {
			close(ready)
			ready = nil
		}
		select {
		case <-c.done:
			return
		default:
			encoded, err := readLine(c.stdout, buf)
			buf = encoded[:0]
			if cap(buf) > maxRetainedLine {
				buf = nil
			}
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, context.Canceled) && !errors.Is(err, fs.ErrClosed) {
					c.logger.Error("Error reading from stdout", "err", err)
				}
				// Signal done so in-flight SendRequest calls unblock
				// instead of hanging forever when the server dies.
				c.closeDone()
				return
			}

			// First try to parse as a generic message to check for ID field
			*baseMessage = envelope{}
			if err := json.Unmarshal(encoded, baseMessage); err != nil {
				continue
			}
			// The id is a value as the decoder gave it, so it is read by its
			// own UnmarshalJSON, called directly: through json.Unmarshal it
			// would be made on the heap.
			var id mcp.RequestId
			readableID := len(baseMessage.ID) > 0 && id.UnmarshalJSON(baseMessage.ID) == nil && !id.IsNil()

			// A request id is a string or an integer and never null: a
			// request whose id cannot be read is answered as invalid, with a
			// null id, and a response without one cannot be matched to a
			// request, so it is logged.
			if len(baseMessage.ID) > 0 && !readableID {
				if baseMessage.Method.Present {
					c.sendResponse(*NewJSONRPCErrorResponse(mcp.NewRequestId(nil), mcp.INVALID_REQUEST, "Invalid request id", nil))
				} else {
					c.logger.Error("Dropping a response without a readable id", "id", string(baseMessage.ID), "error", string(baseMessage.Error))
				}
				continue
			}

			// A message with a method member is a request or a notification,
			// whatever its value; only one without is a response. A method that
			// is not a string makes an invalid request.
			if baseMessage.Method.Present && !baseMessage.Method.IsString {
				answerID := mcp.NewRequestId(nil)
				if readableID {
					answerID = id
				}
				c.sendResponse(*NewJSONRPCErrorResponse(answerID, mcp.INVALID_REQUEST, "Invalid request: method must be a string", nil))
				continue
			}

			// If it has a method but no ID, it's a notification
			if baseMessage.Method.Present && len(baseMessage.ID) == 0 {
				var notification mcp.JSONRPCNotification
				if err := json.Unmarshal(encoded, &notification); err != nil {
					continue
				}
				// The handler runs outside the lock, so one that sets the
				// handler does not deadlock the read loop.
				c.notifyMu.RLock()
				handler := c.onNotification
				c.notifyMu.RUnlock()
				if handler != nil {
					handler(notification)
				}
				continue
			}

			// If it has a method and an ID, it's an incoming request, and never
			// a response, even when it cannot be decoded.
			if baseMessage.Method.Present {
				if !baseMessage.JSONRPC.is20 {
					c.sendResponse(*invalidVersionResponse(id))
					continue
				}
				var request JSONRPCRequest
				if err := json.Unmarshal(encoded, &request); err != nil {
					c.logger.Error("Dropping a server request that cannot be decoded", "err", err)
					continue
				}
				c.handleIncomingRequest(request)
				continue
			}

			// Otherwise, it's a response to our request, built from the one
			// decode above. One whose error cannot be decoded fails the request
			// it names, rather than leave it waiting for its deadline.
			// A response is taken whatever its jsonrpc member says; one that
			// is not "2.0" is recorded with none.
			var version string
			if baseMessage.JSONRPC.is20 {
				version = mcp.JSONRPC_VERSION
			}
			response := JSONRPCResponse{JSONRPC: version, ID: id, Result: baseMessage.Result}
			if len(baseMessage.Error) > 0 && string(baseMessage.Error) != "null" {
				var details mcp.JSONRPCErrorDetails
				if err := json.Unmarshal(baseMessage.Error, &details); err != nil {
					c.logger.Error("Failing a request whose response cannot be decoded", "id", id.String(), "err", err)
					c.answer(responseKeyOf(id), nil)
					continue
				}
				response.Error = &details
			}
			c.answer(responseKeyOf(id), &response)
		}
	}
}

// answer hands response to the request waiting under idKey, if any; a nil
// response tells it the answer could not be decoded.
func (c *Stdio) answer(idKey responseKey, response *JSONRPCResponse) {
	c.mu.RLock()
	ch, exists := c.responses[idKey]
	c.mu.RUnlock()
	if exists {
		ch <- response
		c.mu.Lock()
		delete(c.responses, idKey)
		c.mu.Unlock()
	}
}

// handleIncomingRequest processes incoming requests from the server.
// It calls the registered request handler and sends the response back to the server.
func (c *Stdio) handleIncomingRequest(request JSONRPCRequest) {
	c.requestMu.RLock()
	handler := c.onRequest
	c.requestMu.RUnlock()

	if handler == nil {
		// Send error response if no handler is configured
		errorResponse := *NewJSONRPCErrorResponse(
			request.ID,
			mcp.METHOD_NOT_FOUND,
			"No request handler configured",
			nil,
		)
		c.sendResponse(errorResponse)
		return
	}

	// Handle the request in a goroutine to avoid blocking
	go func() {
		// A panicking handler fails its request, as on Streamable HTTP,
		// rather than end the process.
		defer func() {
			if r := recover(); r != nil {
				c.logger.Error("panic handling server request", "method", request.Method, "panic", r)
				c.sendResponse(*NewJSONRPCErrorResponse(request.ID, mcp.INTERNAL_ERROR, fmt.Sprintf("internal error: panic in request handler: %v", r), nil))
			}
		}()
		c.ctxMu.RLock()
		ctx := c.ctx
		c.ctxMu.RUnlock()

		// Check if context is already cancelled before processing
		select {
		case <-ctx.Done():
			errorResponse := *NewJSONRPCErrorResponse(request.ID, mcp.INTERNAL_ERROR, ctx.Err().Error(), nil)
			c.sendResponse(errorResponse)
			return
		default:
		}

		response, err := handler(ctx, request)
		if err != nil {
			errorResponse := *NewJSONRPCErrorResponse(request.ID, handlerErrorCode(err), err.Error(), nil)
			c.sendResponse(errorResponse)
			return
		}

		if response != nil {
			c.sendResponse(*response)
		}
	}()
}

// sendResponse sends a response back to the server, unless the transport is
// closed and stdin with it.
func (c *Stdio) sendResponse(response JSONRPCResponse) {
	select {
	case <-c.done:
		return
	default:
	}
	responseBytes, err := json.Marshal(response)
	if err != nil {
		c.logger.Error("Error marshaling response", "err", err)
		return
	}
	responseBytes = append(responseBytes, '\n')

	// An answer is owed whatever the handler's context did; Close, closing
	// stdin, bounds the write.
	if err := c.writeLine(context.Background(), responseBytes); err != nil && !errors.Is(err, ErrTransportClosed) {
		c.logger.Error("Error writing response", "err", err)
	}
}
