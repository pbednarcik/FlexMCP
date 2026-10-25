package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
)

// InProcessTransport calls an MCPServer directly, with no wire in between:
// requests go to HandleMessage, and the server's notifications are forwarded
// from the session to the client's handler.
type InProcessTransport struct {
	server    *server.MCPServer
	session   *server.InProcessSession
	sessionID string

	onNotification func(mcp.JSONRPCNotification)
	notifyMu       sync.RWMutex
	started        bool
	starting       bool
	startDone      chan struct{}
	closed         bool
	startedMu      sync.Mutex

	done      chan struct{}
	closeOnce sync.Once

	// closeCtx ends at Close: every request in flight is cancelled through
	// it, as a dropped connection cancels one on the wire transports.
	closeCtx    context.Context
	closeCancel context.CancelFunc
}

// NewInProcessTransport returns a transport that calls server directly.
func NewInProcessTransport(server *server.MCPServer) *InProcessTransport {
	closeCtx, closeCancel := context.WithCancel(context.Background())
	return &InProcessTransport{
		server:      server,
		sessionID:   server.GenerateInProcessSessionID(),
		done:        make(chan struct{}),
		closeCtx:    closeCtx,
		closeCancel: closeCancel,
	}
}

func (c *InProcessTransport) Start(ctx context.Context) error {
	for {
		c.startedMu.Lock()
		if c.closed {
			c.startedMu.Unlock()
			return ErrTransportClosed
		}
		if c.started {
			c.startedMu.Unlock()
			return nil
		}
		if c.starting {
			done := c.startDone
			c.startedMu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		c.starting = true
		c.startDone = make(chan struct{})
		c.startedMu.Unlock()

		// Always create and register a session so server-to-client
		// notifications (progress, list-changed, resource updates, etc.) have
		// somewhere to land.
		//
		// RegisterSession runs the OnRegisterSession hooks synchronously, and a
		// hook may close this transport, so it runs outside startedMu. Close
		// only unregisters a session it can see, and the session is published
		// only after registration, so a Close that lands during registration
		// is caught by the recheck below and this call unregisters the session
		// itself. Until then the server may still address the session, but
		// nothing it sends reaches the client: forwarding starts only once the
		// session is published. Unregistering it from Close instead would run
		// the unregister hooks inside a registration hook that closes, and a
		// second unregister by ID could remove a later session with the same
		// ID. Concurrent Start calls wait on startDone for this attempt.
		session := server.NewInProcessSession(c.sessionID)
		err := c.server.RegisterSession(ctx, session)

		c.startedMu.Lock()
		closed := c.closed
		if err == nil && !closed {
			c.session = session
			c.started = true
		}
		c.starting = false
		close(c.startDone)
		c.startedMu.Unlock()

		if err != nil {
			return fmt.Errorf("failed to register session: %w", err)
		}
		if closed {
			c.server.UnregisterSession(context.Background(), c.sessionID)
			return ErrTransportClosed
		}

		go c.forwardNotifications()

		return nil
	}
}

// forwardNotifications drains the session's notification channel and
// forwards each notification to the registered client handler, mirroring
// the equivalent readResponses notification path in the stdio transport.
// Runs until Close() closes the done channel.
func (c *InProcessTransport) forwardNotifications() {
	notifications := c.session.ClientNotifications()
	for {
		select {
		case <-c.done:
			return
		case notification, ok := <-notifications:
			if !ok {
				return
			}
			c.notifyMu.RLock()
			handler := c.onNotification
			c.notifyMu.RUnlock()
			if handler != nil {
				handler(notification)
			}
		}
	}
}

func (c *InProcessTransport) SendRequest(ctx context.Context, request JSONRPCRequest) (*JSONRPCResponse, error) {
	session, closed := c.sessionState()
	if closed {
		return nil, ErrTransportClosed
	}

	requestBytes, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	requestBytes = append(requestBytes, '\n')

	// The request ends with the caller's context or with Close, whichever
	// comes first.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(c.closeCtx, cancel)
	defer stop()

	// Add session to context if available
	if session != nil {
		ctx = c.server.WithContext(ctx, session)
	}

	return inProcessResponse(c.server.HandleMessage(ctx, requestBytes))
}

// inProcessResponse builds the transport's response from the server's typed
// answer: the result is encoded once, as the client parses bytes, and an
// error is copied. Only an answer of another type goes through the wire
// encoding and back.
func inProcessResponse(message mcp.JSONRPCMessage) (*JSONRPCResponse, error) {
	switch m := message.(type) {
	case mcp.JSONRPCResponse:
		return inProcessResult(m.ID, m.Result)
	case *mcp.JSONRPCResponse:
		if m != nil {
			return inProcessResult(m.ID, m.Result)
		}
	case mcp.JSONRPCError:
		details := m.Error
		return &JSONRPCResponse{JSONRPC: mcp.JSONRPC_VERSION, ID: m.ID, Error: &details}, nil
	case *mcp.JSONRPCError:
		if m != nil {
			details := m.Error
			return &JSONRPCResponse{JSONRPC: mcp.JSONRPC_VERSION, ID: m.ID, Error: &details}, nil
		}
	}

	respByte, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal response message: %w", err)
	}
	var rpcResp JSONRPCResponse
	if err := json.Unmarshal(respByte, &rpcResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response message: %w", err)
	}
	return &rpcResp, nil
}

func inProcessResult(id mcp.RequestId, result any) (*JSONRPCResponse, error) {
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal response result: %w", err)
	}
	return NewJSONRPCResultResponse(id, encoded), nil
}

func (c *InProcessTransport) SendNotification(ctx context.Context, notification mcp.JSONRPCNotification) error {
	session, closed := c.sessionState()
	if closed {
		return ErrTransportClosed
	}

	notificationBytes, err := json.Marshal(notification)
	if err != nil {
		return fmt.Errorf("failed to marshal notification: %w", err)
	}
	notificationBytes = append(notificationBytes, '\n')

	// The session goes with the notification as it does with a request
	// (upstream #1067): a handler sees the client's session, and a cancel
	// finds the request it names, filed under that session.
	if session != nil {
		ctx = c.server.WithContext(ctx, session)
	}
	c.server.HandleMessage(ctx, notificationBytes)

	return nil
}

// sessionState is the session Start published, or nil before it, and
// whether Close has run: both read under the lock Start and Close write
// them under.
func (c *InProcessTransport) sessionState() (session *server.InProcessSession, closed bool) {
	c.startedMu.Lock()
	defer c.startedMu.Unlock()
	return c.session, c.closed
}

func (c *InProcessTransport) SetNotificationHandler(handler func(notification mcp.JSONRPCNotification)) {
	c.notifyMu.Lock()
	defer c.notifyMu.Unlock()
	c.onNotification = handler
}

func (c *InProcessTransport) Close() error {
	c.startedMu.Lock()
	c.closed = true
	session := c.session
	sessionID := c.sessionID
	c.startedMu.Unlock()

	c.closeOnce.Do(func() {
		close(c.done)
		c.closeCancel()
	})

	if session != nil {
		c.server.UnregisterSession(context.Background(), sessionID)
	}
	return nil
}

func (c *InProcessTransport) GetSessionId() string {
	return ""
}
