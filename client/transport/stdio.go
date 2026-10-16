package transport

import (
	"bufio"
	"bytes"
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
	responses        map[string]chan *JSONRPCResponse
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

// stderrBuffer is a bounded, drop-oldest buffer of the subprocess's stderr
// output. The transport writes into it continuously so the OS pipe never
// fills up and blocks the child, while callers can still read recent stderr
// through Stderr(). Reads block until data is available or the stream ends.
type stderrBuffer struct {
	mu   sync.Mutex
	cond *sync.Cond
	buf  bytes.Buffer
	done bool
}

// newStderrBuffer returns an empty, open stderr buffer ready for writing.
func newStderrBuffer() *stderrBuffer {
	b := &stderrBuffer{}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// Write appends p to the buffer, dropping the oldest data when the buffer is
// full. It never blocks, so a child writing to stderr can never deadlock the
// transport.
func (b *stderrBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		return 0, io.ErrClosedPipe
	}
	// Report the original length per the io.Writer contract even when the
	// tail truncation below drops the leading bytes.
	written := len(p)
	if len(p) > stderrBufferSize {
		p = p[len(p)-stderrBufferSize:]
	}
	if overflow := b.buf.Len() + len(p) - stderrBufferSize; overflow > 0 {
		b.buf.Next(overflow)
	}
	b.buf.Write(p)
	b.cond.Broadcast()
	return written, nil
}

// Read returns buffered data, blocking until data is available or the stream
// is marked done.
func (b *stderrBuffer) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.buf.Len() == 0 && !b.done {
		b.cond.Wait()
	}
	if b.buf.Len() == 0 {
		return 0, io.EOF
	}
	return b.buf.Read(p)
}

// Close marks the stream as ended and wakes up blocked readers.
func (b *stderrBuffer) Close() error {
	b.mu.Lock()
	b.done = true
	b.cond.Broadcast()
	b.mu.Unlock()
	return nil
}

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

		responses: make(map[string]chan *JSONRPCResponse),
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

		responses: make(map[string]chan *JSONRPCResponse),
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

	c.cmd = cmd
	c.stdin = stdin
	c.stderr = stderr
	c.stdout = bufio.NewReader(stdout)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start command: %w", err)
	}

	// Drain stderr continuously so the subprocess can never deadlock on a
	// full pipe (see startStderrDrain).
	if stderr != nil {
		c.startStderrDrain(stderr)
	}

	return nil
}

// startStderrDrain starts the background goroutine that keeps the subprocess
// stderr pipe empty. An unread pipe holds only about 64KB; once it fills, the
// child blocks inside write(2) and stops answering on stdout, deadlocking
// every request (including Ping) with no error. The output is written to the
// bounded ring buffer (readable through Stderr()) and mirrored to
// stderrWriter, which defaults to io.Discard.
func (c *Stdio) startStderrDrain(stderr io.ReadCloser) {
	c.stderrRing = newStderrBuffer()
	go drainStderrIntoRing(c.stderrRing, stderr, c.stderrWriter, c.logger)
}

// drainStderrIntoRing copies src into the ring buffer until the stream ends,
// closing the ring afterwards to wake readers blocked in Stderr().Read.
//
// The drain must never stop while the child runs: a full OS pipe blocks the
// child inside write(2) and deadlocks every request. When dest is nil the
// stream is drained directly into the ring without any mirroring overhead;
// otherwise mirroring is decoupled through a bounded queue so a failing or
// blocked writer cannot stop the pipe drain: once the queue fills up (or the
// mirror goroutine exits) further chunks are dropped and the loss is logged
// at debug level. The mirror goroutine is intentionally not waited for: a
// writer stuck inside Write would otherwise delay ring Close and leave
// Stderr() readers blocked.
func drainStderrIntoRing(ring *stderrBuffer, src io.ReadCloser, dest io.Writer, logger *slog.Logger) {
	defer func() {
		_ = ring.Close()
	}()

	// No custom destination configured: drain directly without spawning a
	// mirror goroutine or allocating mirror chunks.
	if dest == nil {
		_, _ = io.Copy(ring, src)
		return
	}

	mirror := make(chan []byte, 8)
	go func() {
		for chunk := range mirror {
			if _, err := dest.Write(chunk); err != nil {
				return
			}
		}
	}()

	buf := make([]byte, 32*1024)
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if _, werr := ring.Write(buf[:n]); werr != nil {
				break
			}
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			select {
			case mirror <- chunk:
			default:
				// Mirror is full or gone; dropping the
				// chunk keeps the pipe draining.
				if logger != nil {
					logger.Debug("stderr mirror queue full; dropping chunk", "bytes", n)
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	close(mirror)
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
	// below copies what it keeps.
	var buf []byte
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
			var baseMessage struct {
				JSONRPC jsonrpcVersion  `json:"jsonrpc"`
				ID      json.RawMessage `json:"id,omitempty"`
				Method  jsonrpc.Method  `json:"method"`
				Result  json.RawMessage `json:"result,omitempty"`
				Error   json.RawMessage `json:"error,omitempty"`
			}
			if err := json.Unmarshal(encoded, &baseMessage); err != nil {
				continue
			}
			var id mcp.RequestId
			readableID := len(baseMessage.ID) > 0 && json.Unmarshal(baseMessage.ID, &id) == nil && !id.IsNil()

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
					c.answer(id.String(), nil)
					continue
				}
				response.Error = &details
			}
			c.answer(id.String(), &response)
		}
	}
}

// maxRetainedLine is the largest line buffer readResponses keeps for the
// next line; one a larger message grew is let go.
const maxRetainedLine = 1 << 20

// readLine reads the next line from r into buf, whose storage it reuses, and
// returns it without its line end. The line lives in buf until the next read.
func readLine(r *bufio.Reader, buf []byte) ([]byte, error) {
	buf = buf[:0]
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return bytes.TrimRight(buf, "\r\n"), err
	}
}

// answer hands response to the request waiting under idKey, if any; a nil
// response tells it the answer could not be decoded.
func (c *Stdio) answer(idKey string, response *JSONRPCResponse) {
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

	// Create string key for map lookup
	idKey := request.ID.String()

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

// Stderr returns a reader for the stderr output of the subprocess.
// This can be used to capture error messages or logs from the subprocess.
//
// The transport drains the subprocess's stderr into a bounded buffer so the
// OS pipe never fills up and deadlocks the child. The reader returns the most
// recent output; older output is dropped once the buffer is full. To capture
// the full, real-time stream, use WithCommandStderrWriter instead.
func (c *Stdio) Stderr() io.Reader {
	if c.stderrRing != nil {
		return c.stderrRing
	}
	return c.stderr
}
