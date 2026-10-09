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

	// Thread-safe tool call processing
	toolCallQueue  chan *toolCallWork
	workerWg       sync.WaitGroup
	requestWg      sync.WaitGroup // Tracks in-flight request handlers
	workerPoolSize int
	queueSize      int
	writeMu        sync.Mutex // Protects concurrent writes
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

// stdioSession is a static client session, since stdio has only one client.
type stdioSession struct {
	clientInfoStore // provides Get/SetClientInfo and Get/SetClientCapabilities via method promotion

	notifications chan mcp.JSONRPCNotification
	initialized   atomic.Bool
	loggingLevel  atomic.Value
}

func (s *stdioSession) SessionID() string {
	return "stdio"
}

func (s *stdioSession) NotificationChannel() chan<- mcp.JSONRPCNotification {
	return s.notifications
}

func (s *stdioSession) Initialize() {
	// set default logging level
	s.loggingLevel.Store(mcp.LoggingLevelError)
	s.initialized.Store(true)
}

func (s *stdioSession) Initialized() bool {
	return s.initialized.Load()
}

func (s *stdioSession) SetLogLevel(level mcp.LoggingLevel) {
	s.loggingLevel.Store(level)
}

func (s *stdioSession) GetLogLevel() mcp.LoggingLevel {
	level := s.loggingLevel.Load()
	if level == nil {
		return mcp.LoggingLevelError
	}
	return level.(mcp.LoggingLevel)
}

var (
	_ ClientSession         = (*stdioSession)(nil)
	_ SessionWithLogging    = (*stdioSession)(nil)
	_ SessionWithClientInfo = (*stdioSession)(nil)
)

var stdioSessionInstance = stdioSession{
	notifications: make(chan mcp.JSONRPCNotification, 100),
}

// NewStdioServer creates a new stdio server wrapper around an MCPServer.
// It initializes the server with a default error logger that discards all output.
func NewStdioServer(server *MCPServer) *StdioServer {
	return &StdioServer{
		server: server,
		errLogger: log.New(
			os.Stderr,
			"",
			log.LstdFlags,
		), // Default to discarding logs
		workerPoolSize: 5,   // Default worker pool size
		queueSize:      100, // Default queue size
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
		case notification := <-stdioSessionInstance.notifications:
			if err := s.writeResponse(notification, stdout); err != nil {
				s.errLogger.Printf("Error writing notification: %v", err)
			}
		case <-ctx.Done():
			return
		}
	}
}

// processInputStream continuously reads and processes messages from the input stream.
// It handles EOF gracefully as a normal termination condition.
// The function returns when either:
// - The context is cancelled (returns context.Err())
// - EOF is encountered (returns nil)
// - An error occurs while reading or processing messages (returns the error)
func (s *StdioServer) processInputStream(ctx context.Context, reader *bufio.Reader, stdout io.Writer) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		line, err := s.readNextLine(ctx, reader)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			s.errLogger.Printf("Error reading input: %v", err)
			return err
		}

		if err := s.processMessage(ctx, line, stdout); err != nil {
			if err == io.EOF {
				return nil
			}
			s.errLogger.Printf("Error handling message: %v", err)
			return err
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
			if response != nil {
				if err := s.writeResponse(response, work.writer); err != nil {
					s.errLogger.Printf("Error writing tool response: %v", err)
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

// readNextLine reads a single line from the input reader in a context-aware manner.
// It uses channels to make the read operation cancellable via context.
// Returns the read line and any error encountered. If the context is cancelled,
// returns an empty string and the context's error. EOF is returned when the input
// stream is closed.
func (s *StdioServer) readNextLine(ctx context.Context, reader *bufio.Reader) (string, error) {
	type result struct {
		line string
		err  error
	}

	resultCh := make(chan result, 1)

	go func() {
		line, err := reader.ReadString('\n')
		resultCh <- result{line: line, err: err}
	}()

	select {
	case <-ctx.Done():
		return "", nil
	case res := <-resultCh:
		return res.line, res.err
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

	// Set a static client context since stdio only has one client
	if err := s.server.RegisterSession(ctx, &stdioSessionInstance); err != nil {
		return fmt.Errorf("register session: %w", err)
	}
	defer s.server.UnregisterSession(ctx, stdioSessionInstance.SessionID())
	ctx = s.server.WithContext(ctx, &stdioSessionInstance)

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
		Method string `json:"method"`
		ID     any    `json:"id,omitempty"`
	}
	parsed := json.Unmarshal(rawMessage, &baseMessage) == nil
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
			// Queue is full, process synchronously as fallback
			s.errLogger.Printf("Tool call queue full, processing synchronously")
			response := s.handleMessage(ctx, baseMessage.ID, rawMessage)
			if response != nil {
				return s.writeResponse(response, writer)
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
	response := s.handleMessage(ctx, id, rawMessage)
	if response != nil {
		if err := s.writeResponse(response, writer); err != nil {
			s.errLogger.Printf("Error writing response: %v", err)
		}
	}
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
