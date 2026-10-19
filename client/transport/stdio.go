package transport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/internal/jsonrpc"
	"github.com/mark3labs/mcp-go/mcp"
)

var (
	// ErrTransportClosed is returned when attempting to send a request or notification
	// to a transport that has already been closed.
	ErrTransportClosed = errors.New("transport closed")
	// ErrChildShutdownTimeout is returned when a stdio subprocess still has not exited
	// after the forced shutdown path has been attempted.
	ErrChildShutdownTimeout = errors.New("stdio child did not exit after forced shutdown")
	// ErrTransportNotStarted is returned when sending through a stdio
	// transport before Start.
	ErrTransportNotStarted = errors.New("stdio client not started")
)

// Stdio implements the transport layer of the MCP protocol using stdio communication.
// It launches a subprocess and communicates with it via standard input/output streams
// using JSON-RPC messages. The client handles message routing between requests and
// responses, and supports asynchronous notifications.
type Stdio struct {
	command string
	args    []string
	env     []string

	cmd              *exec.Cmd
	cmdFunc          CommandFunc
	stdin            io.WriteCloser
	stdinMu          sync.Mutex
	stdout           *bufio.Reader
	stderr           io.ReadCloser
	stderrWriter     io.Writer
	stderrRing       *stderrBuffer
	responses        map[responseKey]chan *JSONRPCResponse
	mu               sync.RWMutex
	done             chan struct{}
	closeOnce        sync.Once
	closeCleanupOnce sync.Once
	onNotification   func(mcp.JSONRPCNotification)
	notifyMu         sync.RWMutex
	onRequest        RequestHandler
	requestMu        sync.RWMutex
	ctx              context.Context
	stopHandlers     context.CancelFunc // ends ctx, the request handlers' context; Close calls it
	ctxMu            sync.RWMutex
	logger           *slog.Logger
	started          bool
	startedMu        sync.Mutex
	// lifecycleMu makes a Start, spawn included, one step against another
	// Start and against Close's closing of done: a closed transport spawns
	// nothing, and Close reads cmd, stdin and stderr after any spawn wrote
	// them.
	lifecycleMu sync.Mutex
}

const (
	gracefulShutdownTimeout = 2 * time.Second
	forceKillTimeout        = 3 * time.Second

	// stderrBufferSize bounds how much recent stderr output the transport
	// keeps available through Stderr(). The OS pipe itself is only ~64KB, so
	// a larger buffer would not add fidelity once the child outruns readers.
	stderrBufferSize = 64 * 1024
)

func waitForProcessExit(waitErrCh <-chan error, timeout time.Duration) (error, bool) {
	select {
	case err := <-waitErrCh:
		return err, true
	case <-time.After(timeout):
		return nil, false
	}
}

// StdioOption defines a function that configures a Stdio transport instance.
// Options can be used to customize the behavior of the transport before it starts,
// such as setting a custom command function.
type StdioOption func(*Stdio)

// CommandFunc is a factory function that returns a custom exec.Cmd used to launch the MCP subprocess.
// It can be used to apply sandboxing, custom environment control, working directories, etc.
type CommandFunc func(ctx context.Context, command string, env []string, args []string) (*exec.Cmd, error)

// WithCommandFunc sets a custom command factory function for the stdio transport.
// The CommandFunc is responsible for constructing the exec.Cmd used to launch the subprocess,
// allowing control over attributes like environment, working directory, and system-level sandboxing.
func WithCommandFunc(f CommandFunc) StdioOption {
	return func(s *Stdio) {
		s.cmdFunc = f
	}
}

// WithCommandLogger sets a custom structured logger for the stdio transport.
// A nil logger falls back to slog.Default().
func WithCommandLogger(logger *slog.Logger) StdioOption {
	return func(s *Stdio) {
		if logger == nil {
			s.logger = slog.Default()
			return
		}
		s.logger = logger
	}
}

// WithCommandStderrWriter sets the destination for the subprocess's stderr
// output. The default destination is io.Discard: the transport drains stderr
// continuously so that the OS pipe (about 64KB) can never fill up and block
// the child process, which would deadlock the whole stdio channel.
// Pass os.Stderr, a log file, or an in-memory buffer to capture the child's
// stderr instead.
func WithCommandStderrWriter(w io.Writer) StdioOption {
	return func(s *Stdio) {
		s.stderrWriter = w
	}
}

// NewStdio creates a stdio transport for a subprocess running command with
// args and env. Nothing is launched until Start, which spawns the command,
// sets up its pipes and reports any failure to do so.
func NewStdio(
	command string,
	env []string,
	args ...string,
) *Stdio {
	return NewStdioWithOptions(command, env, args)
}

// NewStdioWithOptions is NewStdio with options that customize the transport
// before it starts, such as a custom command factory. Nothing is launched
// until Start.
func NewStdioWithOptions(
	command string,
	env []string,
	args []string,
	opts ...StdioOption,
) *Stdio {
	s := &Stdio{
		command: command,
		args:    args,
		env:     env,

		responses: make(map[responseKey]chan *JSONRPCResponse),
		done:      make(chan struct{}),
		ctx:       context.Background(),
		logger:    slog.Default(),
	}

	for _, opt := range opts {
		opt(s)
	}

	return s
}

// NewIO returns a new stdio-based transport using existing input, output, and
// logging streams instead of spawning a subprocess.
// This is useful for testing and simulating client behavior. Close closes
// output and logging; input is the caller's to close.
func NewIO(input io.Reader, output io.WriteCloser, logging io.ReadCloser) *Stdio {
	return &Stdio{
		stdin:  output,
		stdout: bufio.NewReader(input),
		stderr: logging,

		responses: make(map[responseKey]chan *JSONRPCResponse),
		done:      make(chan struct{}),
		ctx:       context.Background(),
		logger:    slog.Default(),
	}
}

// Start launches the command, if the transport has one, and starts reading
// its output. ctx governs the child's whole life and every request handler.
// Start is idempotent, waits for a Start in progress, and refuses with
// ErrTransportClosed once the transport is closed.
func (c *Stdio) Start(ctx context.Context) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()
	select {
	case <-c.done:
		return ErrTransportClosed
	default:
	}
	c.startedMu.Lock()
	if c.started {
		c.startedMu.Unlock()
		return nil
	}
	c.started = true
	c.startedMu.Unlock()

	// The request handlers run on a context of their own, which Close ends;
	// the child keeps ctx, and Close shuts it down in its own order.
	c.ctxMu.Lock()
	c.ctx, c.stopHandlers = context.WithCancel(ctx)
	c.ctxMu.Unlock()

	if err := c.spawnCommand(ctx); err != nil {
		c.ctxMu.Lock()
		c.stopHandlers()
		c.ctxMu.Unlock()
		c.startedMu.Lock()
		c.started = false
		c.startedMu.Unlock()
		return err
	}

	ready := make(chan struct{})
	go func() {
		c.readResponses(ready)
	}()
	<-ready

	return nil
}

// spawnCommand spawns a new process running the configured command, args, and env.
// If an (optional) cmdFunc custom command factory function was configured, it will be used to construct the subprocess;
// otherwise, the default behavior uses exec.CommandContext with the merged environment.
// Initializes stdin, stdout, and stderr pipes for JSON-RPC communication.
func (c *Stdio) spawnCommand(ctx context.Context) error {
	if c.command == "" {
		return nil
	}

	var cmd *exec.Cmd
	var err error

	// Standard behavior if no command func present.
	if c.cmdFunc == nil {
		cmd = exec.CommandContext(ctx, c.command, c.args...)
		cmd.Env = append(os.Environ(), c.env...)
	} else if cmd, err = c.cmdFunc(ctx, c.command, c.env, c.args); err != nil {
		return err
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("failed to create stdout pipe: %w", err)
	}

	// Respect a stderr destination configured by a custom command factory;
	// otherwise capture stderr so it can be drained below.
	var stderr io.ReadCloser
	if cmd.Stderr == nil {
		stderr, err = cmd.StderrPipe()
		if err != nil {
			return fmt.Errorf("failed to create stderr pipe: %w", err)
		}
	}

	// A failed start closes the pipes itself, so the transport keeps them,
	// and the child, only once it runs.
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start command: %w", err)
	}

	c.cmd = cmd
	c.stdin = stdin
	c.stderr = stderr
	c.stdout = bufio.NewReader(stdout)

	// Drain stderr continuously so the subprocess can never deadlock on a
	// full pipe (see startStderrDrain).
	if stderr != nil {
		c.startStderrDrain(stderr)
	}

	return nil
}

// closeDone safely closes the done channel exactly once, unblocking all
// in-flight SendRequest calls. Safe to call from multiple goroutines.
func (c *Stdio) closeDone() {
	c.closeOnce.Do(func() { close(c.done) })
}

// Close shuts down the stdio client, closing the stdin pipe and waiting for the subprocess to exit.
// Returns an error if there are issues closing stdin or waiting for the subprocess to terminate.
// Safe to call multiple times and concurrently with readResponses calling closeDone().
func (c *Stdio) Close() error {
	// Signal all in-flight requests to unblock, after any Start in progress
	// has finished spawning.
	c.lifecycleMu.Lock()
	c.closeDone()
	c.lifecycleMu.Unlock()
	// End the handlers still serving requests the server sent: their
	// answers have nowhere to go.
	c.ctxMu.RLock()
	if c.stopHandlers != nil {
		c.stopHandlers()
	}
	c.ctxMu.RUnlock()

	// Clean up exactly once, even when readResponses already called
	// closeDone (the server died): stdin and stderr are closed and the child
	// is waited for, so no descriptor or zombie process is left behind.
	var closeErr error
	c.closeCleanupOnce.Do(func() {
		// Closed after the child exits: on Windows a pipe Close blocks while
		// the stderr drain's read is pending. cmd.Wait may close it first.
		defer func() {
			if c.stderr != nil {
				if err := c.stderr.Close(); err != nil && !errors.Is(err, os.ErrClosed) && closeErr == nil {
					closeErr = fmt.Errorf("failed to close stderr: %w", err)
				}
			}
		}()
		if c.stdin != nil {
			if err := c.stdin.Close(); err != nil {
				closeErr = fmt.Errorf("failed to close stdin: %w", err)
			}
		}
		if c.cmd != nil {
			waitErrCh := make(chan error, 1)
			go func() {
				waitErrCh <- c.cmd.Wait()
			}()

			if err, done := waitForProcessExit(waitErrCh, gracefulShutdownTimeout); done {
				if err != nil && closeErr == nil {
					closeErr = err
				}
				return
			}

			forceKilled := false
			if c.cmd.Process != nil {
				// SIGTERM is not implemented on Windows. Kill immediately after
				// the graceful stdin-close wait so Close does not spend an
				// extra forceKillTimeout waiting for a signal that never lands.
				if runtime.GOOS == "windows" {
					if err := c.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) && closeErr == nil {
						closeErr = fmt.Errorf("failed to kill process: %w", err)
					}
					forceKilled = true
				} else {
					_ = c.cmd.Process.Signal(syscall.SIGTERM)
				}
			}

			if err, done := waitForProcessExit(waitErrCh, forceKillTimeout); done {
				if err != nil && closeErr == nil {
					closeErr = err
				}
				return
			}

			if forceKilled {
				if closeErr == nil {
					closeErr = ErrChildShutdownTimeout
				}
				return
			}

			if c.cmd.Process != nil {
				if err := c.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) && closeErr == nil {
					closeErr = fmt.Errorf("failed to kill process: %w", err)
				}
			}

			if err, done := waitForProcessExit(waitErrCh, forceKillTimeout); done {
				if err != nil && closeErr == nil {
					closeErr = err
				}
			} else if closeErr == nil {
				closeErr = ErrChildShutdownTimeout
			}
		}
	})
	return closeErr
}

// GetSessionId returns the session ID of the transport.
// Since stdio does not maintain a session ID, it returns an empty string.
func (c *Stdio) GetSessionId() string {
	return ""
}

// SetNotificationHandler sets the handler function to be called when a notification is received.
// Only one handler can be set at a time; setting a new one replaces the previous handler.
// The handler runs on the read loop: it may set the handler, but must not wait
// for the answer to a request, which the read loop would have to deliver.
func (c *Stdio) SetNotificationHandler(
	handler func(notification mcp.JSONRPCNotification),
) {
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	c.onNotification = handler
}

// SetRequestHandler sets the handler function to be called when a request is received from the server.
// This enables bidirectional communication for features like sampling.
func (c *Stdio) SetRequestHandler(handler RequestHandler) {
	c.requestMu.Lock()
	defer c.requestMu.Unlock()
	c.onRequest = handler
}

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
				if err != io.EOF && !errors.Is(err, context.Canceled) && !errors.Is(err, fs.ErrClosed) {
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

// errUndecodableResponse is the error of a request whose response named it
// but could not be decoded.
var errUndecodableResponse = errors.New("failed to decode response")

// received is SendRequest's result for what arrived on its response
// channel: nil there is an answer that could not be decoded.
func received(response *JSONRPCResponse) (*JSONRPCResponse, error) {
	if response == nil {
		return nil, errUndecodableResponse
	}
	return response, nil
}

// SendRequest sends a JSON-RPC request to the server and waits for a response.
// It creates a unique request ID, sends the request over stdin, and waits for
// the corresponding response or context cancellation.
// Returns the raw JSON response message or an error if the request fails.
func (c *Stdio) SendRequest(
	ctx context.Context,
	request JSONRPCRequest,
) (*JSONRPCResponse, error) {
	// Check if transport is closed or context is already canceled before doing any work
	select {
	case <-c.done:
		return nil, ErrTransportClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if c.stdin == nil {
		return nil, ErrTransportNotStarted
	}

	// Marshal request
	requestBytes, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	requestBytes = append(requestBytes, '\n')

	idKey := responseKeyOf(request.ID)

	// Register response channel
	responseChan := make(chan *JSONRPCResponse, 1)
	c.mu.Lock()
	c.responses[idKey] = responseChan
	c.mu.Unlock()
	deleteResponseChan := func() {
		c.mu.Lock()
		delete(c.responses, idKey)
		c.mu.Unlock()
	}

	if err := c.writeLine(ctx, requestBytes); err != nil {
		deleteResponseChan()
		return nil, fmt.Errorf("failed to write request: %w", err)
	}

	select {
	case <-c.done:
		// Drain responseChan first: a valid response may have been delivered
		// just before readResponses closed the done channel on EOF.
		select {
		case response := <-responseChan:
			return received(response)
		default:
		}
		deleteResponseChan()
		return nil, ErrTransportClosed
	case <-ctx.Done():
		deleteResponseChan()
		return nil, ctx.Err()
	case response := <-responseChan:
		return received(response)
	}
}

// SendNotification sends a json RPC Notification to the server.
func (c *Stdio) SendNotification(
	ctx context.Context,
	notification mcp.JSONRPCNotification,
) error {
	select {
	case <-c.done:
		return ErrTransportClosed
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	if c.stdin == nil {
		return ErrTransportNotStarted
	}

	notificationBytes, err := json.Marshal(notification)
	if err != nil {
		return fmt.Errorf("failed to marshal notification: %w", err)
	}
	notificationBytes = append(notificationBytes, '\n')

	if err := c.writeLine(ctx, notificationBytes); err != nil {
		return fmt.Errorf("failed to write notification: %w", err)
	}

	return nil
}

// writeLine writes one JSON-RPC line to stdin. stdinMu keeps concurrent
// writes from interleaving their lines. A child that stops reading blocks the
// write inside the pipe, where only Close, closing stdin, ends it. A send
// that finds stdin free writes in place, at no cost; one that finds it busy
// and whose ctx can end waits from a goroutine of its own and gives up with
// ctx, and if its turn comes after that, it writes nothing. A write that
// Close cut short is ErrTransportClosed.
func (c *Stdio) writeLine(ctx context.Context, line []byte) error {
	if c.stdinMu.TryLock() {
		_, err := c.stdin.Write(line)
		c.stdinMu.Unlock()
		return c.writeError(err)
	}
	if ctx.Done() == nil {
		c.stdinMu.Lock()
		_, err := c.stdin.Write(line)
		c.stdinMu.Unlock()
		return c.writeError(err)
	}
	written := make(chan error, 1)
	go func() {
		c.stdinMu.Lock()
		defer c.stdinMu.Unlock()
		if err := ctx.Err(); err != nil {
			written <- err
			return
		}
		_, err := c.stdin.Write(line)
		written <- err
	}()
	select {
	case err := <-written:
		return c.writeError(err)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// writeError is the error of a stdin write: ErrTransportClosed when the
// transport closed under it, which closes stdin.
func (c *Stdio) writeError(err error) error {
	if err == nil {
		return nil
	}
	select {
	case <-c.done:
		return fmt.Errorf("%w: %w", ErrTransportClosed, err)
	default:
		return err
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
