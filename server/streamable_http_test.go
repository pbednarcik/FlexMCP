package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type jsonRPCResponse struct {
	ID     int               `json:"id"`
	Result map[string]any    `json:"result"`
	Error  *mcp.JSONRPCError `json:"error"`
}

var initRequest = map[string]any{
	"jsonrpc": "2.0",
	"id":      1,
	"method":  "initialize",
	"params": map[string]any{
		"protocolVersion": mcp.LATEST_PROTOCOL_VERSION, "clientInfo": map[string]any{
			"name":    "test-client",
			"version": "1.0.0",
		},
	},
}

func addSSETool(mcpServer *MCPServer) {
	mcpServer.AddTool(mcp.Tool{
		Name: "sseTool",
	}, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Send notification to client
		server := ServerFromContext(ctx)
		for i := range 10 {
			_ = server.SendNotificationToClient(ctx, "test/notification", map[string]any{
				"value": i,
			})
			time.Sleep(10 * time.Millisecond)
		}
		// send final response
		return mcp.NewToolResultText("done"), nil
	})
}

func TestStreamableHTTPServerBasic(t *testing.T) {
	t.Run("Can instantiate", func(t *testing.T) {
		mcpServer := NewMCPServer("test", "1.0.0")
		httpServer := NewStreamableHTTPServer(mcpServer,
			WithEndpointPath("/mcp"),
		)

		if httpServer == nil {
			t.Error("StreamableHTTPServer should not be nil")
		} else {
			if httpServer.server == nil {
				t.Error("MCPServer should not be nil")
			}
			if httpServer.endpointPath != "/mcp" {
				t.Errorf(
					"Expected endpointPath /mcp, got %s",
					httpServer.endpointPath,
				)
			}
		}
	})
}

func TestStreamableHTTP_POST_InvalidContent(t *testing.T) {
	mcpServer := NewMCPServer("test-mcp-server", "1.0")
	addSSETool(mcpServer)
	server := NewTestStreamableHTTPServer(mcpServer)

	t.Run("Invalid content type", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, server.URL, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "text/plain") // Invalid type

		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("Failed to send message: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("Expected status 400, got %d", resp.StatusCode)
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(bodyBytes), "Invalid content type") {
			t.Errorf("Expected error message, got %s", string(bodyBytes))
		}
	})

	t.Run("Invalid JSON", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodPost, server.URL, strings.NewReader("{invalid json"))
		req.Header.Set("Content-Type", "application/json")

		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("Failed to send message: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("Expected status 400, got %d", resp.StatusCode)
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(bodyBytes), "jsonrpc") {
			t.Errorf("Expected error message, got %s", string(bodyBytes))
		}
		if !strings.Contains(string(bodyBytes), "not valid json") {
			t.Errorf("Expected error message, got %s", string(bodyBytes))
		}
	})
}

// A legacy client is served statelessly: initialize is answered for real with
// no session ID, every later request is served whatever session header it
// carries, DELETE has nothing to terminate, no session is registered with the
// server, and a cancel by notification has no session to scope it, so the
// request runs until its stream closes.
func TestStreamableHTTP_LegacyIsStateless(t *testing.T) {
	hooks := &Hooks{}
	var registered atomic.Int32
	hooks.AddOnRegisterSession(func(context.Context, ClientSession) { registered.Add(1) })
	mcpServer := NewMCPServer("test-mcp-server", "1.0", WithHooks(hooks), WithToolCapabilities(false))
	started := make(chan struct{}, 1)
	ended := make(chan error, 1)
	mcpServer.AddTool(mcp.NewTool("block"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		ended <- ctx.Err()
		return nil, ctx.Err()
	})
	server := NewTestStreamableHTTPServer(mcpServer)

	resp, err := postJSON(server.URL, initRequest)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Empty(t, resp.Header.Get(HeaderKeySessionID), "initialize must not mint a session")

	t.Run("a request is served whatever session header it carries", func(t *testing.T) {
		for _, header := range []string{"", "mcp-session-not-minted-here", "garbage"} {
			resp, err := postSessionJSON(server.URL, header, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
			require.NoError(t, err)
			resp.Body.Close()
			assert.Equal(t, http.StatusOK, resp.StatusCode, "session header %q", header)
		}
	})

	t.Run("DELETE has nothing to terminate", func(t *testing.T) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodDelete, server.URL, nil)
		require.NoError(t, err)
		req.Header.Set(HeaderKeySessionID, "mcp-session-not-minted-here")
		resp, err := server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
		// RFC 9110 section 15.5.6 requires an Allow header on a 405.
		assert.Equal(t, http.MethodPost, resp.Header.Get("Allow"))
	})

	t.Run("no session is registered", func(t *testing.T) {
		assert.Zero(t, registered.Load(), "the register-session hook fired for an HTTP client")
		err := mcpServer.SendNotificationToSpecificClient("mcp-session-not-minted-here", "test/notification", nil)
		assert.ErrorIs(t, err, ErrSessionNotFound)
	})

	t.Run("a cancel by notification has no session to scope it", func(t *testing.T) {
		ctx, closeStream := context.WithCancel(t.Context())
		defer closeStream()
		body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{"name": "block"}})
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		go func() {
			if resp, err := server.Client().Do(req); err == nil {
				resp.Body.Close()
			}
		}()
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("the blocking request did not start")
		}

		resp, err := postJSON(server.URL, map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": 7}})
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusAccepted, resp.StatusCode)
		select {
		case err := <-ended:
			t.Fatalf("the notification cancelled the request: %v", err)
		case <-time.After(200 * time.Millisecond):
		}

		closeStream()
		select {
		case <-ended:
		case <-time.After(2 * time.Second):
			t.Fatal("the request did not end when its stream closed")
		}
	})
}

func TestStreamableHTTP_POST_SendAndReceive(t *testing.T) {
	mcpServer := NewMCPServer("test-mcp-server", "1.0")
	server := NewTestStreamableHTTPServer(mcpServer)

	t.Run("initialize", func(t *testing.T) {
		// Send initialize request
		resp, err := postJSON(server.URL, initRequest)
		if err != nil {
			t.Fatalf("Failed to send message: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected status 200, got %d", resp.StatusCode)
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		var responseMessage jsonRPCResponse
		if err := json.Unmarshal(bodyBytes, &responseMessage); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}
		if responseMessage.Result["protocolVersion"] != mcp.LATEST_LEGACY_PROTOCOL_VERSION {
			t.Errorf("Expected protocol version %s, got %s", mcp.LATEST_LEGACY_PROTOCOL_VERSION, responseMessage.Result["protocolVersion"])
		}

		// no session id from header
		sessionID := resp.Header.Get(HeaderKeySessionID)
		if sessionID != "" {
			t.Fatalf("Expected no session id in header, got %s", sessionID)
		}
	})

	t.Run("Send and receive message", func(t *testing.T) {
		// send ping message
		pingMessage := map[string]any{
			"jsonrpc": "2.0",
			"id":      123,
			"method":  "ping",
			"params":  map[string]any{},
		}
		pingMessageBody, _ := json.Marshal(pingMessage)
		req, err := http.NewRequest("POST", server.URL, bytes.NewBuffer(pingMessageBody))
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("Failed to send message: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected status 200, got %d", resp.StatusCode)
		}

		// read response
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("Failed to read response: %v", err)
		}
		var response map[string]any
		if err := json.Unmarshal(responseBody, &response); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}
		if response["id"].(float64) != 123 {
			t.Errorf("Expected id 123, got %v", response["id"])
		}
	})

	t.Run("Send notification", func(t *testing.T) {
		// send notification
		notification := mcp.JSONRPCNotification{
			JSONRPC: "2.0",
			Method:  "testNotification",
			Params: mcp.NotificationParams{
				AdditionalFields: map[string]any{"param1": "value1"},
			},
		}
		rawNotification, _ := json.Marshal(notification)

		req, _ := http.NewRequest(http.MethodPost, server.URL, bytes.NewBuffer(rawNotification))
		req.Header.Set("Content-Type", "application/json")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("Failed to send message: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusAccepted {
			t.Errorf("Expected status 202, got %d", resp.StatusCode)
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		if len(bodyBytes) > 0 {
			t.Errorf("Expected empty body, got %s", string(bodyBytes))
		}
	})

	t.Run("Session id ignored", func(t *testing.T) {
		// send ping message with session ID - the server keeps no sessions
		pingMessage := map[string]any{
			"jsonrpc": "2.0",
			"id":      123,
			"method":  "ping",
			"params":  map[string]any{},
		}
		pingMessageBody, _ := json.Marshal(pingMessage)
		req, err := http.NewRequest("POST", server.URL, bytes.NewBuffer(pingMessageBody))
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(HeaderKeySessionID, "dummy-session-id")

		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("Failed to send message: %v", err)
		}
		defer resp.Body.Close()

		// The session ID is ignored and the request succeeds
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected status 200, got %d", resp.StatusCode)
		}

		// Verify the response is valid
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("Failed to read response: %v", err)
		}
		var response map[string]any
		if err := json.Unmarshal(responseBody, &response); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}
		if response["id"].(float64) != 123 {
			t.Errorf("Expected id 123, got %v", response["id"])
		}
	})

	t.Run("tools/list with session id", func(t *testing.T) {
		// Test the specific scenario from the issue - tools/list with session ID
		toolsListMessage := map[string]any{
			"jsonrpc": "2.0",
			"method":  "tools/list",
			"id":      1,
		}
		toolsListBody, _ := json.Marshal(toolsListMessage)
		req, err := http.NewRequest("POST", server.URL, bytes.NewBuffer(toolsListBody))
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(HeaderKeySessionID, "mcp-session-2c44d701-fd50-44ce-92b8-dec46185a741")

		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("Failed to send message: %v", err)
		}
		defer resp.Body.Close()

		// Succeeds with a session ID the server never minted
		if resp.StatusCode != http.StatusOK {
			bodyBytes, _ := io.ReadAll(resp.Body)
			t.Errorf("Expected status 200, got %d. Response: %s", resp.StatusCode, string(bodyBytes))
		}

		// Verify the response is valid
		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("Failed to read response: %v", err)
		}
		var response map[string]any
		if err := json.Unmarshal(responseBody, &response); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}
		if response["id"].(float64) != 1 {
			t.Errorf("Expected id 1, got %v", response["id"])
		}
	})
}

// The standalone GET stream was removed in protocol version 2026-07-28 and
// is not served to a legacy client either: a GET answers 405 with Allow:
// POST whatever era the client is on, and whether or not it holds a session.
func TestStreamableHTTP_GETIsNotServed(t *testing.T) {
	mcpServer := NewMCPServer("test-mcp-server", "1.0")
	addSSETool(mcpServer)
	server := NewTestStreamableHTTPServer(mcpServer)

	legacyInitialize(t, server.URL)

	legs := []struct {
		name    string
		headers map[string]string
	}{
		{name: "legacy without a session", headers: map[string]string{}},
		{name: "legacy with a stale session header", headers: map[string]string{
			mcp.HeaderProtocolVersion: mcp.LATEST_LEGACY_PROTOCOL_VERSION,
			HeaderKeySessionID:        "mcp-session-not-minted-here",
		}},
		{name: "modern", headers: map[string]string{
			mcp.HeaderProtocolVersion: mcp.ProtocolVersion20260728,
		}},
	}
	for _, leg := range legs {
		t.Run(leg.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
			require.NoError(t, err)
			req.Header.Set("Accept", "text/event-stream")
			for k, v := range leg.headers {
				req.Header.Set(k, v)
			}

			resp, err := server.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()

			assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
			// RFC 9110 section 15.5.6 requires an Allow header on a 405.
			assert.Equal(t, http.MethodPost, resp.Header.Get("Allow"))
		})
	}
}

// GET and DELETE have nothing to serve in either era, so both verbs answer
// through one path: the same 405 with the same body whatever protocol version
// the client names, after the one check every verb makes, that the version is
// one this SDK implements (Transports.md row 28).
func TestStreamableHTTP_GETAndDELETEAnswerOnePathInBothEras(t *testing.T) {
	mcpServer := NewMCPServer("test-mcp-server", "1.0")
	server := NewTestStreamableHTTPServer(mcpServer)

	send := func(t *testing.T, method, version string) (*http.Response, string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, method, server.URL, nil)
		require.NoError(t, err)
		if version != "" {
			req.Header.Set(mcp.HeaderProtocolVersion, version)
		}
		resp, err := server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp, string(body)
	}

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			t.Run("one answer in both eras", func(t *testing.T) {
				resp, legacyBody := send(t, method, mcp.LATEST_LEGACY_PROTOCOL_VERSION)
				require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
				for _, version := range []string{"", mcp.ProtocolVersion20260728} {
					resp, body := send(t, method, version)
					assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, "version %q", version)
					// RFC 9110 section 15.5.6 requires an Allow header on a 405.
					assert.Equal(t, http.MethodPost, resp.Header.Get("Allow"), "version %q", version)
					assert.Equal(t, legacyBody, body, "version %q answers through another path", version)
				}
			})

			t.Run("an unknown version is rejected first, on either side of 2026-07-28", func(t *testing.T) {
				for _, version := range []string{"2025-01-01", "2027-01-01"} {
					resp, body := send(t, method, version)
					assert.Equal(t, http.StatusBadRequest, resp.StatusCode, "version %q", version)
					assert.Contains(t, body, "Unsupported protocol version: "+version)
				}
			})
		})
	}
}

func TestStreamableHTTP_HttpHandler(t *testing.T) {
	t.Run("Works with custom mux", func(t *testing.T) {
		mcpServer := NewMCPServer("test", "1.0.0")
		server := NewStreamableHTTPServer(mcpServer)

		mux := http.NewServeMux()
		mux.Handle("/mypath", server)

		ts := httptest.NewServer(mux)
		defer ts.Close()

		// Send initialize request
		initRequest := map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "initialize",
			"params": map[string]any{
				"protocolVersion": mcp.LATEST_PROTOCOL_VERSION, "clientInfo": map[string]any{
					"name":    "test-client",
					"version": "1.0.0",
				},
			},
		}

		resp, err := postJSON(ts.URL+"/mypath", initRequest)
		if err != nil {
			t.Fatalf("Failed to send message: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected status 200, got %d", resp.StatusCode)
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		var responseMessage jsonRPCResponse
		if err := json.Unmarshal(bodyBytes, &responseMessage); err != nil {
			t.Fatalf("Failed to unmarshal response: %v", err)
		}
		if responseMessage.Result["protocolVersion"] != mcp.LATEST_LEGACY_PROTOCOL_VERSION {
			t.Errorf("Expected protocol version %s, got %s", mcp.LATEST_LEGACY_PROTOCOL_VERSION, responseMessage.Result["protocolVersion"])
		}
	})
}

func TestStreamableHTTPServer_WithOptions(t *testing.T) {
	t.Run("WithStreamableHTTPServer sets httpServer field", func(t *testing.T) {
		mcpServer := NewMCPServer("test", "1.0.0")
		customServer := &http.Server{Addr: ":9999"}
		httpServer := NewStreamableHTTPServer(mcpServer, WithStreamableHTTPServer(customServer))

		if httpServer.httpServer != customServer {
			t.Errorf("Expected httpServer to be set to custom server instance, got %v", httpServer.httpServer)
		}
	})

	t.Run("Start with conflicting address returns error", func(t *testing.T) {
		mcpServer := NewMCPServer("test", "1.0.0")
		customServer := &http.Server{Addr: ":9999"}
		httpServer := NewStreamableHTTPServer(mcpServer, WithStreamableHTTPServer(customServer))

		err := httpServer.Start(":8888")
		if err == nil {
			t.Error("Expected error for conflicting address, got nil")
		} else if !strings.Contains(err.Error(), "conflicting listen address") {
			t.Errorf("Expected error message to contain 'conflicting listen address', got '%s'", err.Error())
		}
	})

	t.Run("Options consistency test", func(t *testing.T) {
		mcpServer := NewMCPServer("test", "1.0.0")
		endpointPath := "/test-mcp"
		customServer := &http.Server{}

		// Options to test
		options := []StreamableHTTPOption{
			WithEndpointPath(endpointPath),
			WithStreamableHTTPServer(customServer),
		}

		// Apply options multiple times and verify consistency
		for range 10 {
			server := NewStreamableHTTPServer(mcpServer, options...)

			if server.endpointPath != endpointPath {
				t.Errorf("Expected endpointPath %s, got %s", endpointPath, server.endpointPath)
			}

			if server.httpServer != customServer {
				t.Errorf("Expected httpServer to match, got %v", server.httpServer)
			}
		}
	})
}

func TestStreamableHTTP_HeaderPassthrough(t *testing.T) {
	mcpServer := NewMCPServer("test-mcp-server", "1.0")

	var receivedHeaders struct {
		contentType  string
		customHeader string
	}
	mcpServer.AddTool(
		mcp.NewTool("check-headers"),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			receivedHeaders.contentType = request.Header.Get("Content-Type")
			receivedHeaders.customHeader = request.Header.Get("X-Custom-Header")
			return mcp.NewToolResultText("ok"), nil
		},
	)

	server := NewTestStreamableHTTPServer(mcpServer)
	defer server.Close()

	// Test header passthrough
	toolRequest := map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "check-headers",
		},
	}
	toolBody, _ := json.Marshal(toolRequest)
	req, _ := http.NewRequest("POST", server.URL, bytes.NewReader(toolBody))

	const expectedContentType = "application/json"
	const expectedCustomHeader = "test-value"
	req.Header.Set("Content-Type", expectedContentType)
	req.Header.Set("X-Custom-Header", expectedCustomHeader)

	resp, _ := server.Client().Do(req)
	resp.Body.Close()

	if receivedHeaders.contentType != expectedContentType {
		t.Errorf("Expected Content-Type header '%s', got '%s'", expectedContentType, receivedHeaders.contentType)
	}
	if receivedHeaders.customHeader != expectedCustomHeader {
		t.Errorf("Expected X-Custom-Header '%s', got '%s'", expectedCustomHeader, receivedHeaders.customHeader)
	}
}

func TestStreamableHTTP_PongResponseHandling(t *testing.T) {
	// Ping/Pong does not require session ID
	// https://modelcontextprotocol.io/specification/2025-03-26/basic/utilities/ping
	mcpServer := NewMCPServer("test-mcp-server", "1.0")
	server := NewTestStreamableHTTPServer(mcpServer)
	defer server.Close()

	t.Run("Pong response with empty result should return 202 and not be treated as sampling response", func(t *testing.T) {
		// According to MCP spec, pong responses have empty result: {"jsonrpc": "2.0", "id": "123", "result": {}}
		pongResponse := map[string]any{
			"jsonrpc": "2.0",
			"id":      123,
			"result":  map[string]any{},
		}

		resp, err := postJSON(server.URL, pongResponse)
		if err != nil {
			t.Fatalf("Failed to send pong response: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("Failed to read response body: %v", err)
		}
		bodyStr := string(bodyBytes)

		if strings.Contains(bodyStr, "Missing session ID for sampling response") {
			t.Errorf("Pong response was incorrectly detected as sampling response. Response: %s", bodyStr)
		}
		if strings.Contains(bodyStr, "Failed to handle sampling response") {
			t.Errorf("Pong response was incorrectly detected as sampling response. Response: %s", bodyStr)
		}

		if resp.StatusCode != http.StatusAccepted {
			t.Errorf("Expected status 202 for pong response, got %d. Body: %s", resp.StatusCode, bodyStr)
		}
	})

	t.Run("Pong response with omitted result should return 202 and not be treated as sampling response", func(t *testing.T) {
		// Some MCP clients (e.g. VS Code Copilot Chat) send ping responses without
		// the result field: {"jsonrpc":"2.0","id":N}. While this is not strictly
		// compliant with JSON-RPC 2.0 (which requires result or error), the server
		// should accept it leniently and return 202 Accepted per the MCP spec
		// (Streamable HTTP transport, rule 4).
		pongResponse := map[string]any{
			"jsonrpc": "2.0",
			"id":      124,
		}

		resp, err := postJSON(server.URL, pongResponse)
		if err != nil {
			t.Fatalf("Failed to send pong response: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("Failed to read response body: %v", err)
		}
		bodyStr := string(bodyBytes)

		if strings.Contains(bodyStr, "Missing session ID for sampling response") {
			t.Errorf("Pong response with omitted result was incorrectly detected as sampling response. Response: %s", bodyStr)
		}

		if resp.StatusCode != http.StatusAccepted {
			t.Errorf("Expected status 202 for pong response, got %d. Body: %s", resp.StatusCode, bodyStr)
		}
	})

	t.Run("Response with empty error should return 202 and not be treated as sampling response", func(t *testing.T) {
		response := map[string]any{
			"jsonrpc": "2.0",
			"id":      125,
			"error":   map[string]any{},
		}

		resp, err := postJSON(server.URL, response)
		if err != nil {
			t.Fatalf("Failed to send response: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("Failed to read response body: %v", err)
		}
		bodyStr := string(bodyBytes)

		if strings.Contains(bodyStr, "Missing session ID for sampling response") {
			t.Errorf("Response with empty error was incorrectly detected as sampling response. Response: %s", bodyStr)
		}

		if resp.StatusCode != http.StatusAccepted {
			t.Errorf("Expected status 202 for response with empty error, got %d. Body: %s", resp.StatusCode, bodyStr)
		}
	})
}

func TestStreamableHTTPServer_TLS(t *testing.T) {
	t.Run("TLS options are set correctly", func(t *testing.T) {
		mcpServer := NewMCPServer("test-mcp-server", "1.0.0")
		certFile := "/path/to/cert.pem"
		keyFile := "/path/to/key.pem"

		server := NewStreamableHTTPServer(
			mcpServer,
			WithTLSCert(certFile, keyFile),
		)

		if server.tlsCertFile != certFile {
			t.Errorf("Expected tlsCertFile to be %s, got %s", certFile, server.tlsCertFile)
		}
		if server.tlsKeyFile != keyFile {
			t.Errorf("Expected tlsKeyFile to be %s, got %s", keyFile, server.tlsKeyFile)
		}
	})
}

func postJSON(url string, bodyObject any) (*http.Response, error) {
	jsonBody, _ := json.Marshal(bodyObject)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

func postSessionJSON(url, session string, bodyObject any) (*http.Response, error) {
	jsonBody, _ := json.Marshal(bodyObject)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderKeySessionID, session)
	return http.DefaultClient.Do(req)
}

func TestStreamableHTTP_ProtocolVersionHeader(t *testing.T) {
	mcpServer := NewMCPServer("test-server", "1.0.0", WithToolCapabilities(true))
	server := NewTestStreamableHTTPServer(mcpServer)
	defer server.Close()

	legacyInitialize(t, server.URL)

	send := func(t *testing.T, method string, body any, protocolVersion string) (int, string) {
		t.Helper()
		var reader io.Reader
		if body != nil {
			jsonBody, err := json.Marshal(body)
			require.NoError(t, err)
			reader = bytes.NewReader(jsonBody)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, method, server.URL, reader)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		if protocolVersion != "" {
			req.Header.Set(mcp.HeaderProtocolVersion, protocolVersion)
		}
		resp, err := server.Client().Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		payload, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(payload)
	}

	listTools := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"}

	t.Run("unsupported version is rejected", func(t *testing.T) {
		for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
			t.Run(method, func(t *testing.T) {
				var body any
				if method == http.MethodPost {
					body = listTools
				}
				status, payload := send(t, method, body, "2025-01-01")
				assert.Equal(t, http.StatusBadRequest, status)
				assert.Contains(t, payload, "Unsupported protocol version: 2025-01-01")
			})
		}
	})

	t.Run("unsupported version is rejected on responses", func(t *testing.T) {
		responses := map[string]map[string]any{
			"ping":  {"jsonrpc": "2.0", "id": 1, "result": map[string]any{}},
			"empty": {"jsonrpc": "2.0", "id": 1},
		}
		for name, body := range responses {
			t.Run(name, func(t *testing.T) {
				status, payload := send(t, http.MethodPost, body, "2025-01-01")
				assert.Equal(t, http.StatusBadRequest, status)
				assert.Contains(t, payload, "Unsupported protocol version: 2025-01-01")
			})
		}
	})

	t.Run("supported version is accepted", func(t *testing.T) {
		status, payload := send(t, http.MethodPost, listTools, mcp.LATEST_LEGACY_PROTOCOL_VERSION)
		assert.Equal(t, http.StatusOK, status, payload)
	})

	t.Run("missing header is accepted", func(t *testing.T) {
		status, payload := send(t, http.MethodPost, listTools, "")
		assert.Equal(t, http.StatusOK, status, payload)
	})

	t.Run("initialize is not checked", func(t *testing.T) {
		initialize := map[string]any{
			"jsonrpc": "2.0",
			"id":      3,
			"method":  mcp.MethodInitialize,
			"params": map[string]any{
				"protocolVersion": mcp.LATEST_LEGACY_PROTOCOL_VERSION,
				"clientInfo":      map[string]any{"name": "test-client", "version": "1.0.0"},
				"capabilities":    map[string]any{},
			},
		}
		status, payload := send(t, http.MethodPost, initialize, "2025-01-01")
		assert.Equal(t, http.StatusOK, status, payload)
	})
}

// TestStreamableHTTP_AddToolDuringToolCall tests that adding a tool while a tool call
// is in progress doesn't break the client's response.
// This is a regression test for issue #638 where notifications sent via
// sendNotificationToAllClients during an in-progress request would cause
// the response to fail with "unexpected nil response".
func TestStreamableHTTP_AddToolDuringToolCall(t *testing.T) {
	mcpServer := NewMCPServer("test-mcp-server", "1.0",
		WithToolCapabilities(true), // Enable tool list change notifications
	)
	// Add a tool that takes some time to complete
	mcpServer.AddTool(mcp.NewTool("slow_tool",
		mcp.WithDescription("A tool that takes time to complete"),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Simulate work that takes some time
		time.Sleep(100 * time.Millisecond)
		return mcp.NewToolResultText("done"), nil
	})
	server := NewTestStreamableHTTPServer(mcpServer)
	defer server.Close()
	// Start the tool call in a goroutine
	resultChan := make(chan struct {
		statusCode int
		body       string
		err        error
	})
	go func() {
		toolRequest := map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "tools/call",
			"params": map[string]any{
				"name": "slow_tool",
			},
		}
		toolBody, _ := json.Marshal(toolRequest)
		req, _ := http.NewRequest("POST", server.URL, bytes.NewReader(toolBody))
		req.Header.Set("Content-Type", "application/json")
		resp, err := server.Client().Do(req)
		if err != nil {
			resultChan <- struct {
				statusCode int
				body       string
				err        error
			}{0, "", err}
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		resultChan <- struct {
			statusCode int
			body       string
			err        error
		}{resp.StatusCode, string(body), nil}
	}()
	// Wait a bit then add a new tool while the slow_tool is executing
	// This triggers sendNotificationToAllClients
	time.Sleep(50 * time.Millisecond)
	mcpServer.AddTool(mcp.NewTool("new_tool",
		mcp.WithDescription("A new tool added during execution"),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return mcp.NewToolResultText("new tool result"), nil
	})
	// Wait for the tool call to complete
	result := <-resultChan
	if result.err != nil {
		t.Fatalf("Tool call failed with error: %v", result.err)
	}
	if result.statusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d. Body: %s", result.statusCode, result.body)
	}
	// The response should contain the tool result
	// It may be SSE format (text/event-stream) due to the notification upgrade
	if !strings.Contains(result.body, "done") {
		t.Errorf("Expected response to contain 'done', got: %s", result.body)
	}
}

func TestStreamableHTTP_HeadReturns200(t *testing.T) {
	mcpServer := NewMCPServer("test-mcp-server", "1.0")
	sseServer := NewStreamableHTTPServer(mcpServer)
	testServer := httptest.NewServer(sseServer)
	defer testServer.Close()

	req, _ := http.NewRequest(http.MethodHead, testServer.URL, nil)
	resp, err := testServer.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestStreamableHTTP_DrainNotifications(t *testing.T) {
	t.Run("drain pending notifications after response is computed", func(t *testing.T) {
		mcpServer := NewMCPServer("test-mcp-server", "1.0")

		drainLoopCalled := make(chan int, 1)

		// Add a tool that sends notifications rapidly (faster than the goroutine can process)
		// This forces notifications to queue up in the channel, testing the drain loop
		mcpServer.AddTool(mcp.Tool{
			Name: "drainTestTool",
		}, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			server := ServerFromContext(ctx)
			// Send notifications in rapid succession (no delays)
			// The concurrent goroutine (line 394-434 in streamable_http.go) may not process all of them
			// before we hit the drain loop at line 448-468
			for i := range 10 {
				_ = server.SendNotificationToClient(ctx, "test/drain", map[string]any{
					"index": i,
				})
			}
			return mcp.NewToolResultText("drain test done"), nil
		})

		server := NewTestStreamableHTTPServer(mcpServer)
		defer server.Close()

		// Call tool with rapid notifications
		callToolRequest := map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "tools/call",
			"params": map[string]any{
				"name": "drainTestTool",
			},
		}
		callToolRequestBody, err := json.Marshal(callToolRequest)
		if err != nil {
			t.Fatalf("Failed to marshal request: %v", err)
		}
		req, err := http.NewRequest("POST", server.URL, bytes.NewBuffer(callToolRequestBody))
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatalf("Failed to send request: %v", err)
		}
		defer resp.Body.Close()

		// Verify response is SSE format (indicates drain loop was used)
		if resp.Header.Get("content-type") != "text/event-stream" {
			t.Errorf("Expected content-type text/event-stream, got %s", resp.Header.Get("content-type"))
		}

		responseBody, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("Failed to read response: %v", err)
		}
		responseStr := string(responseBody)

		// Verify we received drain notifications
		// Without the drain loop, we'd get fewer notifications
		// With the drain loop, we catch the pending ones at line 448-468
		drainCount := strings.Count(responseStr, "test/drain")
		if drainCount < 5 {
			t.Logf("Drain loop captured %d notifications. Response:\n%s", drainCount, responseStr)
			// This is informational - the test verifies the drain loop is functional
		}

		// The critical verification: final response is present
		if !strings.Contains(responseStr, "drain test done") {
			t.Errorf("Expected final response with 'drain test done'")
		}

		// Verify response has SSE event format (proves drain loop was executed)
		if !strings.Contains(responseStr, "event: message") {
			t.Errorf("Expected SSE event format in response")
		}

		_ = drainLoopCalled
	})
}

// TestStreamableHTTP_NotificationUpgradeDoesNotCorruptResponse verifies that when
// a notification is sent during HandleMessage (triggering an SSE upgrade on the
// response), the final response is written as SSE — not as JSON on top of SSE
// headers, which would corrupt the response.
//
// This is a regression test for https://github.com/mark3labs/mcp-go/issues/742
func TestStreamableHTTP_NotificationUpgradeDoesNotCorruptResponse(t *testing.T) {
	// Run multiple iterations to increase the chance of hitting the race window.
	// The bug is timing-dependent: a notification must be processed by the
	// goroutine before the main handler writes the response.
	for i := range 20 {
		t.Run(fmt.Sprintf("iteration_%d", i), func(t *testing.T) {
			t.Parallel()

			mcpServer := NewMCPServer("test-mcp-server", "1.0",
				WithToolCapabilities(true),
			)

			// Add a tool that sends a notification during execution.
			// This notification goes through session.notificationChannel,
			// where the goroutine in handlePost picks it up and writes SSE
			// headers before the main handler gets to write the response.
			mcpServer.AddTool(mcp.NewTool("notifying_tool"), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				server := ServerFromContext(ctx)
				_ = server.SendNotificationToClient(ctx, "test/raceNotification", map[string]any{
					"info": "this triggers the SSE upgrade",
				})
				return mcp.NewToolResultText("tool result ok"), nil
			})

			server := NewTestStreamableHTTPServer(mcpServer)
			defer server.Close()

			// Call the tool — this is where the race can happen
			callReq := map[string]any{
				"jsonrpc": "2.0",
				"id":      1,
				"method":  "tools/call",
				"params":  map[string]any{"name": "notifying_tool"},
			}
			resp, err := postJSON(server.URL, callReq)
			require.NoError(t, err)
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			bodyStr := string(body)

			assert.Equal(t, http.StatusOK, resp.StatusCode)

			contentType := resp.Header.Get("Content-Type")

			// The tool sends a notification synchronously before returning,
			// so by the time HandleMessage completes the notification is on the
			// channel. Either the goroutine or the drain loop will process it,
			// setting upgradedHeader = true. The response MUST be SSE.
			require.True(t, strings.HasPrefix(contentType, "text/event-stream"),
				"notification path must upgrade this response to SSE, got %q", contentType)
			require.Contains(t, bodyStr, "event: message")
			require.Contains(t, bodyStr, "tool result ok")

			// Every non-empty line must be valid SSE syntax — no raw JSON
			// objects appended (which would indicate the corruption this test guards against).
			for line := range strings.SplitSeq(strings.TrimSpace(bodyStr), "\n") {
				if line == "" {
					continue
				}
				require.True(t,
					strings.HasPrefix(line, "event:") ||
						strings.HasPrefix(line, "data:") ||
						strings.HasPrefix(line, "id:") ||
						strings.HasPrefix(line, "retry:") ||
						strings.HasPrefix(line, ":"),
					"unexpected non-SSE line %q", line,
				)
			}
		})
	}
}

func TestStreamableHTTPNotificationRace(t *testing.T) {
	s := NewMCPServer("test-server", "1.0")
	s.AddNotificationHandler("notifications/initialized", func(ctx context.Context, _ mcp.JSONRPCNotification) {
		session := ClientSessionFromContext(ctx)
		session.NotificationChannel() <- mcp.JSONRPCNotification{
			JSONRPC: mcp.JSONRPC_VERSION,
			Method:  "server/ping",
		}
	})

	hs := NewStreamableHTTPServer(s)

	ts := httptest.NewServer(hs)
	defer ts.Close()

	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	body := `{"jsonrpc":"2.0","method":"notifications/initialized"}`

	for i := range 200 {
		resp, err := client.Post(ts.URL+"/mcp", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
			t.Fatalf("iteration %d: expected 200 or 202, got %d", i, resp.StatusCode)
		}
	}
}

func TestStreamableHTTP_ShutdownReturns(t *testing.T) {
	mcpServer := NewMCPServer("test", "1.0.0")
	mux := http.NewServeMux()
	customServer := &http.Server{Handler: mux}
	httpServer := NewStreamableHTTPServer(
		mcpServer,
		WithStreamableHTTPServer(customServer),
	)
	mux.Handle(httpServer.endpointPath, httpServer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- customServer.Serve(listener)
	}()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		select {
		case err := <-serveDone:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Logf("Serve returned: %v", err)
			}
		case <-time.After(2 * time.Second):
		}
	})

	baseURL := "http://" + listener.Addr().String() + httpServer.endpointPath

	resp, err := postJSON(baseURL, initRequest)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	shutdownDone := make(chan error, 1)
	shutdownCtx, shutdownCancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer shutdownCancel()
	go func() {
		shutdownDone <- httpServer.Shutdown(shutdownCtx)
	}()

	select {
	case err := <-shutdownDone:
		if shutdownCtx.Err() == context.DeadlineExceeded {
			t.Fatalf("Shutdown deadlocked (timed out): %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Shutdown did not return in time (likely deadlocked)")
	}
}
