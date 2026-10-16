package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sync"
	"time"
	"unicode"

	"github.com/mark3labs/mcp-go/internal/jsonrpc"
	"github.com/mark3labs/mcp-go/mcp"
)

func (s *StreamableHTTPServer) handlePost(w HTTPResponseWriter, r *HTTPRequest) {
	// post request carry request/notification message

	// Check content type
	contentType := r.header().Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "application/json" {
		writeHTTPError(w, "Invalid content type: must be 'application/json'", http.StatusBadRequest)
		return
	}

	// Body has already been buffered by the caller (ServeHTTP or Handle).
	rawData := r.Body
	x := new(postExchange)
	jsonMessage := &x.message
	// The message is read once, here, for the transport's checks and the
	// core's, which takes the read from the request's session. Should it not
	// read (params or _meta not an object, a member of the wrong type), the
	// envelope alone is read and the era and the core read the body on their
	// own, as they always did.
	envelope := &x.envelope
	readErr := readEnvelope(rawData, envelope)
	if readErr == nil {
		jsonMessage.ID, jsonMessage.Result, jsonMessage.Error, jsonMessage.Method =
			envelope.RawID, envelope.Result, envelope.Error, envelope.Method
	} else {
		envelope = nil
		readErr = json.Unmarshal(rawData, jsonMessage)
	}
	if readErr != nil {
		if json.Valid(rawData) {
			id, _ := requestIDValue(jsonMessage.ID)
			s.writeJSONRPCError(w, id, mcp.INVALID_REQUEST, "request body is not a JSON-RPC message")
			return
		}
		s.writeJSONRPCError(w, nil, mcp.PARSE_ERROR, "request body is not valid json")
		return
	}

	method := jsonrpc.MethodOf(string(jsonMessage.Method), rawData)
	isInitializeRequest := jsonMessage.Method == mcp.MethodInitialize

	requestID, validID := requestIDValue(jsonMessage.ID)
	isResponse := !method.Present && (len(jsonMessage.Result) > 0 || len(jsonMessage.Error) > 0)
	if !validID && !isResponse {
		s.writeJSONRPCError(w, nil, mcp.INVALID_REQUEST, "invalid request id")
		return
	}

	// Decide which protocol era this message belongs to. Both eras run through
	// the same stateless path below; the era only chooses the version check
	// here, the modern header validation, and the HTTP status of an error.
	var era requestEra
	if envelope != nil {
		era = eraOf(r.header().Get(mcp.HeaderProtocolVersion), envelope.Params.Meta.ProtocolVersion)
	} else {
		era = detectRequestEra(r.header(), rawData)
	}

	// On 2026-07-28 the server sends no requests, so a response from the client
	// answers none: it is input the server cannot accept (rule 4), refused with
	// an error that names no request.
	if era.modern && isResponse {
		s.writeJSONRPCError(w, nil, mcp.INVALID_REQUEST, "a client sends no responses on this protocol revision")
		return
	}
	// A message with no method that is no response is an invalid request.
	// Earlier revisions still take an id alone as an empty response, the way a
	// lax client answers a ping.
	if !method.Present && !isResponse && (len(jsonMessage.ID) == 0 || era.modern) {
		s.writeJSONRPCError(w, requestID, mcp.INVALID_REQUEST, "Invalid request: no method")
		return
	}
	// A method that is not a string makes no request, nor a response.
	if method.Present && !method.IsString {
		s.writeJSONRPCError(w, requestID, mcp.INVALID_REQUEST, "Invalid request: method must be a string")
		return
	}
	// A notification is never answered, so one the server cannot read is
	// refused here, as input it cannot accept (rule 4), not accepted with 202.
	if method.Present && len(jsonMessage.ID) == 0 && json.Unmarshal(rawData, new(mcp.JSONRPCNotification)) != nil {
		s.writeJSONRPCError(w, nil, mcp.INVALID_REQUEST, "notification could not be read")
		return
	}

	// The header is only required after initialize, which negotiates the
	// version itself. Responses posted by the client are checked as well.
	if !era.modern && !isInitializeRequest && s.rejectUnsupportedProtocolVersion(w, era.headerVersion, requestID) {
		return
	}

	// A client's answer to a server ping, or any empty response, is accepted
	// without being dispatched.
	isEmptyResponse := !method.Present && jsonMessage.ID != nil &&
		(isJSONEmpty(jsonMessage.Result) && isJSONEmpty(jsonMessage.Error))
	isPingResponse := !method.Present && jsonMessage.ID != nil &&
		isExplicitEmptyObject(jsonMessage.Result) && len(bytes.TrimSpace(jsonMessage.Error)) == 0

	if isPingResponse {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if isEmptyResponse {
		// Per MCP spec (Streamable HTTP transport, rule 4): the server MUST return
		// 202 Accepted for any accepted JSON-RPC response or notification, regardless
		// of whether result is {} or omitted. HTTP 200 with no body has no defined
		// meaning in this transport for POST requests carrying responses.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Every request runs on its own ephemeral session: the server keeps none
	// in either era, so a legacy client's Mcp-Session-Id is ignored and its
	// initialize mints none.
	listening := false
	if era.modern {
		if !s.validateModernRequest(w, r, era, requestID) {
			return
		}
		// A subscriptions/listen stream is where a modern client receives
		// the notifications the server broadcasts, such as
		// notifications/tools/list_changed.
		listening = jsonMessage.Method == mcp.MethodSubscriptionsListen && requestID != nil
	}
	session := &x.session
	// The core takes the transport's read only of a body the transport read
	// itself: a body handed to Handle is its caller's, who may change it
	// while the request is handled.
	if r.original != nil {
		session.requestBody, session.request = rawData, envelope
	}

	// Broadcasts only reach sessions the server knows of, and a modern
	// request's session is known nowhere else. A listen stream's session is
	// made reachable while the request runs; it receives nothing until
	// subscriptions/listen records the types the client asked for.
	if listening {
		session.serveSubscriptionStream()
		s.server.registerListenSession(session)
		defer s.server.unregisterListenSession(session)
	}

	// Set the client context before handling the message
	x.base = postContext{Context: r.ctx(), session: session, versions: s.supportedVersions}
	var ctx context.Context = &x.base
	if s.contextFunc != nil {
		ctx = s.contextFunc(ctx, r.asHTTPRequest())
	}
	// The header goes on top of what the context func returned, so the
	// checks that read it run whatever context the func made.
	x.header = headerContext{Context: ctx, header: r.header()}
	ctx = &x.header

	// A listen stream ends when Shutdown says so; see Shutdown.
	if listening && s.shutdown != nil {
		var stop context.CancelFunc
		ctx, stop = context.WithCancel(ctx)
		defer stop()
		defer context.AfterFunc(s.shutdown, stop)()
	}

	// SSE upgrades require a streaming-capable response writer; if the
	// underlying transport can't stream, we still attempt the request but
	// any notifications will be buffered and flushed at the end as a single
	// application/json response (the upgrade simply won't fire).
	canStream := w.CanStream()

	// The forwarder starts with the first notification the request sends:
	// most requests send none, and a goroutine per request costs a thread
	// wakeup each.
	x.server, x.w, x.ctx, x.canStream, x.listening, x.requestID = s, w, ctx, canStream, listening, requestID
	if !serverHasWriteTimeout(r.ctx()) {
		x.writeTimeout = s.writeTimeout
	}
	x.keepAlive = s.keepAlive
	// A handler may keep its context, and with it the exchange, after the
	// request; the response writer is let go when the request is done.
	defer func() { x.w = nil }()
	session.forwardWith(x)

	response := s.server.HandleMessage(ctx, rawData)
	if listening {
		// Stop broadcasts before draining what is already queued.
		s.server.unregisterListenSession(session)
	}

	// Stop the forwarder, if one started, and wait for it before touching the
	// response: it may hold a notification it has not written yet, and every
	// notification the handler sent must reach the client, in order, ahead of
	// the response. From here on this goroutine is the only writer.
	if session.stopForwarding() {
		close(x.done)
		<-x.forwarderExited
	}

	if response == nil {
		if !x.upgraded {
			w.WriteHeader(http.StatusAccepted)
		}
		return
	}

	x.mu.Lock()

	notifications := session.pending()
drainLoop:
	for {
		select {
		case nt := <-notifications:
			if !canStream {
				continue
			}
			if listening {
				nt = tagSubscription(nt, requestID)
			}
			x.deliver(func() error {
				x.upgrade()
				if err := writeSSEEvent(w, nt); err != nil {
					return err
				}
				return flush(w)
			})
		default:
			break drainLoop
		}
	}

	x.mu.Unlock()
	if ctx.Err() != nil {
		// The connection is gone; there is nobody to answer.
		return
	}
	// If client-server communication already upgraded to SSE stream
	// Also check upgradedHeader: a notification during HandleMessage processing
	// may have already written SSE headers on this response, so we must continue
	// in SSE mode to avoid writing JSON on top of SSE data.
	x.mu.Lock()
	defer x.mu.Unlock()
	if (session.upgradeToSSE.Load() && canStream) || x.upgraded {
		x.deliver(func() error {
			x.upgrade()
			if err := writeSSEEvent(w, response); err != nil {
				return err
			}
			return flush(w)
		})
	} else {
		w.Header().Set("Content-Type", "application/json")
		// Protocol version 2026-07-28 maps JSON-RPC error codes onto HTTP
		// status codes, so that intermediaries can act on failures without
		// parsing the body.
		status := http.StatusOK
		if era.modern {
			if errResponse, ok := response.(mcp.JSONRPCError); ok {
				status = httpStatusForJSONRPCError(errResponse.Error.Code)
			}
		}
		x.deliver(func() error {
			w.WriteHeader(status)
			return json.NewEncoder(w).Encode(response)
		})
	}
}

// postExchange is what handling one POST needs, made in one allocation: the
// read of its body, its session, its context and the response its
// notifications ride on.
type postExchange struct {
	// The envelope: a client's response carries an id and no method.
	message struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  json.RawMessage `json:"error,omitempty"`
		Method mcp.MCPMethod   `json:"method"`
	}
	envelope requestEnvelope
	session  streamableHttpSession
	base     postContext     // the session and the versions
	header   headerContext   // the header, on top of what the context func returned
	ctx      context.Context // header, and everything below it

	server    *StreamableHTTPServer
	w         HTTPResponseWriter
	canStream bool
	listening bool
	requestID any

	mu       sync.Mutex // held while a message is written to the response
	upgraded bool       // the response is committed as SSE
	// dead, under mu, says a write to the response failed: the client is
	// gone, and nothing more is written for the request.
	dead         bool
	writeTimeout time.Duration
	keepAlive    time.Duration // a listen stream's keep-alive interval; zero is none

	// done stops the forwarder; forwarderExited lets the response path wait
	// until it can no longer be holding an undelivered notification.
	done, forwarderExited chan struct{}
}

// upgrade commits the response as SSE once; every later message rides it.
func (x *postExchange) upgrade() {
	if !x.upgraded {
		startSSEResponse(x.w)
		x.upgraded = true
	}
}

// forward starts the goroutine that writes the request's notifications to the
// response while the request is handled.
func (x *postExchange) forward(notifications <-chan mcp.JSONRPCNotification) {
	x.done = make(chan struct{})
	x.forwarderExited = make(chan struct{})
	go func() {
		defer close(x.forwarderExited)
		defer func() {
			if r := recover(); r != nil {
				x.server.logger.Error("panic in notification forwarder", "panic", r)
			}
		}()
		// A listen stream carries a keep-alive comment every keepAlive.
		var keepAlive <-chan time.Time
		if x.listening && x.keepAlive > 0 {
			ticker := time.NewTicker(x.keepAlive)
			defer ticker.Stop()
			keepAlive = ticker.C
		}
		for {
			select {
			case nt := <-notifications:
				if x.listening {
					nt = tagSubscription(nt, x.requestID)
				}
				x.write(nt)
			case <-keepAlive:
				x.comment()
			case <-x.done:
				return
			case <-x.ctx.Done():
				return
			}
		}
	}()
}

// write writes one notification to the response, as SSE.
func (x *postExchange) write(nt mcp.JSONRPCNotification) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.canStream {
		// Without streaming we can't deliver notifications mid-flight;
		// they will be dropped on the floor here. The final response
		// path remains the buffered JSON reply below.
		return
	}
	x.deliver(func() error {
		x.upgrade()
		if err := writeSSEEvent(x.w, nt); err != nil {
			return err
		}
		return flush(x.w)
	})
}

// comment writes an SSE comment line, a keep-alive, to a stream already
// committed as SSE; before that there is nothing to keep alive.
func (x *postExchange) comment() {
	x.mu.Lock()
	defer x.mu.Unlock()
	if !x.canStream || !x.upgraded {
		return
	}
	x.deliver(func() error {
		if _, err := io.WriteString(x.w, ": keep-alive\n\n"); err != nil {
			return err
		}
		return flush(x.w)
	})
}

// flush forwards w's buffered bytes to the client, and reports a flush that
// failed when w can tell: an SSE event smaller than net/http's buffer reaches
// the socket only in the flush, so a client that stopped reading, the write
// deadline passing, is seen there.
func flush(w HTTPResponseWriter) error {
	if f, ok := w.(interface{ FlushError() error }); ok {
		return f.FlushError()
	}
	w.Flush()
	return nil
}

// deliver runs write, one write to the response and its flush, under the
// write deadline: a client that stops reading cannot hold the request. A
// write that fails marks the exchange dead: the client is gone, nothing more
// is written for the request, and whoever waits on a write moves on. mu is
// held.
func (x *postExchange) deliver(write func() error) {
	if x.dead {
		return
	}
	deadliner, bounded := x.w.(writeDeadliner)
	bounded = bounded && x.writeTimeout > 0 && deadliner.SetWriteDeadline(time.Now().Add(x.writeTimeout)) == nil
	err := write()
	if bounded {
		_ = deadliner.SetWriteDeadline(time.Time{})
	}
	if err != nil {
		x.dead = true
		// A client that went away is nothing wrong on the server: a warning.
		x.server.logger.Warn("write to the response failed; the request's remaining output is dropped", "err", err)
	}
}

// postContext holds the values the transport gives a request before the
// context func runs - its session and the protocol versions it serves - as
// one context layer. Every other value, and the cancellation, are its
// parent's.
type postContext struct {
	context.Context
	session  ClientSession
	versions any
}

func (c *postContext) Value(key any) any {
	switch key {
	case clientSessionKey{}:
		return c.session
	case supportedProtocolVersionsKey{}:
		return c.versions
	}
	return c.Context.Value(key)
}

// headerContext holds a request's HTTP header; every other value, and the
// cancellation, are its parent's.
type headerContext struct {
	context.Context
	header http.Header
}

func (c *headerContext) Value(key any) any {
	if key == requestHeader {
		return c.header
	}
	return c.Context.Value(key)
}

var (
	sseEventData = []byte("event: message\ndata: ")
	sseEventEnd  = []byte("\n\n")
)

func writeSSEEvent(w io.Writer, data any) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal data: %w", err)
	}
	// Encoded first, so an event that cannot be encoded writes nothing; the
	// caller holds the response while the parts are written.
	for _, part := range [][]byte{sseEventData, jsonData, sseEventEnd} {
		if _, err := w.Write(part); err != nil {
			return fmt.Errorf("failed to write SSE event: %w", err)
		}
	}
	return nil
}

// startSSEResponse commits the response as an SSE stream: the headers every
// stream carries, including the one that asks proxies not to buffer events
// (Transports.md row 23), then the 200 that sends them.
func startSSEResponse(w HTTPResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// isJSONEmpty reports whether the provided JSON value is "empty":
//   - null
//   - empty object: {}
//   - empty array: []
//
// It also treats nil/whitespace-only input as empty.
// It does NOT treat 0, false, "" or non-empty composites as empty.
func isJSONEmpty(data json.RawMessage) bool {
	if len(data) == 0 {
		return true
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return true
	}

	switch trimmed[0] {
	case '{':
		if len(trimmed) == 2 && trimmed[1] == '}' {
			return true
		}
		for i := 1; i < len(trimmed); i++ {
			if !unicode.IsSpace(rune(trimmed[i])) {
				return trimmed[i] == '}'
			}
		}
	case '[':
		if len(trimmed) == 2 && trimmed[1] == ']' {
			return true
		}
		for i := 1; i < len(trimmed); i++ {
			if !unicode.IsSpace(rune(trimmed[i])) {
				return trimmed[i] == ']'
			}
		}

	case '"': // treat "" as not empty
		return false

	case 'n': // null
		return len(trimmed) == 4 &&
			trimmed[1] == 'u' &&
			trimmed[2] == 'l' &&
			trimmed[3] == 'l'
	}
	return false
}

// isExplicitEmptyObject reports whether data is a JSON object literal with no fields.
func isExplicitEmptyObject(data json.RawMessage) bool {
	if len(data) == 0 {
		return false
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		return false
	}

	return len(obj) == 0
}
