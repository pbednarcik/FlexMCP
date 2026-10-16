package transport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/internal/jsonrpc"
	"github.com/mark3labs/mcp-go/mcp"
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

	// resumeErr is why the stream couldn't be resumed. It is written before
	// responseChan is closed and read after, so the close orders the two.
	var resumeErr error

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
		handle := func(event, data string) {
			// Every decode below reads these bytes, and none keeps them.
			encoded := []byte(data)
			var envelope struct {
				JSONRPC jsonrpcVersion  `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Method  jsonrpc.Method  `json:"method"`
			}
			if err := json.Unmarshal(encoded, &envelope); err != nil {
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
				if err := json.Unmarshal(encoded, &notification); err != nil {
					c.logger.Error("failed to unmarshal notification", "err", err)
					return
				}
				c.notifyMu.RLock()
				if c.notificationHandler != nil {
					c.notificationHandler(notification)
				}
				c.notifyMu.RUnlock()
			case envelope.Method.Present && !readableID:
				// A request id is a string or an integer and never null: a
				// server request whose id cannot be read is answered as invalid.
				c.sendResponseToServer(ctx, NewJSONRPCErrorResponse(mcp.NewRequestId(nil), mcp.INVALID_REQUEST, "Invalid request id", nil))
			case envelope.Method.Present && !envelope.JSONRPC.is20:
				c.sendResponseToServer(ctx, invalidVersionResponse(id))
			case envelope.Method.Present:
				var request JSONRPCRequest
				if err := json.Unmarshal(encoded, &request); err != nil {
					c.logger.Error("failed to unmarshal server request", "err", err)
					return
				}
				c.handleIncomingRequest(ctx, request)
			default:
				var message JSONRPCResponse
				if err := json.Unmarshal(encoded, &message); err != nil {
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
		// fails the request, as before.
		for ended && !ignoreResponse && !delivered && ctx.Err() == nil && cursor.lastEventID != "" && !c.isModern() {
			ended, resumeErr = c.resumeSSE(ctx, &cursor, handle)
			if resumeErr != nil {
				return
			}
		}
	}()

	// Wait for the response or context cancellation
	select {
	case response := <-responseChan:
		if response == nil {
			if resumeErr != nil {
				return nil, fmt.Errorf("failed to resume the SSE stream for the response: %w", resumeErr)
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
func (c *StreamableHTTP) resumeSSE(ctx context.Context, cursor *sseCursor, handler func(event, data string)) (bool, error) {
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
		cancel()
		return false, err
	}
	// Cancel before closing the body, as SendRequest does.
	defer func() { cancel(); resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType != "text/event-stream" {
		return false, fmt.Errorf("unexpected content type %q", resp.Header.Get("Content-Type"))
	}
	return c.readSSEWithCursor(ctx, resp.Body, cursor, handler), nil
}

// readSSE reads the SSE stream(reader) and calls the handler for each event and data pair.
// It will end when the reader is closed (or the context is done).
//
// A background goroutine closes the reader when ctx is cancelled, which unblocks
// any in-progress ReadString call. This is necessary because ReadString is blocking
// I/O that does not respect context cancellation on its own.
func (c *StreamableHTTP) readSSE(ctx context.Context, reader io.ReadCloser, handler func(event, data string)) {
	c.readSSEWithCursor(ctx, reader, nil, handler)
}

// sseCursor holds what a client needs to resume an SSE stream (SEP-1699):
// the ID of the last event dispatched and the reconnection time the server
// asked for with the retry field, if any.
type sseCursor struct {
	lastEventID string
	retry       time.Duration
	hasRetry    bool
}

// readSSEWithCursor is readSSE that also records the id and retry fields
// in cursor, when it isn't nil, and reports whether the server ended the
// stream, as opposed to it breaking off or the context ending. As in the SSE
// spec, an event's id counts once the event is dispatched, even an event
// with no data, which is how a server primes the client to reconnect.
func (c *StreamableHTTP) readSSEWithCursor(ctx context.Context, reader io.ReadCloser, cursor *sseCursor, handler func(event, data string)) bool {
	// Close the reader when context is cancelled to interrupt blocking reads.
	// This ensures ReadString returns immediately with an error instead of
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
	var event, data, id string
	var hasID bool
	commitID := func() {
		if cursor != nil && hasID {
			cursor.lastEventID = id
		}
		hasID = false
	}

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			// Context was cancelled — reader was closed by the goroutine above.
			if ctx.Err() != nil {
				return false
			}
			if err == io.EOF {
				// Process any pending event before exit. Its id isn't
				// recorded, as the event was never finished: resuming from
				// the one before at worst repeats it.
				if data != "" {
					if event == "" {
						event = "message"
					}
					handler(event, data)
				}
				return true
			}
			c.logger.Error("SSE stream error", "err", err)
			return false
		}

		// Remove only newline markers
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			// Empty line means end of event
			commitID()
			if data != "" {
				if event == "" {
					event = "message"
				}
				handler(event, data)
				event = ""
				data = ""
			}
			continue
		}

		if eventStr, ok := strings.CutPrefix(line, "event:"); ok {
			event = strings.TrimSpace(eventStr)
		} else if dataStr, ok := strings.CutPrefix(line, "data:"); ok {
			data = appendSSEData(data, dataStr)
		} else if idStr, ok := strings.CutPrefix(line, "id:"); ok {
			// The SSE spec ignores an id that contains a NUL character.
			if value := strings.TrimPrefix(idStr, " "); !strings.ContainsRune(value, 0) {
				id, hasID = value, true
			}
		} else if retryStr, ok := strings.CutPrefix(line, "retry:"); ok && cursor != nil {
			// Only ASCII digits are a valid retry value.
			value := strings.TrimPrefix(retryStr, " ")
			if ms, err := strconv.ParseUint(value, 10, 31); err == nil && value != "" && strings.Trim(value, "0123456789") == "" {
				cursor.retry = time.Duration(ms) * time.Millisecond
				cursor.hasRetry = true
			}
		}
	}
}
