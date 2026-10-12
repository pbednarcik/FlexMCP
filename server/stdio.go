package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/mark3labs/mcp-go/mcp"
)

// StdioContextFunc is a function that takes an existing context and returns
// a potentially modified context.
// This can be used to inject context values from environment variables,
// for example.
type StdioContextFunc func(ctx context.Context) context.Context

// StdioServer wraps a MCPServer and handles stdio communication.
// It provides a simple way to create command-line MCP servers that
// communicate via standard input/output streams using JSON-RPC messages.
type StdioServer struct {
	server      *MCPServer
	errLogger   *log.Logger
	contextFunc StdioContextFunc

	// session is the one client's session: stdio serves a single client for
	// the life of the server, and each server has its own.
	session *stdioSession

	// Thread-safe tool call processing
	toolCallQueue  chan *toolCallWork
	workerWg       sync.WaitGroup
	requestWg      sync.WaitGroup // Tracks in-flight request handlers
	workerPoolSize int
	queueSize      int
	writeMu        sync.Mutex // Protects concurrent writes

	// cancelled remembers which in-flight requests the client cancelled by
	// notification, so that nothing more is sent for them.
	cancelled cancelledRequests
}

// cancelledRequests tracks the requests in flight on a stdio server and which
// of them the client cancelled with notifications/cancelled. After a cancel
// the server sends nothing more for that request, not even the error its
// handler returns on the cancelled context: the client has stopped waiting.
// Keyed by the JSON-RPC id as requestIDValue decodes it, exact for an integer.
type cancelledRequests struct {
	mu       sync.Mutex
	inflight map[any]bool
}

// comparableID reports whether id can key the map: a request id is a string or
// an integer, and anything else is refused before it gets here, but a map key
// that is not comparable would panic.
func comparableID(id any) bool {
	switch id.(type) {
	case int64, json.Number, string:
		return true
	}
	return false
}

// start records id as in flight.
func (c *cancelledRequests) start(id any) {
	if !comparableID(id) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight == nil {
		c.inflight = make(map[any]bool)
	}
	c.inflight[id] = false
}

// cancel marks an in-flight id as cancelled; an id that is not in flight is
// ignored, as an invalid cancellation should be.
func (c *cancelledRequests) cancel(id any) {
	if !comparableID(id) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.inflight[id]; ok {
		c.inflight[id] = true
	}
}

// isCancelled reports whether the client cancelled id while it was in flight.
func (c *cancelledRequests) isCancelled(id any) bool {
	if !comparableID(id) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inflight[id]
}

// finish forgets id and reports whether it was cancelled while in flight.
func (c *cancelledRequests) finish(id any) bool {
	if !comparableID(id) {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cancelled := c.inflight[id]
	delete(c.inflight, id)
	return cancelled
}

// toolCallWork represents a queued tool call request
type toolCallWork struct {
	ctx     context.Context
	id      any // JSON-RPC id, so a recovered panic stays correlatable
	message json.RawMessage
	writer  io.Writer
}

// StdioOption defines a function type for configuring StdioServer
type StdioOption func(*StdioServer)

// WithErrorLogger sets the error logger for the server
func WithErrorLogger(logger *log.Logger) StdioOption {
	return func(s *StdioServer) {
		s.errLogger = logger
	}
}

// WithStdioContextFunc sets a function that will be called to customise the context
// to the server. Note that the stdio server uses the same context for all requests,
// so this function will only be called once per server instance.
func WithStdioContextFunc(fn StdioContextFunc) StdioOption {
	return func(s *StdioServer) {
		s.contextFunc = fn
	}
}

// WithWorkerPoolSize sets the number of workers for processing tool calls
func WithWorkerPoolSize(size int) StdioOption {
	return func(s *StdioServer) {
		const maxWorkerPoolSize = 100
		if size > 0 && size <= maxWorkerPoolSize {
			s.workerPoolSize = size
		} else if size > maxWorkerPoolSize {
			s.errLogger.Printf("Worker pool size %d exceeds maximum (%d), using maximum", size, maxWorkerPoolSize)
			s.workerPoolSize = maxWorkerPoolSize
		}
	}
}

// WithQueueSize sets the size of the tool call queue
func WithQueueSize(size int) StdioOption {
	return func(s *StdioServer) {
		const maxQueueSize = 10000
		if size > 0 && size <= maxQueueSize {
			s.queueSize = size
		} else if size > maxQueueSize {
			s.errLogger.Printf("Queue size %d exceeds maximum (%d), using maximum", size, maxQueueSize)
			s.queueSize = maxQueueSize
		}
	}
}

// stdioSession is the session of a stdio server's one client. Every request
// and every subscriptions/listen stream of that client shares it, so it
// holds the client's streams and tags what it delivers on them.
type stdioSession struct {
	clientInfoStore     // provides Get/SetClientInfo and Get/SetClientCapabilities via method promotion
	subscriptionStreams // the client's subscriptions/listen streams

	notifications chan mcp.JSONRPCNotification
	initialized   atomic.Bool
}

func (s *stdioSession) SessionID() string {
	return "stdio"
}

func (s *stdioSession) NotificationChannel() chan<- mcp.JSONRPCNotification {
	return s.notifications
}

func (s *stdioSession) Initialize() {
	s.initialized.Store(true)
}

func (s *stdioSession) Initialized() bool {
	return s.initialized.Load()
}

var (
	_ ClientSession                  = (*stdioSession)(nil)
	_ SessionWithClientInfo          = (*stdioSession)(nil)
	_ SessionWithSubscriptionStreams = (*stdioSession)(nil)
)

// NewStdioServer creates a new stdio server wrapper around an MCPServer.
// Errors are logged to stderr until SetErrorLogger or WithErrorLogger says
// otherwise; stdout carries nothing but protocol messages.
func NewStdioServer(server *MCPServer) *StdioServer {
	return &StdioServer{
		server:         server,
		errLogger:      log.New(os.Stderr, "", log.LstdFlags),
		session:        &stdioSession{notifications: make(chan mcp.JSONRPCNotification, 100)},
		workerPoolSize: 5,
		queueSize:      100,
	}
}

// SetErrorLogger configures where error messages from the StdioServer are logged.
// The provided logger will receive all error messages generated during server operation.
func (s *StdioServer) SetErrorLogger(logger *log.Logger) {
	s.errLogger = logger
}

// SetContextFunc sets a function that will be called to customise the context
// to the server. Note that the stdio server uses the same context for all requests,
// so this function will only be called once per server instance.
func (s *StdioServer) SetContextFunc(fn StdioContextFunc) {
	s.contextFunc = fn
}

// handleNotifications continuously processes notifications from the session's notification channel
// and writes them to the provided output. It runs until the context is cancelled.
// Any errors encountered while writing notifications are logged but do not stop the handler.
func (s *StdioServer) handleNotifications(ctx context.Context, stdout io.Writer) {
	for {
		select {
		case notification := <-s.session.notifications:
			if err := s.writeResponse(notification, stdout); err != nil {
				s.errLogger.Printf("Error writing notification: %v", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

// inputLine is one line of stdin, or the error that ended the reading.
type inputLine struct {
	text string
	err  error
}

// processInputStream reads stdin line by line and processes each message.
// It returns nil at EOF, the normal end of a stdio server (the client closes
// stdin to shut it down), the context's error when the context ends first,
// or the error that reading or processing hit.
//
// One goroutine reads for the life of the loop, a bounded number of lines
// ahead of processing. A blocking read cannot be interrupted, so when the
// context ends first that goroutine stays in its read until stdin closes;
// the loop itself returns at once.
func (s *StdioServer) processInputStream(ctx context.Context, reader *bufio.Reader, stdout io.Writer) error {
	lines := make(chan inputLine, 64)
	go func() {
		for {
			text, err := reader.ReadString('\n')
			select {
			case lines <- inputLine{text: text, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case line := <-lines:
			if line.err != nil {
				if line.err == io.EOF {
					return nil
				}
				s.errLogger.Printf("Error reading input: %v", line.err)
				return line.err
			}
			if err := s.processMessage(ctx, line.text, stdout); err != nil {
				s.errLogger.Printf("Error handling message: %v", err)
				return err
			}
		}
	}
}

// toolCallWorker processes tool calls from the queue
func (s *StdioServer) toolCallWorker(ctx context.Context) {
	defer s.workerWg.Done()

	for {
		select {
		case work, ok := <-s.toolCallQueue:
			if !ok {
				// Channel closed, exit worker
				return
			}
			// The end of input is the shutdown: a call still queued then is
			// dropped, not started, and so is one the client cancelled while
			// it waited, which no handler is there yet to stop. Passing this
			// check admits the call to running work, which the shutdown and a
			// cancellation stop through its context like any other; it does
			// not make the handler's entry atomic with either.
			if ctx.Err() != nil || s.cancelled.isCancelled(work.id) {
				s.cancelled.finish(work.id)
				continue
			}
			// Process the tool call with panic recovery so a single
			// panicking handler does not kill the worker permanently.
			response := func() (resp mcp.JSONRPCMessage) {
				defer func() {
					if r := recover(); r != nil {
						s.errLogger.Printf("panic recovered in stdio tool call worker: %v", r)
						resp = createErrorResponse(work.id, mcp.INTERNAL_ERROR, fmt.Sprintf("internal panic: %v", r))
					}
				}()
				return s.server.HandleMessage(work.ctx, work.message)
			}()
			s.answer(work.id, response, work.writer)
		case <-ctx.Done():
			return
		}
	}
}

// answer writes a request's response, unless the client cancelled the
// request while it ran, in which case nothing is sent for it.
func (s *StdioServer) answer(id any, response mcp.JSONRPCMessage, writer io.Writer) {
	cancelled := s.cancelled.finish(id)
	if response == nil || cancelled {
		return
	}
	if err := s.writeResponse(response, writer); err != nil {
		s.errLogger.Printf("Error writing response: %v", err)
	}
}

// Listen starts listening for JSON-RPC messages on the provided input and writes responses to the provided output.
// It runs until the context is cancelled or an error occurs.
// Returns an error if there are issues with reading input or writing output.
func (s *StdioServer) Listen(
	ctx context.Context,
	stdin io.Reader,
	stdout io.Writer,
) error {
	// Initialize the tool call queue
	s.toolCallQueue = make(chan *toolCallWork, s.queueSize)

	// The one client's session, for the life of this Listen.
	if err := s.server.RegisterSession(ctx, s.session); err != nil {
		return fmt.Errorf("register session: %w", err)
	}
	defer s.server.UnregisterSession(ctx, s.session.SessionID())
	ctx = s.server.WithContext(ctx, s.session)

	// Add in any custom context.
	if s.contextFunc != nil {
		ctx = s.contextFunc(ctx)
	}

	// Requests are served on their own goroutines. Cancelling this context when
	// input processing ends releases any handler still blocked on it, so none
	// outlives the session it was registered against.
	ctx, cancelRequests := context.WithCancel(ctx)
	defer cancelRequests()

	reader := bufio.NewReader(stdin)

	// Start worker pool for tool calls
	for i := 0; i < s.workerPoolSize; i++ {
		s.workerWg.Add(1)
		go s.toolCallWorker(ctx)
	}

	// Start notification handler
	go s.handleNotifications(ctx, stdout)

	// Process input stream
	err := s.processInputStream(ctx, reader, stdout)

	// Shutdown workers gracefully
	cancelRequests()
	close(s.toolCallQueue)
	s.workerWg.Wait()
	s.requestWg.Wait()

	return err
}

// processMessage handles a single JSON-RPC message and writes the response.
// It parses the message, processes it through the wrapped MCPServer, and writes any response.
// Returns an error if there are issues with message processing or response writing.
func (s *StdioServer) processMessage(
	ctx context.Context,
	line string,
	writer io.Writer,
) error {
	// If line is empty, likely due to ctx cancellation
	if len(line) == 0 {
		return nil
	}

	// Parse the message as raw JSON
	var rawMessage json.RawMessage
	if err := json.Unmarshal([]byte(line), &rawMessage); err != nil {
		response := createErrorResponse(nil, mcp.PARSE_ERROR, "Parse error")
		return s.writeResponse(response, writer)
	}

	// Tool calls go to the worker pool, so a slow one never blocks the read loop.
	var baseMessage struct {
		Method string          `json:"method"`
		RawID  json.RawMessage `json:"id,omitempty"`
		ID     any             `json:"-"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  json.RawMessage `json:"error,omitempty"`
		Params struct {
			RequestID json.RawMessage `json:"requestId"`
		} `json:"params"`
	}
	parsed := json.Unmarshal(rawMessage, &baseMessage) == nil
	// A response is never answered, whatever its id, and the stdio server sends
	// no request for one to answer: it is dropped before any id bookkeeping. A
	// message with a method is a request, whatever it carries besides.
	if parsed && baseMessage.Method == "" && len(baseMessage.RawID) > 0 && (len(baseMessage.Result) > 0 || len(baseMessage.Error) > 0) {
		return nil
	}
	var validID bool
	baseMessage.ID, validID = requestIDValue(baseMessage.RawID)
	if parsed && !validID {
		return s.writeResponse(createErrorResponse(nil, mcp.INVALID_REQUEST, "Invalid request id"), writer)
	}
	if parsed && baseMessage.ID != nil {
		s.cancelled.start(baseMessage.ID)
	}
	if parsed && baseMessage.Method == string(mcp.MethodToolsCall) {
		// Queue tool calls for processing by workers
		select {
		case s.toolCallQueue <- &toolCallWork{
			ctx:     ctx,
			id:      baseMessage.ID,
			message: rawMessage,
			writer:  writer,
		}:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		default:
			// The queue is full: the call is refused at once rather than run
			// on the read loop, which must keep reading so that the end of
			// input, the shutdown, is seen however many lines wait behind it.
			// The pool and the queue bound the tool calls in flight. A call
			// without an id is a notification, dropped without a reply.
			s.errLogger.Printf("Tool call queue full, refusing the call")
			if baseMessage.ID != nil {
				s.answer(baseMessage.ID, createErrorResponse(baseMessage.ID, mcp.INTERNAL_ERROR,
					"server busy: the tool call queue is full, retry later"), writer)
			}
			return nil
		}
	}

	// Serve requests off the read loop: a handler that blocks, such as
	// subscriptions/listen, must not stall it. writeMu serialises the writes.
	if parsed && baseMessage.ID != nil {
		s.requestWg.Go(func() {
			s.handleRequest(ctx, baseMessage.ID, rawMessage, writer)
		})
		return nil
	}

	// A cancellation is noted before the core acts on it, so the cancelled
	// request's response is dropped whenever its handler returns.
	if parsed && baseMessage.Method == string(mcp.MethodNotificationCancelled) {
		id, _ := requestIDValue(baseMessage.Params.RequestID)
		s.cancelled.cancel(id)
	}

	// Notifications carry no response and must not queue behind a request.
	response := s.handleMessage(ctx, baseMessage.ID, rawMessage)

	// Only write response if there is one (not for notifications)
	if response != nil {
		if err := s.writeResponse(response, writer); err != nil {
			return fmt.Errorf("failed to write response: %w", err)
		}
	}

	return nil
}

// handleMessage runs HandleMessage, recovering a panicking handler so that
// it can't end the process. A request gets an INTERNAL_ERROR response
// carrying its id; a notification (nil id) gets no response at all.
func (s *StdioServer) handleMessage(ctx context.Context, id any, rawMessage json.RawMessage) (response mcp.JSONRPCMessage) {
	defer func() {
		if r := recover(); r != nil {
			s.errLogger.Printf("panic recovered in stdio message handler: %v", r)
			if id != nil {
				response = createErrorResponse(id, mcp.INTERNAL_ERROR, fmt.Sprintf("internal panic: %v", r))
			}
		}
	}()
	return s.server.HandleMessage(ctx, rawMessage)
}

// handleRequest serves one JSON-RPC request and writes its response. id is the
// request's JSON-RPC id, so a recovered panic stays correlatable by the client.
func (s *StdioServer) handleRequest(ctx context.Context, id any, rawMessage json.RawMessage, writer io.Writer) {
	s.answer(id, s.handleMessage(ctx, id, rawMessage), writer)
}

// writeResponse marshals and writes a JSON-RPC response message followed by a newline.
// Returns an error if marshaling or writing fails.
func (s *StdioServer) writeResponse(
	response mcp.JSONRPCMessage,
	writer io.Writer,
) error {
	responseBytes, err := json.Marshal(response)
	if err != nil {
		return err
	}

	// Protect concurrent writes
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// Write response followed by newline
	if _, err := fmt.Fprintf(writer, "%s\n", responseBytes); err != nil {
		return err
	}

	return nil
}

// ServeStdio is a convenience function that creates and starts a StdioServer with os.Stdin and os.Stdout.
// It sets up signal handling for graceful shutdown on SIGTERM and SIGINT.
// Returns an error if the server encounters any issues during operation.
func ServeStdio(server *MCPServer, opts ...StdioOption) error {
	s := NewStdioServer(server)

	for _, opt := range opts {
		opt(s)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Set up signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		<-sigChan
		cancel()
	}()

	return s.Listen(ctx, os.Stdin, os.Stdout)
}
