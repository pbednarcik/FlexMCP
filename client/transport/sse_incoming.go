package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/pbednarcik/flexmcp/internal/jsonrpc"
	"github.com/pbednarcik/flexmcp/mcp"
)

// readSSE continuously reads the SSE stream and processes events.
// It runs until the connection is closed or an error occurs. attempt is the
// Start attempt that opened the stream.
func (c *SSE) readSSE(reader io.ReadCloser, endpointChan chan struct{}, endpointOnce *sync.Once, attempt uint64) {
	defer reader.Close()

	br := bufio.NewReader(reader)
	// The line, the event's name and its data are read into buffers kept
	// from one event to the next; one a message grew past maxRetainedLine is
	// let go.
	var line, event, data []byte

	for {
		// when close or start's ctx cancel, the reader will be closed
		// and the for loop will break.
		var err error
		line, err = readLine(br, line)
		if err != nil {
			// Close() cancels the stream context, which surfaces here as a read
			// error; firing the handler would reconnect a transport the caller
			// just shut down.
			if c.closed.Load() {
				return
			}
			// An event the stream ended before its blank line is unfinished,
			// and SSE discards it.
			if c.testHookStreamEnding != nil {
				c.testHookStreamEnding()
			}
			// A stream an earlier attempt opened ends nothing a later one
			// serves. Under lifecycleMu, as Start publishes its success: the
			// end is reported to the handler if Start succeeded first, and
			// by Start's error otherwise. The pending requests are failed
			// under it too: a retry's Start waits for it, so none of the
			// retry's requests is among them. Close sets closed under it as
			// well, so a Close since the check above is seen here: the end of
			// a stream the caller shut down is not reported, and Close fails
			// the pending requests itself.
			c.lifecycleMu.Lock()
			current := !c.closed.Load() && c.attempt.Load() == attempt
			var endStream context.CancelFunc
			if current {
				c.ended.Store(true)
				endStream = c.cancelSSEStream
				c.failPending()
			}
			started := current && c.started.Load()
			c.lifecycleMu.Unlock()
			if !current {
				return
			}
			// The POSTs sent on this stream end with it.
			if endStream != nil {
				endStream()
			}
			if !started {
				return
			}
			c.connectionLostMu.RLock()
			handler := c.onConnectionLost
			c.connectionLostMu.RUnlock()
			if handler != nil {
				// Notify that the connection will be closed due to an error
				handler(err)
			} else {
				c.logger.Error("SSE stream error", "err", err)
			}
			return
		}

		if len(line) == 0 {
			// Empty line means end of event. An event without data is not
			// dispatched, but its name ends with it too, as SSE specifies.
			if len(data) > 0 {
				c.handleSSEEvent(sseEventName(event), data, endpointChan, endpointOnce, attempt)
			}
			event, data = resetEvent(event, data)
			continue
		}

		if after, ok := bytes.CutPrefix(line, []byte("event:")); ok {
			event = append(event[:0], bytes.TrimSpace(after)...)
		} else if after, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = appendSSEDataBytes(data, after)
		}
		if cap(line) > maxRetainedLine {
			line = nil
		}
	}
}

// resetEvent empties an event's name and data for the next event, keeping
// their storage unless a message grew it past maxRetainedLine.
func resetEvent(event, data []byte) ([]byte, []byte) {
	event, data = event[:0], data[:0]
	if cap(event) > maxRetainedLine {
		event = nil
	}
	if cap(data) > maxRetainedLine {
		data = nil
	}
	return event, data
}

// sseEventName is the name an event is handled by: "message" for one the
// stream named none, as SSE specifies.
func sseEventName(event []byte) string {
	switch string(event) {
	case "", "message":
		return "message"
	case "endpoint":
		return "endpoint"
	}
	return string(event)
}

// appendSSEData joins the value of an SSE "data:" field onto any data already
// accumulated for the current event. Per the Server-Sent Events specification,
// a single event may carry multiple "data:" lines and they must be
// concatenated with a newline. The previous implementation overwrote the
// buffer on every line, silently truncating multi-line payloads to their final
// line and corrupting JSON-RPC messages that span more than one data line.
//
// Only a single optional leading space after "data:" is removed, as the spec
// requires; any other leading or trailing whitespace in the value is
// significant and preserved.
func appendSSEData(existing, line string) string {
	value := strings.TrimPrefix(line, " ")
	if existing == "" {
		return value
	}
	return existing + "\n" + value
}

// appendSSEDataBytes is appendSSEData appending into existing's storage.
func appendSSEDataBytes(existing, line []byte) []byte {
	value := bytes.TrimPrefix(line, []byte(" "))
	if len(existing) > 0 {
		existing = append(existing, '\n')
	}
	return append(existing, value...)
}

// handleSSEEvent processes SSE events based on their type.
// Handles 'endpoint' events for connection setup and 'message' events for JSON-RPC communication.
// attempt is the Start attempt whose stream carried the event: once a later
// attempt has begun, the event is dropped, so a superseded reader neither
// replaces the endpoint nor dispatches what it had already read.
func (c *SSE) handleSSEEvent(event string, data []byte, endpointChan chan struct{}, endpointOnce *sync.Once, attempt uint64) {
	if c.attempt.Load() != attempt {
		return
	}
	switch event {
	case "endpoint":
		endpoint, err := c.baseURL.Parse(string(data))
		if err != nil {
			c.logger.Error("Error parsing endpoint URL", "err", err)
			return
		}
		// The endpoint keeps the connection's origin, scheme included: an
		// https connection must not send its requests, and their bearer
		// token, to an http endpoint on the same host.
		if endpoint.Host != c.baseURL.Host || !strings.EqualFold(endpoint.Scheme, c.baseURL.Scheme) ||
			endpoint.User.String() != c.baseURL.User.String() {
			c.logger.Error("Endpoint origin does not match connection origin")
			return
		}
		// Checked again under mu: a later attempt is begun before its reader
		// runs, so a reader of an earlier one that gets here after the later
		// reader published its endpoint sees that it is superseded.
		c.mu.Lock()
		if c.attempt.Load() != attempt {
			c.mu.Unlock()
			return
		}
		c.endpoint, c.endpointURL = endpoint, endpoint.String()
		c.mu.Unlock()
		// The SSE spec sends a single endpoint event, but a proxy or a
		// re-broadcasting server can emit it more than once. Closing the
		// channel twice panics, so guard the close with the attempt's
		// sync.Once.
		endpointOnce.Do(func() {
			close(endpointChan)
		})
	case "message":
		// Every decode below reads these bytes, and none keeps them: they are
		// the reader's, reused for the next event.
		encoded := data
		// A response's result and error are read here too, so a response is
		// decoded once.
		var envelope struct {
			JSONRPC jsonrpcVersion  `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  jsonrpc.Method  `json:"method"`
			Result  json.RawMessage `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(encoded, &envelope); err != nil {
			c.logger.Error("Error unmarshaling message", "err", err)
			return
		}
		var id mcp.RequestId
		readableID := len(envelope.ID) > 0 && id.UnmarshalJSON(envelope.ID) == nil && !id.IsNil()

		// A message with a method member is a request or a notification,
		// whatever its value; only one without is a response.
		switch {
		case envelope.Method.Present && !envelope.Method.IsString:
			answerID := mcp.NewRequestId(nil)
			if readableID {
				answerID = id
			}
			c.answer(NewJSONRPCErrorResponse(answerID, mcp.INVALID_REQUEST, "Invalid request: method must be a string", nil))
			return
		case envelope.Method.Present && len(envelope.ID) == 0:
			var notification mcp.JSONRPCNotification
			if err := json.Unmarshal(encoded, &notification); err != nil {
				return
			}
			// The handler runs outside the lock, so one that sets the handler
			// does not deadlock the stream.
			c.notifyMu.RLock()
			handler := c.onNotification
			c.notifyMu.RUnlock()
			if handler != nil {
				handler(notification)
			}
			return
		case envelope.Method.Present && !readableID:
			// A request id is a string or an integer and never null.
			c.answer(NewJSONRPCErrorResponse(mcp.NewRequestId(nil), mcp.INVALID_REQUEST, "Invalid request id", nil))
			return
		case envelope.Method.Present && !envelope.JSONRPC.is20:
			c.answer(invalidVersionResponse(id))
			return
		case envelope.Method.Name == string(mcp.MethodPing):
			// The receiver of a ping MUST answer it on the earlier revisions.
			c.answer(NewJSONRPCResultResponse(id, json.RawMessage(`{}`)))
			return
		case envelope.Method.Present:
			// A request from the server is never a response, even when it
			// carries the id of one the client is waiting on: request ids are
			// per sender. This transport serves no other server request.
			c.logger.Debug("Refusing a server request: the SSE transport serves none", "method", envelope.Method.Name)
			c.answer(NewJSONRPCErrorResponse(id, mcp.METHOD_NOT_FOUND, "Method not found: the SSE transport serves no server requests", nil))
			return
		case !readableID:
			// A response without an id, an error to a request whose id the
			// server could not read, answers no request this shared stream
			// can name.
			c.logger.Error("Dropping a response without a readable id", "id", string(envelope.ID))
			return
		}

		// A JSON-RPC 2.0 response is built from the envelope; one with any
		// other jsonrpc member is decoded whole, as it always was, so its
		// member is kept, or the response dropped when it is not a string.
		baseMessage := JSONRPCResponse{JSONRPC: mcp.JSONRPC_VERSION, ID: id, Result: envelope.Result}
		if !envelope.JSONRPC.is20 {
			baseMessage = JSONRPCResponse{}
			if err := json.Unmarshal(encoded, &baseMessage); err != nil {
				c.logger.Error("Error unmarshaling response", "err", err)
				return
			}
		} else if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
			var details mcp.JSONRPCErrorDetails
			if err := json.Unmarshal(envelope.Error, &details); err != nil {
				c.logger.Error("Error unmarshaling response", "err", err)
				return
			}
			baseMessage.Error = &details
		}

		idKey := responseKeyOf(id)

		// Hold the write lock while sending: Close() also takes the write
		// lock to close pending response channels, so the send can never
		// race a close and panic with "send on closed channel".
		c.mu.Lock()
		ch, exists := c.responses[idKey]
		if exists {
			ch <- &baseMessage
			delete(c.responses, idKey)
		}
		c.mu.Unlock()
	}
}

// answer sends the client's response to a server request. It runs on the
// stream's reader, as the Streamable HTTP transport's answers do, bounded by
// the time a server request may take there, and Close abandons it.
func (c *SSE) answer(response *JSONRPCResponse) {
	body, err := json.Marshal(response)
	if err != nil {
		c.logger.Error("Error marshaling response", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(c.answers, 30*time.Second)
	defer cancel()
	if err := c.post(ctx, body, "response"); err != nil {
		c.logger.Error("Error answering a server request", "err", err)
	}
}
