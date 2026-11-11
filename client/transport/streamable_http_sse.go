package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/pbednarcik/flexmcp/internal/jsonrpc"
	"github.com/pbednarcik/flexmcp/mcp"
)

// handleSSEResponse processes an SSE stream for a specific request.
// It returns the final result for the request once received, or an error.
// want is the id of the request the stream answers; the response to it, or
// an error with a null id, ends the stream. A nil want is a listening stream,
// which returns no response.
func (c *StreamableHTTP) handleSSEResponse(ctx context.Context, reader io.ReadCloser, want *mcp.RequestId) (*JSONRPCResponse, error) {
	ignoreResponse := want == nil
	// Create a channel for this specific request
	responseChan := make(chan *JSONRPCResponse, 1)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// end is why the stream ended without the response: failure, that it
	// broke off or couldn't be resumed, and brokenOff, whether it broke
	// off. It is written before responseChan is closed and read after, so
	// the close orders the two; one variable, so that the reader's closure
	// keeps one.
	var end struct {
		failure   error
		brokenOff bool
	}

	// Start a goroutine to process the SSE stream
	go func() {
		defer func() {
			if r := recover(); r != nil {
				c.logger.Error("panic in SSE stream reader", "panic", r)
			}
		}()
		// Ensure this goroutine respects the context
		defer close(responseChan)

		var cursor sseCursor
		delivered := false
		handle := func(event string, data []byte) {
			// data is the reader's buffer, valid until handle returns: every
			// decode below reads it, and none keeps it.
			var envelope struct {
				JSONRPC jsonrpcVersion  `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Method  jsonrpc.Method  `json:"method"`
			}
			if err := json.Unmarshal(data, &envelope); err != nil {
				c.logger.Info("failed to unmarshal message (non-fatal)", "err", err, "message_len", len(data))
				return
			}
			var id mcp.RequestId
			readableID := len(envelope.ID) > 0 && json.Unmarshal(envelope.ID, &id) == nil && !id.IsNil()

			// A message with a method member is a request or a notification,
			// whatever its value; only one without is a response.
			switch {
			case envelope.Method.Present && !envelope.Method.IsString:
				answerID := mcp.NewRequestId(nil)
				if readableID {
					answerID = id
				}
				c.sendResponseToServer(ctx, NewJSONRPCErrorResponse(answerID, mcp.INVALID_REQUEST, "Invalid request: method must be a string", nil))
			case envelope.Method.Present && len(envelope.ID) == 0:
				var notification mcp.JSONRPCNotification
				if err := json.Unmarshal(data, &notification); err != nil {
					c.logger.Error("failed to unmarshal notification", "err", err)
					return
				}
				// The handler runs outside the lock, so one that sets the
				// handler does not deadlock the stream.
				c.notifyMu.RLock()
				handler := c.notificationHandler
				c.notifyMu.RUnlock()
				if handler != nil {
					handler(notification)
				}
			case envelope.Method.Present && !readableID:
				// A request id is a string or an integer and never null: a
				// server request whose id cannot be read is answered as invalid.
				c.sendResponseToServer(ctx, NewJSONRPCErrorResponse(mcp.NewRequestId(nil), mcp.INVALID_REQUEST, "Invalid request id", nil))
			case envelope.Method.Present && !envelope.JSONRPC.is20:
				c.sendResponseToServer(ctx, invalidVersionResponse(id))
			case envelope.Method.Present:
				var request JSONRPCRequest
				if err := json.Unmarshal(data, &request); err != nil {
					c.logger.Error("failed to unmarshal server request", "err", err)
					return
				}
				c.handleIncomingRequest(ctx, request)
			default:
				var message JSONRPCResponse
				if err := json.Unmarshal(data, &message); err != nil {
					c.logger.Info("failed to unmarshal response (non-fatal)", "err", err, "message_len", len(data))
					return
				}
				// The stream answers one request: its response, or an error with
				// a null id, the server saying it could not read that id.
				answers := !ignoreResponse && !delivered &&
					((readableID && id.String() == want.String()) || (message.ID.IsNil() && message.Error != nil))
				if !answers {
					c.logger.Error("Dropping a response that answers no request on this stream", "id", string(envelope.ID))
					return
				}
				delivered = true
				responseChan <- &message
			}
		}
		ended := c.readSSEWithCursor(ctx, reader, &cursor, handle)

		// Before protocol version 2026-07-28, a server may end the stream
		// before the response, once it has sent an event ID, and the client
		// polls it by reconnecting (SEP-1699). A stream that breaks off
		// fails the request with its read error, as does one an event over
		// the bound ended: a resume would have the server replay the event.
		for ended && !ignoreResponse && !delivered && ctx.Err() == nil && cursor.lastEventID != "" && !c.isModern() {
			var resumeErr error
			ended, resumeErr = c.resumeSSE(ctx, &cursor, handle)
			if resumeErr != nil {
				end.failure = fmt.Errorf("failed to resume the SSE stream for the response: %w", resumeErr)
				return
			}
		}
		// A listening stream answers no request: why it broke off, which
		// brokeOff has logged, is not reported again.
		if cursor.err != nil && !ignoreResponse {
			end.failure = fmt.Errorf("failed to read the SSE stream for the response: %w", cursor.err)
		}
		end.brokenOff = cursor.err != nil
	}()

	// Wait for the response or context cancellation
	select {
	case response := <-responseChan:
		if response == nil {
			if end.failure != nil || end.brokenOff {
				return nil, end.failure
			}
			return nil, errors.New("unexpected nil response")
		}
		return response, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// defaultSSEReconnectDelay is how long a client waits before resuming an SSE
// stream when the server sent no retry field. minSSEReconnectDelay is the
// shortest wait it takes from one, so that a server asking for no wait at
// all isn't polled in a tight loop.
var (
	defaultSSEReconnectDelay = 1 * time.Second
	minSSEReconnectDelay     = 10 * time.Millisecond
)

// resumeSSE waits the reconnection time the server asked for, reopens the
// stream with a GET carrying Last-Event-ID, the 2025-11-25 way of resuming a
// stream the server ended before the response, and reads it. It reports
// whether the server ended this stream too.
func (c *StreamableHTTP) resumeSSE(ctx context.Context, cursor *sseCursor, handler func(event string, data []byte)) (bool, error) {
	delay := defaultSSEReconnectDelay
	if cursor.hasRetry {
		delay = max(cursor.retry, minSSEReconnectDelay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		return false, ctx.Err()
	}

	// The connection gets a context of its own, so that it and the goroutine
	// readSSEWithCursor starts for it are released as soon as it ends, not
	// when the request does.
	ctx, cancel := context.WithCancel(ctx)
	header := make(http.Header)
	header.Set("Last-Event-ID", cursor.lastEventID)
	resp, err := c.sendHTTP(ctx, http.MethodGet, nil, "text/event-stream", header, false)
	if err != nil {
		releaseResponse(cancel, resp)
		return false, err
	}
	// readSSEWithCursor reads the body on this goroutine and is done with it
	// when it returns, so the body is released as any other.
	defer releaseResponse(cancel, resp)
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType != "text/event-stream" {
		return false, fmt.Errorf("unexpected content type %q", resp.Header.Get("Content-Type"))
	}
	return c.readSSEWithCursor(ctx, resp.Body, cursor, handler), nil
}

// readSSE reads the SSE stream(reader) and calls the handler for each event and data pair.
// It will end when the reader is closed (or the context is done). The data
// lives in the reader's buffer and is valid only until the handler returns.
//
// A background goroutine closes the reader when ctx is cancelled, which unblocks
// any read in progress. This is necessary because the read is blocking I/O that
// does not respect context cancellation on its own.
func (c *StreamableHTTP) readSSE(ctx context.Context, reader io.ReadCloser, handler func(event string, data []byte)) {
	c.readSSEWithCursor(ctx, reader, nil, handler)
}

// sseCursor holds what a client needs to resume an SSE stream (SEP-1699):
// the ID of the last event dispatched and the reconnection time the server
// asked for with the retry field, if any. err is why the stream last broke
// off, if it did: a read error, or ErrMessageTooLarge.
type sseCursor struct {
	lastEventID string
	retry       time.Duration
	hasRetry    bool
	err         error
}

// readSSEWithCursor is readSSE that also records the id and retry fields
// in cursor, when it isn't nil, and reports whether the server ended the
// stream, as opposed to it breaking off or the context ending. As in the SSE
// spec, an event's id counts once the event is dispatched, even an event
// with no data, which is how a server primes the client to reconnect.
func (c *StreamableHTTP) readSSEWithCursor(ctx context.Context, reader io.ReadCloser, cursor *sseCursor, handler func(event string, data []byte)) bool {
	// Close the reader when context is cancelled to interrupt blocking reads.
	// This ensures a read returns immediately with an error instead of
	// blocking indefinitely when the SSE stream is open but idle.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				c.logger.Error("panic closing SSE reader", "panic", r)
			}
		}()
		<-ctx.Done()
		reader.Close()
	}()

	br := bufio.NewReader(reader)
	// The line and the event's data are read into buffers kept from one
	// event to the next, as the SSE transport's reader does; one a message
	// grew past maxRetainedLine is let go. The event's name and id are made
	// strings only when the stream names them. A line over lineLimit, or
	// data that would grow past the message bound, breaks the stream off
	// before it is held.
	var line, data []byte
	lineLimit := sseLineLimit(c.maxMessageBytes)
	var event, id string
	var hasID bool
	commitID := func() {
		if cursor != nil && hasID {
			cursor.lastEventID = id
		}
		hasID = false
	}

	for {
		if cap(line) > maxRetainedLine {
			line = nil
		}
		var err error
		line, err = readLineMax(br, line, lineLimit)
		if err != nil {
			// Context was cancelled — reader was closed by the goroutine above.
			if ctx.Err() != nil {
				return false
			}
			if errors.Is(err, io.EOF) {
				// An event the stream ended before its blank line is
				// unfinished, and SSE discards it, id and all: a resume from
				// the event before delivers it whole.
				return true
			}
			return c.brokeOff(cursor, err)
		}

		if len(line) == 0 {
			// Empty line means end of event
			commitID()
			if len(data) > 0 {
				if event == "" {
					event = "message"
				}
				handler(event, data)
				data = data[:0]
				if cap(data) > maxRetainedLine {
					data = nil
				}
			}
			// The event type resets on every dispatch, even one with no
			// data, so it cannot leak into the next event.
			event = ""
			continue
		}

		if after, ok := bytes.CutPrefix(line, []byte("event:")); ok {
			// sseEventName makes no string for "message", the name an event
			// is handled by when it names none.
			event = sseEventName(bytes.TrimSpace(after))
		} else if after, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			if !sseDataFits(data, after, c.maxMessageBytes) {
				return c.brokeOff(cursor, ErrMessageTooLarge)
			}
			if len(data) == 0 {
				// The event's first data line is moved to the front of the
				// line's buffer, which becomes the data's, and the data's
				// buffer reads the next line: the value is not copied into a
				// second buffer.
				n := copy(line, bytes.TrimPrefix(after, []byte(" ")))
				data, line = line[:n], data[:0]
			} else {
				data = appendSSEDataBytes(data, after)
			}
		} else if after, ok := bytes.CutPrefix(line, []byte("id:")); ok {
			// The SSE spec ignores an id that contains a NUL character.
			if value := bytes.TrimPrefix(after, []byte(" ")); bytes.IndexByte(value, 0) < 0 {
				id, hasID = string(value), true
			}
		} else if after, ok := bytes.CutPrefix(line, []byte("retry:")); ok && cursor != nil {
			// Only ASCII digits are a valid retry value.
			value := bytes.TrimPrefix(after, []byte(" "))
			if ms, err := strconv.ParseUint(string(value), 10, 31); err == nil && len(value) > 0 && len(bytes.Trim(value, "0123456789")) == 0 {
				cursor.retry = time.Duration(ms) * time.Millisecond
				cursor.hasRetry = true
			}
		}
	}
}

// brokeOff records err as why the stream broke off, in cursor when there is
// one, and reports that the server did not end the stream.
func (c *StreamableHTTP) brokeOff(cursor *sseCursor, err error) bool {
	c.logger.Error("SSE stream error", "err", err)
	if cursor != nil {
		cursor.err = err
	}
	return false
}
