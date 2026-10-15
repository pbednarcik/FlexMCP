package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
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
}

// NewInProcessTransport returns a transport that calls server directly.
func NewInProcessTransport(server *server.MCPServer) *InProcessTransport {
	return &InProcessTransport{
		server:    server,
		sessionID: server.GenerateInProcessSessionID(),
		done:      make(chan struct{}),
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
	requestBytes, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}
	requestBytes = append(requestBytes, '\n')

	// Add session to context if available
	if c.session != nil {
		ctx = c.server.WithContext(ctx, c.session)
	}

	respMessage := c.server.HandleMessage(ctx, requestBytes)
	respByte, err := json.Marshal(respMessage)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal response message: %w", err)
	}
	var rpcResp JSONRPCResponse
	err = json.Unmarshal(respByte, &rpcResp)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal response message: %w", err)
	}

	return &rpcResp, nil
}

func (c *InProcessTransport) SendNotification(ctx context.Context, notification mcp.JSONRPCNotification) error {
	notificationBytes, err := json.Marshal(notification)
	if err != nil {
		return fmt.Errorf("failed to marshal notification: %w", err)
	}
	notificationBytes = append(notificationBytes, '\n')

	// The session goes with the notification as it does with a request
	// (upstream #1067): a handler sees the client's session, and a cancel
	// finds the request it names, filed under that session.
	if c.session != nil {
		ctx = c.server.WithContext(ctx, c.session)
	}
	c.server.HandleMessage(ctx, notificationBytes)

	return nil
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
	})

	if session != nil {
		c.server.UnregisterSession(context.Background(), sessionID)
	}
	return nil
}

func (c *InProcessTransport) GetSessionId() string {
	return ""
}
