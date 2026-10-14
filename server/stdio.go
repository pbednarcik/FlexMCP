package server

import (
	"bufio"
	"bytes"
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

	"github.com/mark3labs/mcp-go/internal/jsonrpc"
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

	// responses carries every response to the one goroutine that writes
	// stdout, which writes a response after the notifications queued
	// before it; writerDone closes when that goroutine has written its last.
	// listenDone is the caller's cancel of Listen: a response waiting to be
	// queued gives up on it. A failed write ends Listen: the writer keeps
	// the error in writeErr, closes writeFailed, and stops the input through
	// stopInput; nothing more is written.
	responses   chan mcp.JSONRPCMessage
	writerDone  chan struct{}
	listenDone  <-chan struct{}
	writeFailed chan struct{}
	writeErr    error
	stopInput   context.CancelFunc

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
	read    *requestEnvelope // the message read for the core, or nil
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

// writeLoop is the one writer of stdout. It writes the session's
// notifications as they come, and each response once every notification
// queued before it is out: a handler's notifications are queued before it
// returns, so a request's own precede its response, and progress stops
// with the request that reported it (Patterns.md row 34). It runs until
// responses is closed, which Listen does once every request is answered,
// and writes the notifications still queued before it returns.
func (s *StdioServer) writeLoop(stdout io.Writer) {
	defer close(s.writerDone)
	for {
		select {
		case notification := <-s.session.notifications:
			s.write(notification, stdout)
		case response, ok := <-s.responses:
			s.drainNotifications(stdout)
			if !ok {
				return
			}
			s.write(response, stdout)
		}
	}
}

// drainNotifications writes the notifications queued so far: those, and
// not every one that keeps coming while they are written, precede the
// response that follows. This goroutine is the channel's one receiver, so
// what was counted is there to take.
func (s *StdioServer) drainNotifications(stdout io.Writer) {
	for range len(s.session.notifications) {
		s.write(<-s.session.notifications, stdout)
	}
}

// write writes one message to stdout. The first failure ends Listen, which
// returns the error: the client is gone, and nothing more could reach it;
// what is still queued is dropped.
func (s *StdioServer) write(message mcp.JSONRPCMessage, stdout io.Writer) {
	if s.writeErr != nil {
		return
	}
	err := s.writeResponse(message, stdout)
	if err == nil {
		return
	}
	s.errLogger.Printf("Error writing to stdout, ending Listen: %v", err)
	s.writeErr = err
	close(s.writeFailed)
	s.stopInput()
}

// queue hands a response to the writer. It gives up when the caller
// cancelled Listen, which does not wait on a client that stopped reading.
func (s *StdioServer) queue(response mcp.JSONRPCMessage) {
	select {
	case s.responses <- response:
	case <-s.listenDone:
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
						resp = createErrorResponse(work.id, mcp.INTERNAL_ERROR, internalErrorMessage)
					}
				}()
				return s.server.handleMessageRead(work.ctx, work.message, work.read)
			}()
			s.answer(work.id, response, work.writer)
		case <-ctx.Done():
			return
		}
	}
}

// answer queues a request's response for the writer, unless the client
// cancelled the request while it ran, in which case nothing is sent for it.
func (s *StdioServer) answer(id any, response mcp.JSONRPCMessage, _ io.Writer) {
	cancelled := s.cancelled.finish(id)
	if response == nil || cancelled {
		return
	}
	s.queue(response)
}

// Listen starts listening for JSON-RPC messages on the provided input and
// writes responses to the provided output. It runs until the input ends,
// the context is cancelled, or reading the input or writing the output
// fails, and returns that error: a failed write ends it, since the client
// is gone.
func (s *StdioServer) Listen(
	ctx context.Context,
	stdin io.Reader,
	stdout io.Writer,
) error {
	// Initialize the tool call queue
	s.toolCallQueue = make(chan *toolCallWork, s.queueSize)
	s.responses = make(chan mcp.JSONRPCMessage, 16)
	s.writerDone = make(chan struct{})
	s.writeFailed = make(chan struct{})
	s.writeErr = nil

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
	// outlives the session it was registered against. The caller's own cancel
	// is kept apart: it ends the wait for the writer below.
	callerDone := ctx.Done()
	ctx, cancelRequests := context.WithCancel(ctx)
	defer cancelRequests()
	s.listenDone, s.stopInput = callerDone, cancelRequests

	reader := bufio.NewReader(stdin)

	// Start worker pool for tool calls
	for i := 0; i < s.workerPoolSize; i++ {
		s.workerWg.Add(1)
		go s.toolCallWorker(ctx)
	}

	// Start the one writer of stdout
	go s.writeLoop(stdout)

	// Process input stream
	err := s.processInputStream(ctx, reader, stdout)

	// Shutdown workers gracefully, then the writer, once nothing more can
	// be queued: what was queued is still written, unless the caller
	// cancelled Listen, which asks for the end: a writer stuck on a client
	// that stopped reading would hold it for good.
	cancelRequests()
	close(s.toolCallQueue)
	s.workerWg.Wait()
	s.requestWg.Wait()
	close(s.responses)
	select {
	case <-s.writerDone:
	case <-callerDone:
	}

	// A failed write ended the input; its error is the one to report.
	select {
	case <-s.writeFailed:
		err = s.writeErr
	default:
	}
	return err
}

// processMessage handles a single JSON-RPC message: it parses the message,
// has the wrapped MCPServer process it, and queues any response for the
// writer. Returns an error when the message cannot be taken up.
func (s *StdioServer) processMessage(
	ctx context.Context,
	line string,
	writer io.Writer,
) error {
	// If line is empty, likely due to ctx cancellation
	if len(line) == 0 {
		return nil
	}

	rawMessage, ok := stdioMessage(line)
	if !ok {
		s.queue(createErrorResponse(nil, mcp.PARSE_ERROR, "Parse error"))
		return nil
	}

	// Tool calls go to the worker pool, so a slow one never blocks the read loop.
	baseMessage, read, parsed := readStdioMessage(rawMessage)
	// A response is never answered, whatever its id, and the stdio server sends
	// no request for one to answer: it is dropped before any id bookkeeping. A
	// message with a method member is a request, whatever it carries besides.
	if parsed && !jsonrpc.MethodOf(baseMessage.Method, rawMessage).Present && len(baseMessage.RawID) > 0 && (len(baseMessage.Result) > 0 || len(baseMessage.Error) > 0) {
		return nil
	}
	id, validID := requestIDValue(baseMessage.RawID)
	if parsed && !validID {
		s.queue(createErrorResponse(nil, mcp.INVALID_REQUEST, "Invalid request id"))
		return nil
	}
	if parsed && id != nil {
		s.cancelled.start(id)
	}
	if parsed && baseMessage.Method == string(mcp.MethodToolsCall) {
		// Queue tool calls for processing by workers
		select {
		case s.toolCallQueue <- &toolCallWork{
			ctx:     ctx,
			id:      id,
			message: rawMessage,
			read:    read,
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
			if id != nil {
				s.answer(id, createErrorResponse(id, mcp.INTERNAL_ERROR,
					"server busy: the tool call queue is full, retry later"), writer)
			}
			return nil
		}
	}

	// Serve requests off the read loop: a handler that blocks, such as
	// subscriptions/listen, must not stall it. writeMu serialises the writes.
	if parsed && id != nil {
		s.requestWg.Go(func() {
			s.handleRequest(ctx, id, rawMessage, read, writer)
		})
		return nil
	}

	// A cancellation is noted before the core acts on it, so the cancelled
	// request's response is dropped whenever its handler returns.
	if parsed && baseMessage.Method == string(mcp.MethodNotificationCancelled) {
		cancelled, _ := requestIDValue(baseMessage.Params.RequestID)
		s.cancelled.cancel(cancelled)
	}

	// Notifications carry no response and must not queue behind a request.
	response := s.handleMessage(ctx, id, rawMessage, read)

	// Only write response if there is one (not for notifications)
	if response != nil {
		s.queue(response)
	}

	return nil
}

// stdioMembers is what the read loop needs of a message before it hands it
// over.
type stdioMembers struct {
	Method string          `json:"method"`
	RawID  json.RawMessage `json:"id,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
	Params struct {
		RequestID json.RawMessage `json:"requestId"`
	} `json:"params"`
}

// readStdioMessage reads a message once, for the read loop and the core; ok
// is false when not even the members the loop needs can be read. read is the
// read the core takes over; when the message does not read whole (params or
// _meta not an object, a member named twice), it is nil, and the members are
// decoded on their own, as they always were.
func readStdioMessage(message json.RawMessage) (members stdioMembers, read *requestEnvelope, ok bool) {
	read = new(requestEnvelope)
	if readEnvelope(message, read) != nil {
		return readStdioMembers(message)
	}
	members.Method = string(read.Method)
	members.RawID, members.Result, members.Error = read.RawID, read.Result, read.Error
	members.Params.RequestID = read.Params.RequestID
	return members, read, true
}

// readStdioMembers decodes the members on their own.
func readStdioMembers(message json.RawMessage) (stdioMembers, *requestEnvelope, bool) {
	var members stdioMembers
	ok := json.Unmarshal(message, &members) == nil
	return members, nil, ok
}

// stdioMessage is the JSON-RPC message a line holds, or false when the line
// is not JSON.
func stdioMessage(line string) (json.RawMessage, bool) {
	// The line is checked, not decoded: its copy is the message, without the
	// JSON whitespace around it, as a json.RawMessage decoded from it was.
	message := []byte(line)
	if !json.Valid(message) {
		return nil, false
	}
	return bytes.Trim(message, " \t\r\n"), true
}

// handleMessage runs HandleMessage, recovering a panicking handler so that
// it can't end the process. A request gets an INTERNAL_ERROR response
// carrying its id; a notification (nil id) gets no response at all.
func (s *StdioServer) handleMessage(ctx context.Context, id any, rawMessage json.RawMessage, read *requestEnvelope) (response mcp.JSONRPCMessage) {
	defer func() {
		if r := recover(); r != nil {
			s.errLogger.Printf("panic recovered in stdio message handler: %v", r)
			if id != nil {
				response = createErrorResponse(id, mcp.INTERNAL_ERROR, internalErrorMessage)
			}
		}
	}()
	return s.server.handleMessageRead(ctx, rawMessage, read)
}

// handleRequest serves one JSON-RPC request and writes its response. id is the
// request's JSON-RPC id, so a recovered panic stays correlatable by the client.
func (s *StdioServer) handleRequest(ctx context.Context, id any, rawMessage json.RawMessage, read *requestEnvelope, writer io.Writer) {
	s.answer(id, s.handleMessage(ctx, id, rawMessage, read), writer)
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

	// One write: lines from concurrent requests must not interleave.
	if _, err := writer.Write(append(responseBytes, '\n')); err != nil {
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
