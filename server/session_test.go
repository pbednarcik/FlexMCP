package server

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mark3labs/mcp-go/mcp"
)

// sessionTestClient implements the basic ClientSession interface for testing
type sessionTestClient struct {
	sessionID           string
	notificationChannel chan mcp.JSONRPCNotification
	initialized         bool
}

func (f sessionTestClient) SessionID() string {
	return f.sessionID
}

func (f sessionTestClient) NotificationChannel() chan<- mcp.JSONRPCNotification {
	return f.notificationChannel
}

// Initialize marks the session as initialized
// This implementation properly sets the initialized flag to true
// as required by the interface contract
func (f *sessionTestClient) Initialize() {
	f.initialized = true
}

// Initialized returns whether the session has been initialized
func (f sessionTestClient) Initialized() bool {
	return f.initialized
}

// sessionTestClientWithClientInfo implements the SessionWithClientInfo interface for testing
type sessionTestClientWithClientInfo struct {
	sessionID           string
	notificationChannel chan mcp.JSONRPCNotification
	initialized         bool
	clientInfo          atomic.Value
	clientCapabilities  atomic.Value
}

func (f *sessionTestClientWithClientInfo) SessionID() string {
	return f.sessionID
}

func (f *sessionTestClientWithClientInfo) NotificationChannel() chan<- mcp.JSONRPCNotification {
	return f.notificationChannel
}

func (f *sessionTestClientWithClientInfo) Initialize() {
	f.initialized = true
}

func (f *sessionTestClientWithClientInfo) Initialized() bool {
	return f.initialized
}

func (f *sessionTestClientWithClientInfo) GetClientInfo() mcp.Implementation {
	if value := f.clientInfo.Load(); value != nil {
		if clientInfo, ok := value.(mcp.Implementation); ok {
			return clientInfo
		}
	}
	return mcp.Implementation{}
}

func (f *sessionTestClientWithClientInfo) SetClientInfo(clientInfo mcp.Implementation) {
	f.clientInfo.Store(clientInfo)
}

func (f *sessionTestClientWithClientInfo) GetClientCapabilities() mcp.ClientCapabilities {
	if value := f.clientCapabilities.Load(); value != nil {
		if clientCapabilities, ok := value.(mcp.ClientCapabilities); ok {
			return clientCapabilities
		}
	}
	return mcp.ClientCapabilities{}
}

func (f *sessionTestClientWithClientInfo) SetClientCapabilities(clientCapabilities mcp.ClientCapabilities) {
	f.clientCapabilities.Store(clientCapabilities)
}

// Verify that all implementations satisfy their respective interfaces
var (
	_ ClientSession         = (*sessionTestClient)(nil)
	_ SessionWithClientInfo = (*sessionTestClientWithClientInfo)(nil)
)

func TestMCPServer_PromptFiltering(t *testing.T) {
	// Create a filter that filters prompts by prefix
	filterByPrefix := func(prefix string) PromptFilterFunc {
		return func(ctx context.Context, prompts []mcp.Prompt) []mcp.Prompt {
			var filtered []mcp.Prompt
			for _, prompt := range prompts {
				if len(prompt.Name) >= len(prefix) && prompt.Name[:len(prefix)] == prefix {
					filtered = append(filtered, prompt)
				}
			}
			return filtered
		}
	}

	// Create a server with a prompt filter
	server := NewMCPServer("test-server", "1.0.0",
		WithPromptCapabilities(true),
		WithPromptFilter(filterByPrefix("allow-")),
	)

	// Add prompts with different prefixes
	noopHandler := func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{}, nil
	}
	server.AddPrompt(mcp.Prompt{Name: "allow-prompt-1"}, noopHandler)
	server.AddPrompt(mcp.Prompt{Name: "allow-prompt-2"}, noopHandler)
	server.AddPrompt(mcp.Prompt{Name: "deny-prompt-1"}, noopHandler)
	server.AddPrompt(mcp.Prompt{Name: "deny-prompt-2"}, noopHandler)

	// List prompts
	ctx := t.Context()
	response := server.HandleMessage(ctx, []byte(`{
		"jsonrpc": "2.0",
		"id": 1,
		"method": "prompts/list"
	}`))
	resp, ok := response.(mcp.JSONRPCResponse)
	require.True(t, ok)

	result, ok := resp.Result.(mcp.ListPromptsResult)
	require.True(t, ok)

	// Should only include prompts with the "allow-" prefix
	assert.Len(t, result.Prompts, 2)
	for _, prompt := range result.Prompts {
		assert.True(t, len(prompt.Name) >= 6 && prompt.Name[:6] == "allow-",
			"Prompt should start with 'allow-', got: %s", prompt.Name)
	}
}

func TestMCPServer_MultiplePromptFilters(t *testing.T) {
	// First filter: keep only prompts starting with "a"
	filterA := func(ctx context.Context, prompts []mcp.Prompt) []mcp.Prompt {
		var filtered []mcp.Prompt
		for _, p := range prompts {
			if len(p.Name) > 0 && p.Name[0] == 'a' {
				filtered = append(filtered, p)
			}
		}
		return filtered
	}
	// Second filter: keep only prompts containing "keep"
	filterKeep := func(ctx context.Context, prompts []mcp.Prompt) []mcp.Prompt {
		var filtered []mcp.Prompt
		for _, p := range prompts {
			for i := 0; i+4 <= len(p.Name); i++ {
				if p.Name[i:i+4] == "keep" {
					filtered = append(filtered, p)
					break
				}
			}
		}
		return filtered
	}

	server := NewMCPServer("test-server", "1.0.0",
		WithPromptCapabilities(true),
		WithPromptFilter(PromptFilterFunc(filterA)),
		WithPromptFilter(PromptFilterFunc(filterKeep)),
	)

	noopHandler := func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{}, nil
	}
	server.AddPrompt(mcp.Prompt{Name: "a-keep-this"}, noopHandler)
	server.AddPrompt(mcp.Prompt{Name: "a-remove-this"}, noopHandler)
	server.AddPrompt(mcp.Prompt{Name: "b-keep-this"}, noopHandler)

	ctx := t.Context()
	response := server.HandleMessage(ctx, []byte(`{
		"jsonrpc": "2.0",
		"id": 1,
		"method": "prompts/list"
	}`))
	resp, ok := response.(mcp.JSONRPCResponse)
	require.True(t, ok)

	result, ok := resp.Result.(mcp.ListPromptsResult)
	require.True(t, ok)

	// Only "a-keep-this" should survive both filters
	assert.Len(t, result.Prompts, 1)
	assert.Equal(t, "a-keep-this", result.Prompts[0].Name)
}

func TestMCPServer_PromptHandlerMiddleware(t *testing.T) {
	server := NewMCPServer("test-server", "1.0.0",
		WithPromptCapabilities(true),
		WithPromptHandlerMiddleware(func(next PromptHandlerFunc) PromptHandlerFunc {
			return func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				result, err := next(ctx, req)
				if err != nil {
					return nil, err
				}
				// Middleware appends a message
				result.Messages = append(result.Messages, mcp.PromptMessage{
					Role: mcp.RoleAssistant,
					Content: mcp.TextContent{
						Type: "text",
						Text: "added-by-middleware",
					},
				})
				return result, nil
			}
		}),
	)

	server.AddPrompt(mcp.Prompt{Name: "test-prompt"}, func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{
			Messages: []mcp.PromptMessage{
				{
					Role:    mcp.RoleUser,
					Content: mcp.TextContent{Type: "text", Text: "original"},
				},
			},
		}, nil
	})

	ctx := t.Context()
	response := server.HandleMessage(ctx, []byte(`{
		"jsonrpc": "2.0",
		"id": 1,
		"method": "prompts/get",
		"params": {"name": "test-prompt"}
	}`))
	resp, ok := response.(mcp.JSONRPCResponse)
	require.True(t, ok)

	result, ok := resp.Result.(mcp.GetPromptResult)
	require.True(t, ok)

	assert.Len(t, result.Messages, 2)
	// Verify original message is first
	tc, ok := result.Messages[0].Content.(mcp.TextContent)
	require.True(t, ok)
	assert.Equal(t, "original", tc.Text)
	// Verify middleware-added message is second
	tc, ok = result.Messages[1].Content.(mcp.TextContent)
	require.True(t, ok)
	assert.Equal(t, "added-by-middleware", tc.Text)
}

func TestMCPServer_MultiplePromptHandlerMiddlewares(t *testing.T) {
	var order []string

	makeMiddleware := func(name string) PromptHandlerMiddleware {
		return func(next PromptHandlerFunc) PromptHandlerFunc {
			return func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				order = append(order, name+"-before")
				result, err := next(ctx, req)
				order = append(order, name+"-after")
				return result, err
			}
		}
	}

	server := NewMCPServer("test-server", "1.0.0",
		WithPromptCapabilities(true),
		WithPromptHandlerMiddleware(makeMiddleware("first")),
		WithPromptHandlerMiddleware(makeMiddleware("second")),
	)

	server.AddPrompt(mcp.Prompt{Name: "test-prompt"}, func(ctx context.Context, req mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		order = append(order, "handler")
		return &mcp.GetPromptResult{}, nil
	})

	ctx := t.Context()
	response := server.HandleMessage(ctx, []byte(`{
		"jsonrpc": "2.0",
		"id": 1,
		"method": "prompts/get",
		"params": {"name": "test-prompt"}
	}`))
	_, ok := response.(mcp.JSONRPCResponse)
	require.True(t, ok)

	// Middlewares applied in reverse order means first-registered wraps outermost
	assert.Equal(t, []string{"first-before", "second-before", "handler", "second-after", "first-after"}, order)
}

func TestMCPServer_SendNotificationToSpecificClient(t *testing.T) {
	server := NewMCPServer("test-server", "1.0.0")

	session1Chan := make(chan mcp.JSONRPCNotification, 10)
	session1 := &sessionTestClient{
		sessionID:           "session-1",
		notificationChannel: session1Chan,
	}
	session1.Initialize()

	session2Chan := make(chan mcp.JSONRPCNotification, 10)
	session2 := &sessionTestClient{
		sessionID:           "session-2",
		notificationChannel: session2Chan,
	}
	session2.Initialize()

	session3 := &sessionTestClient{
		sessionID:           "session-3",
		notificationChannel: make(chan mcp.JSONRPCNotification, 10),
		initialized:         false, // Not initialized - deliberately not calling Initialize()
	}

	// Register sessions
	err := server.RegisterSession(t.Context(), session1)
	require.NoError(t, err)
	err = server.RegisterSession(t.Context(), session2)
	require.NoError(t, err)
	err = server.RegisterSession(t.Context(), session3)
	require.NoError(t, err)

	// Send notification to session 1
	err = server.SendNotificationToSpecificClient(session1.SessionID(), "test-method", map[string]any{
		"data": "test-data",
	})
	require.NoError(t, err)

	// Check that only session 1 received the notification
	select {
	case notification := <-session1Chan:
		assert.Equal(t, "test-method", notification.Method)
		assert.Equal(t, "test-data", notification.Params.AdditionalFields["data"])
	case <-time.After(100 * time.Millisecond):
		t.Error("Expected notification not received by session 1")
	}

	// Verify session 2 did not receive notification
	select {
	case notification := <-session2Chan:
		t.Errorf("Unexpected notification received by session 2: %v", notification)
	case <-time.After(100 * time.Millisecond):
		// Expected, no notification for session 2
	}

	// Test sending to non-existent session
	err = server.SendNotificationToSpecificClient("non-existent", "test-method", nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")

	// Test sending to uninitialized session
	err = server.SendNotificationToSpecificClient(session3.SessionID(), "test-method", nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not properly initialized")
}

func TestMCPServer_NotificationChannelBlocked(t *testing.T) {
	// Set up a hooks object to capture error notifications
	var mu sync.Mutex
	errorCaptured := false
	errorSessionID := ""
	errorMethod := ""

	hooks := &Hooks{}
	hooks.AddOnError(func(ctx context.Context, id any, method mcp.MCPMethod, message any, err error) {
		mu.Lock()
		defer mu.Unlock()

		errorCaptured = true
		// Extract session ID and method from the error message metadata
		if msgMap, ok := message.(map[string]any); ok {
			if sid, ok := msgMap["sessionID"].(string); ok {
				errorSessionID = sid
			}
			if m, ok := msgMap["method"].(string); ok {
				errorMethod = m
			}
		}
		// Verify the error is a notification channel blocked error
		assert.True(t, errors.Is(err, ErrNotificationChannelBlocked))
	})

	// Create a server with hooks
	server := NewMCPServer("test-server", "1.0.0", WithHooks(hooks))

	// Create a session with a very small buffer that will get blocked
	smallBufferChan := make(chan mcp.JSONRPCNotification, 1)
	session := &sessionTestClient{
		sessionID:           "blocked-session",
		notificationChannel: smallBufferChan,
	}
	session.Initialize()

	// Register the session
	err := server.RegisterSession(t.Context(), session)
	require.NoError(t, err)

	// Fill the buffer first to ensure it gets blocked
	err = server.SendNotificationToSpecificClient(session.SessionID(), "first-message", nil)
	require.NoError(t, err)

	// This will cause the buffer to block
	err = server.SendNotificationToSpecificClient(session.SessionID(), "blocked-message", nil)
	assert.Error(t, err)
	assert.Equal(t, ErrNotificationChannelBlocked, err)

	// Wait a bit for the goroutine to execute
	time.Sleep(10 * time.Millisecond)

	// Verify the error was logged via hooks
	mu.Lock()
	localErrorCaptured := errorCaptured
	localErrorSessionID := errorSessionID
	localErrorMethod := errorMethod
	mu.Unlock()

	assert.True(t, localErrorCaptured, "Error hook should have been called")
	assert.Equal(t, "blocked-session", localErrorSessionID, "Session ID should be captured in the error hook")
	assert.Equal(t, "blocked-message", localErrorMethod, "Method should be captured in the error hook")

	// Also test SendNotificationToAllClients with a blocked channel
	// Reset the captured data
	mu.Lock()
	errorCaptured = false
	errorSessionID = ""
	errorMethod = ""
	mu.Unlock()

	// Send to all clients (which includes our blocked one)
	server.SendNotificationToAllClients("broadcast-message", nil)

	// Wait a bit for the goroutine to execute
	time.Sleep(10 * time.Millisecond)

	// Verify the error was logged via hooks
	mu.Lock()
	localErrorCaptured = errorCaptured
	localErrorSessionID = errorSessionID
	localErrorMethod = errorMethod
	mu.Unlock()

	assert.True(t, localErrorCaptured, "Error hook should have been called for broadcast")
	assert.Equal(t, "blocked-session", localErrorSessionID, "Session ID should be captured in the error hook")
	assert.Equal(t, "broadcast-message", localErrorMethod, "Method should be captured in the error hook")
}

func TestSessionWithClientInfo_Integration(t *testing.T) {
	server := NewMCPServer("test-server", "1.0.0")

	session := &sessionTestClientWithClientInfo{
		sessionID:           "session-1",
		notificationChannel: make(chan mcp.JSONRPCNotification, 10),
		initialized:         false,
	}

	err := server.RegisterSession(t.Context(), session)
	require.NoError(t, err)

	clientInfo := mcp.Implementation{
		Name:    "test-client",
		Version: "1.0.0",
	}

	clientCapability := mcp.ClientCapabilities{
		Sampling: &mcp.SamplingCapability{},
	}

	initRequest := mcp.InitializeRequest{}
	initRequest.Params.ClientInfo = clientInfo
	initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initRequest.Params.Capabilities = clientCapability

	sessionCtx := server.WithContext(t.Context(), session)

	// Retrieve the session from context
	retrievedSession := ClientSessionFromContext(sessionCtx)
	require.NotNil(t, retrievedSession, "Session should be available from context")
	assert.Equal(t, session.SessionID(), retrievedSession.SessionID(), "Session ID should match")

	result, reqErr := server.handleInitialize(sessionCtx, 1, initRequest)
	require.Nil(t, reqErr)
	require.NotNil(t, result)

	// Check if the session can be cast to SessionWithClientInfo
	sessionWithClientInfo, ok := retrievedSession.(SessionWithClientInfo)
	require.True(t, ok, "Session should implement SessionWithClientInfo")

	assert.True(t, sessionWithClientInfo.Initialized(), "Session should be initialized")

	storedClientInfo := sessionWithClientInfo.GetClientInfo()

	assert.Equal(t, clientInfo.Name, storedClientInfo.Name, "Client name should match")
	assert.Equal(t, clientInfo.Version, storedClientInfo.Version, "Client version should match")

	storedClientCapabilities := sessionWithClientInfo.GetClientCapabilities()

	assert.Equal(t, clientCapability, storedClientCapabilities, "Client capability should match")
}

// New test function to cover log notification functionality
// queued returns the notification waiting on ch, if any. Log sends are
// synchronous, so nothing arrives after the send returns.
func queued(ch chan mcp.JSONRPCNotification) (mcp.JSONRPCNotification, bool) {
	select {
	case n := <-ch:
		return n, true
	default:
		return mcp.JSONRPCNotification{}, false
	}
}

func TestMCPServer_SendLogMessageToClient(t *testing.T) {
	server := NewMCPServer("test-server", "1.0.0", WithLogging())
	ch := make(chan mcp.JSONRPCNotification, 10)
	session := &sessionTestClient{sessionID: "session-1", notificationChannel: ch}
	session.Initialize()
	require.NoError(t, server.RegisterSession(t.Context(), session))
	legacyCtx := server.WithContext(t.Context(), session)

	// A legacy client cannot choose a level: error and worse reach it.
	tests := []struct {
		level mcp.LoggingLevel
		sent  bool
	}{
		{mcp.LoggingLevelDebug, false},
		{mcp.LoggingLevelWarning, false},
		{mcp.LoggingLevelError, true},
		{mcp.LoggingLevelEmergency, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.level), func(t *testing.T) {
			notification := mcp.NewLoggingMessageNotification(tt.level, "test-logger", "test message")
			require.NoError(t, server.SendLogMessageToClient(legacyCtx, notification))
			notif, sent := queued(ch)
			require.Equal(t, tt.sent, sent)
			if sent {
				assert.Equal(t, "notifications/message", notif.Method)
				assert.Equal(t, tt.level, notif.Params.AdditionalFields["level"])
			}
		})
	}

	t.Run("uninitialized session", func(t *testing.T) {
		uninit := &sessionTestClient{sessionID: "uninit", notificationChannel: make(chan mcp.JSONRPCNotification, 1)}
		notification := mcp.NewLoggingMessageNotification(mcp.LoggingLevelError, "test-logger", "test message")
		err := server.SendLogMessageToClient(server.WithContext(t.Context(), uninit), notification)
		assert.ErrorIs(t, err, ErrNotificationNotInitialized)
	})
}

func TestMCPServer_LegacySetLevelIsAcknowledgedNotKept(t *testing.T) {
	server := NewMCPServer("test-server", "1.0.0", WithLogging())
	ch := make(chan mcp.JSONRPCNotification, 10)
	session := &sessionTestClient{sessionID: "session-1", notificationChannel: ch}
	session.Initialize()
	require.NoError(t, server.RegisterSession(t.Context(), session))
	legacyCtx := server.WithContext(t.Context(), session)

	setLevel := func(level string) mcp.JSONRPCMessage {
		return server.HandleMessage(legacyCtx, json.RawMessage(
			`{"jsonrpc":"2.0","id":1,"method":"logging/setLevel","params":{"level":"`+level+`"}}`))
	}

	resp, ok := setLevel("debug").(mcp.JSONRPCResponse)
	require.True(t, ok, "a valid level is acknowledged")
	assert.IsType(t, mcp.EmptyResult{}, resp.Result)

	notification := mcp.NewLoggingMessageNotification(mcp.LoggingLevelDebug, "test-logger", "test message")
	require.NoError(t, server.SendLogMessageToClient(legacyCtx, notification))
	_, sent := queued(ch)
	assert.False(t, sent, "the level is not kept: the threshold stays legacyLogLevel")

	errResp, ok := setLevel("loud").(mcp.JSONRPCError)
	require.True(t, ok, "an unknown level is rejected")
	assert.Equal(t, mcp.INVALID_PARAMS, errResp.Error.Code)
}

func TestMCPServer_SendLogMessageToSpecificClient(t *testing.T) {
	server := NewMCPServer("test-server", "1.0.0", WithLogging())
	ch := make(chan mcp.JSONRPCNotification, 10)
	session := &sessionTestClient{sessionID: "session-1", notificationChannel: ch}
	session.Initialize()
	uninit := &sessionTestClient{sessionID: "uninit", notificationChannel: make(chan mcp.JSONRPCNotification, 1)}
	require.NoError(t, server.RegisterSession(t.Context(), session))
	require.NoError(t, server.RegisterSession(t.Context(), uninit))

	t.Run("below error is dropped", func(t *testing.T) {
		notification := mcp.NewLoggingMessageNotification(mcp.LoggingLevelWarning, "test-logger", "test message")
		require.NoError(t, server.SendLogMessageToSpecificClient(session.SessionID(), notification))
		_, sent := queued(ch)
		assert.False(t, sent)
	})

	t.Run("error is sent", func(t *testing.T) {
		notification := mcp.NewLoggingMessageNotification(mcp.LoggingLevelError, "test-logger", "test message")
		require.NoError(t, server.SendLogMessageToSpecificClient(session.SessionID(), notification))
		notif, sent := queued(ch)
		require.True(t, sent)
		assert.Equal(t, "notifications/message", notif.Method)
		assert.Equal(t, mcp.LoggingLevelError, notif.Params.AdditionalFields["level"])
		assert.Equal(t, "test-logger", notif.Params.AdditionalFields["logger"])
		assert.Equal(t, "test message", notif.Params.AdditionalFields["data"])
	})

	t.Run("unknown session", func(t *testing.T) {
		notification := mcp.NewLoggingMessageNotification(mcp.LoggingLevelError, "test-logger", "test message")
		assert.ErrorIs(t, server.SendLogMessageToSpecificClient("non-existent", notification), ErrSessionNotFound)
	})

	t.Run("uninitialized session", func(t *testing.T) {
		notification := mcp.NewLoggingMessageNotification(mcp.LoggingLevelError, "test-logger", "test message")
		assert.ErrorIs(t, server.SendLogMessageToSpecificClient(uninit.SessionID(), notification), ErrSessionNotInitialized)
	})
}

func TestMCPServer_LoggingNotificationFormat(t *testing.T) {
	server := NewMCPServer("test-server", "1.0.0", WithLogging())
	ch := make(chan mcp.JSONRPCNotification, 10)
	session := &sessionTestClient{sessionID: "session-1", notificationChannel: ch}
	session.Initialize()
	require.NoError(t, server.RegisterSession(t.Context(), session))

	testCases := []struct {
		name     string
		data     any
		expected any
	}{
		{name: "string data", data: "simple log message", expected: "simple log message"},
		{name: "structured data", data: map[string]any{"key": "value", "num": 42}, expected: map[string]any{"key": "value", "num": 42}},
		{name: "error data", data: errors.New("error message"), expected: errors.New("error message")},
		{name: "nil data", data: nil, expected: nil},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			notification := mcp.NewLoggingMessageNotification(mcp.LoggingLevelError, "test-logger", tc.data)
			require.NoError(t, server.SendLogMessageToSpecificClient(session.SessionID(), notification))

			notif, sent := queued(ch)
			require.True(t, sent)
			assert.Equal(t, "notifications/message", notif.Method)
			assert.Equal(t, mcp.LoggingLevelError, notif.Params.AdditionalFields["level"])
			assert.Equal(t, "test-logger", notif.Params.AdditionalFields["logger"])

			dataField := notif.Params.AdditionalFields["data"]
			switch expected := tc.expected.(type) {
			case string:
				assert.Equal(t, expected, dataField)
			case map[string]any:
				assert.IsType(t, map[string]any{}, dataField)
				dataMap := dataField.(map[string]any)
				for k, v := range expected {
					assert.Equal(t, v, dataMap[k])
				}
			case nil:
				assert.Nil(t, dataField)
			}
		})
	}
}
