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
	closed         bool
	startedMu      sync.Mutex

	done      chan struct{}
	closeOnce sync.Once
}

// InProcessOption configures an InProcessTransport.
type InProcessOption func(*InProcessTransport)

func NewInProcessTransport(server *server.MCPServer) *InProcessTransport {
	return &InProcessTransport{
		server:    server,
		sessionID: server.GenerateInProcessSessionID(),
		done:      make(chan struct{}),
	}
}

func NewInProcessTransportWithOptions(server *server.MCPServer, opts ...InProcessOption) *InProcessTransport {
	t := NewInProcessTransport(server)
	for _, opt := range opts {
		opt(t)
	}
	return t
}

func (c *InProcessTransport) Start(ctx context.Context) error {
	c.startedMu.Lock()
	if c.closed {
		c.startedMu.Unlock()
		return ErrTransportClosed
	}
	if c.started {
		c.startedMu.Unlock()
		return nil
	}

	// Always create and register a session so server-to-client notifications
	// (progress, list-changed, resource updates, etc.) have somewhere to land.
	//
	// Registration and the c.session/c.started assignments all happen under a
	// single startedMu hold so that Start and Close are mutually exclusive:
	// a concurrent Close either runs entirely before this section (observed
	// via c.closed above, so Start bails out before registering anything) or
	// entirely after (Close will see c.started and c.session set, and will
	// unregister the session). There is no window where a session is
	// registered but Close skips unregistering it. RegisterSession does a
	// sync.Map store plus runs synchronous OnRegisterSession hooks; neither
	// re-enters this transport's lock (safe from deadlock); slow user hooks
	// only extend the lock hold.
	session := server.NewInProcessSession(c.sessionID)
	if err := c.server.RegisterSession(ctx, session); err != nil {
		c.startedMu.Unlock()
		return fmt.Errorf("failed to register session: %w", err)
	}

	c.session = session
	c.started = true
	c.startedMu.Unlock()

	go c.forwardNotifications()

	return nil
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
