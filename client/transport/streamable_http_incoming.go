package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

func (c *StreamableHTTP) listenForever(ctx context.Context) {
	c.logger.Info("listening to server forever")
	// openedOn is the last session the stream opened on.
	openedOn := ""
	for {
		// No per-connection timeout - persistent SSE connections are meant to
		// stay open indefinitely:
		// 1. The SSE connection itself will detect disconnections via the underlying HTTP transport
		// 2. Network-level timeouts and keep-alives handle connection health
		// 3. Context cancellation (user-initiated or system shutdown) provides clean shutdown
		// Each connection has its own context, which a new session cancels so
		// that the stream follows it. While the session has ended, listenOn
		// waits for the caller to start a new one.
		connCtx, cancelConn := context.WithCancel(ctx)
		session, ok := c.listenOn(ctx, cancelConn)
		if !ok {
			cancelConn()
			return
		}
		opened, err := c.createGETConnectionToServer(connCtx)
		c.listenDone()
		moved := connCtx.Err() != nil
		cancelConn()
		if opened {
			openedOn = session
		}
		if errors.Is(err, ErrGetMethodNotAllowed) {
			// server does not support listening
			c.logger.Error("server does not support listening")
			return
		}
		if errors.Is(err, ErrSessionTerminated) {
			// The server answered 404, or the session ended before the GET went
			// out. If the stream worked on the session, the server has forgotten
			// it (restarted, or the session expired): listen again once the
			// caller starts a new session. If the stream never opened on it, and
			// the session is still current, the server doesn't offer the stream
			// (it should answer 405), so stop.
			if session == openedOn {
				c.endSession(session)
			}
			if current, ended := c.currentSession(); !ended && current == session {
				c.logger.Error("session terminated, stopping listener", "err", err)
				return
			}
			c.logger.Info("session terminated, listening again after re-initialization", "err", err)
			continue
		}

		select {
		case <-ctx.Done():
			return
		default:
		}
		if moved {
			// A new session started.
			continue
		}

		if err != nil {
			c.logger.Error("failed to listen to server. retry in 1 second", "err", err)
		}

		// Use context-aware sleep
		select {
		case <-time.After(retryInterval):
		case <-ctx.Done():
			return
		}
	}
}

var (
	// ErrSessionTerminated indicates the server no longer recognizes the current session.
	ErrSessionTerminated   = errors.New("session terminated (404). need to re-initialize")
	ErrGetMethodNotAllowed = errors.New("GET method not allowed")
	ErrUnauthorized        = errors.New("unauthorized (401)")
	ErrLegacySSEServer     = errors.New("server returned 4xx for initialize POST, likely a legacy SSE server")

	retryInterval = 1 * time.Second // a variable is convenient for testing
)

// createGETConnectionToServer opens the GET stream and reads it until it
// ends. opened reports whether the server accepted the stream.
func (c *StreamableHTTP) createGETConnectionToServer(ctx context.Context) (opened bool, err error) {
	resp, err := c.sendHTTP(ctx, http.MethodGet, nil, "text/event-stream", nil, false)
	if err != nil {
		return false, fmt.Errorf("failed to send request: %w", err)
	}
	// Cancel the context before closing the body to prevent HTTP/2 drain hangs,
	// matching the pattern used in SendRequest and SendNotification.
	defer func() { resp.Body.Close() }()

	// Check if we got an error response
	if resp.StatusCode == http.StatusMethodNotAllowed {
		return false, ErrGetMethodNotAllowed
	}

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("request failed with status %d: %s", resp.StatusCode, body)
	}

	// handle SSE response. Parse the media type to tolerate parameters such as
	// "text/event-stream; charset=utf-8" (same handling as SendRequest).
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType != "text/event-stream" {
		return false, fmt.Errorf("unexpected content type: %s", resp.Header.Get("Content-Type"))
	}

	// When ignoreResponse is true, the function will never return expect context is done.
	// NOTICE: Due to the ambiguity of the specification, other SDKs may use the GET connection to transfer the response
	// messages. To be more compatible, we should handle this response, however, as the transport layer is message-based,
	// currently, there is no convenient way to handle this response.
	// So we ignore the response here. It's not a bug, but may be not compatible with other SDKs.
	_, err = c.handleSSEResponse(ctx, resp.Body, nil)
	if err != nil {
		return true, fmt.Errorf("failed to handle SSE response: %w", err)
	}

	return true, nil
}

// handleIncomingRequest processes requests from the server (like sampling requests)
func (c *StreamableHTTP) handleIncomingRequest(ctx context.Context, request JSONRPCRequest) {
	c.requestMu.RLock()
	handler := c.requestHandler
	c.requestMu.RUnlock()

	if handler == nil {
		c.logger.Error("received request from server but no handler set", "method", request.Method)
		// Send method not found error
		errorResponse := NewJSONRPCErrorResponse(
			request.ID,
			mcp.METHOD_NOT_FOUND,
			fmt.Sprintf("no handler configured for method: %s", request.Method),
			nil,
		)
		c.sendResponseToServer(ctx, errorResponse)
		return
	}

	// Handle the request in a goroutine to avoid blocking the SSE reader
	go func() {
		defer func() {
			if r := recover(); r != nil {
				c.logger.Error("panic handling server request", "method", request.Method, "panic", r)
				// Attempt to send an internal error response so the server doesn't hang.
				// Use a nested recover to prevent sendResponseToServer from propagating
				// a secondary panic (e.g., nil serverURL during shutdown).
				func() {
					defer func() {
						if r2 := recover(); r2 != nil {
							c.logger.Error("failed to send error response after panic", "err", r2)
						}
					}()
					errorResponse := NewJSONRPCErrorResponse(
						request.ID,
						mcp.INTERNAL_ERROR,
						fmt.Sprintf("internal error: panic in request handler: %v", r),
						nil,
					)
					c.sendResponseToServer(ctx, errorResponse)
				}()
			}
		}()
		// Create a new context with timeout for request handling, respecting parent context
		requestCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		response, err := handler(requestCtx, request)
		if err != nil {
			c.logger.Error("error handling request", "method", request.Method, "err", err)

			// Determine appropriate JSON-RPC error code based on error type
			var errorCode int
			var errorMessage string

			// Check for specific sampling-related errors
			if errors.Is(err, context.Canceled) {
				errorCode = mcp.REQUEST_INTERRUPTED
				errorMessage = "request was cancelled"
			} else if errors.Is(err, context.DeadlineExceeded) {
				errorCode = mcp.REQUEST_INTERRUPTED
				errorMessage = "request timed out"
			} else {
				// Generic error cases
				errorCode = handlerErrorCode(err)
				switch request.Method {
				case string(mcp.MethodSamplingCreateMessage):
					errorMessage = fmt.Sprintf("sampling request failed: %v", err)
				default:
					errorMessage = err.Error()
				}
			}

			// Send error response
			errorResponse := NewJSONRPCErrorResponse(request.ID, errorCode, errorMessage, nil)
			c.sendResponseToServer(requestCtx, errorResponse)
			return
		}

		if response != nil {
			c.sendResponseToServer(requestCtx, response)
		}
	}()
}

// sendResponseToServer sends a response back to the server via HTTP POST
func (c *StreamableHTTP) sendResponseToServer(ctx context.Context, response *JSONRPCResponse) {
	if response == nil {
		c.logger.Error("cannot send nil response to server")
		return
	}

	responseBody, err := json.Marshal(response)
	if err != nil {
		c.logger.Error("failed to marshal response", "err", err)
		return
	}

	ctx, cancel := c.contextAwareOfClientClose(ctx)

	resp, err := c.sendHTTP(ctx, http.MethodPost, bytes.NewReader(responseBody), "application/json, text/event-stream", nil, false)
	if err != nil {
		cancel()
		c.logger.Error("failed to send response to server", "err", err)
		return
	}
	defer func() { cancel(); resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		c.logger.Error("server rejected response", "status", resp.StatusCode, "body_len", len(body))
	}
}
