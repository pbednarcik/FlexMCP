package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// unencodableServer serves tools whose output cannot be encoded: "nan"
// returns a result holding NaN, "notify-then-nan" sends a notification first,
// and "nan-notification" sends a notification holding NaN, then a result
// that encodes.
func unencodableServer() *MCPServer {
	srv := NewMCPServer("p", "1.0.0", WithToolCapabilities(false))
	nan := func() (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{}, StructuredContent: map[string]any{"x": math.NaN()}}, nil
	}
	srv.AddTool(mcp.NewTool("nan"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nan()
	})
	srv.AddTool(mcp.NewTool("notify-then-nan"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		_ = ServerFromContext(ctx).SendNotificationToClient(ctx, "notifications/message", map[string]any{"level": "info", "data": "before"})
		return nan()
	})
	srv.AddTool(mcp.NewTool("nan-notification"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		_ = ServerFromContext(ctx).SendNotificationToClient(ctx, "notifications/message", map[string]any{"level": "info", "data": math.NaN()})
		return mcp.NewToolResultText("after"), nil
	})
	return srv
}

const unencodableInit = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`

func unencodableCall(id int, name string) string {
	return `{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,"method":"tools/call","params":{"name":"` + name + `"}}`
}

// wireMessage is one message as the client reads it.
type wireMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func readWireMessages(t *testing.T, lines []string) map[string]wireMessage {
	t.Helper()
	byID := map[string]wireMessage{}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m wireMessage
		require.NoError(t, json.Unmarshal([]byte(line), &m), "line %q", line)
		if m.ID != nil {
			byID[string(m.ID)] = m
		} else {
			byID[m.Method] = m
		}
	}
	return byID
}

func assertInternalError(t *testing.T, messages map[string]wireMessage, id string) {
	t.Helper()
	answer, ok := messages[id]
	require.True(t, ok, "request %s is answered: %v", id, messages)
	require.NotNil(t, answer.Error, "request %s is answered with an error", id)
	assert.Equal(t, mcp.INTERNAL_ERROR, answer.Error.Code)
	assert.Equal(t, internalErrorMessage, answer.Error.Message, "the cause stays in the server's log")
}

// A response that cannot be encoded, such as a tool result holding NaN, is
// answered with an internal error on stdio, and the server keeps serving; a
// notification that cannot be encoded is dropped, and what follows it is
// written.
func TestStdioAnswersAResponseThatCannotBeEncoded(t *testing.T) {
	stdio := NewStdioServer(unencodableServer())
	var logged strings.Builder
	stdio.SetErrorLogger(log.New(&logged, "", 0))

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- stdio.Listen(t.Context(), inR, outW) }()
	lines := make(chan string)
	go func() {
		scanner := bufio.NewScanner(outR)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	// exchange sends request and reads the n lines it is answered with.
	exchange := func(request string, n int) map[string]wireMessage {
		t.Helper()
		_, err := io.WriteString(inW, request+"\n")
		require.NoError(t, err)
		var read []string
		for range n {
			select {
			case line := <-lines:
				read = append(read, line)
			case <-time.After(5 * time.Second):
				t.Fatalf("no answer to %s; read %q", request, read)
			}
		}
		return readWireMessages(t, read)
	}

	exchange(unencodableInit, 1)
	assertInternalError(t, exchange(unencodableCall(2, "nan"), 1), "2")

	// The notification comes first, then the answer.
	messages := exchange(unencodableCall(3, "notify-then-nan"), 2)
	assert.Contains(t, messages, "notifications/message")
	assertInternalError(t, messages, "3")

	messages = exchange(unencodableCall(4, "nan-notification"), 1)
	require.Contains(t, messages, "4", "the notification is dropped and the result follows")
	assert.Nil(t, messages["4"].Error)
	assert.Contains(t, string(messages["4"].Result), "after")

	assert.Contains(t, exchange(`{"jsonrpc":"2.0","id":5,"method":"ping"}`, 1), "5")

	require.NoError(t, inW.Close())
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Listen did not end with its input")
	}
	assert.Contains(t, logged.String(), "NaN", "the cause is logged")
}

// The same on Streamable HTTP: a JSON answer that cannot be encoded is an
// internal error with a body, not a 200 with none, and on an SSE stream the
// answer follows the notifications sent before it; a notification that
// cannot be encoded leaves the stream open for the result.
func TestStreamableHTTPAnswersAResponseThatCannotBeEncoded(t *testing.T) {
	ts := NewTestStreamableHTTPServer(unencodableServer())
	defer ts.Close()
	post := func(session, body string) (*http.Response, []string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
			req.Header.Set("MCP-Protocol-Version", "2025-11-25")
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		var lines []string
		for line := range strings.SplitSeq(string(data), "\n") {
			if payload, ok := strings.CutPrefix(line, "data: "); ok {
				lines = append(lines, payload)
			} else if strings.HasPrefix(line, "{") {
				lines = append(lines, line)
			}
		}
		return resp, lines
	}

	resp, _ := post("", unencodableInit)
	session := resp.Header.Get("Mcp-Session-Id")

	resp, lines := post(session, unencodableCall(2, "nan"))
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	assertInternalError(t, readWireMessages(t, lines), "2")

	resp, lines = post(session, unencodableCall(3, "notify-then-nan"))
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))
	messages := readWireMessages(t, lines)
	assert.Contains(t, messages, "notifications/message")
	assertInternalError(t, messages, "3")

	_, lines = post(session, unencodableCall(4, "nan-notification"))
	messages = readWireMessages(t, lines)
	require.Contains(t, messages, "4", "the notification is dropped and the result follows")
	assert.Nil(t, messages["4"].Error)
	assert.Contains(t, string(messages["4"].Result), "after")
}

// statusRecorder records the statuses sent and the bytes written.
type statusRecorder struct {
	statuses []int
	body     strings.Builder
	err      error
}

func (r *statusRecorder) WriteHeader(status int) { r.statuses = append(r.statuses, status) }
func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	return r.body.Write(p)
}

// The encoder sends the status with the message's bytes, so a message that
// cannot be encoded sends neither, and its stand-in sends its own status; a
// failed write is the writer's error, not the encoding's.
func TestEncodeWriterSendsTheStatusOnlyWithTheMessage(t *testing.T) {
	var encoder encodeWriter
	unencodable := mcp.JSONRPCError{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      mcp.NewRequestId(7),
		Error:   mcp.NewJSONRPCErrorDetails(mcp.INVALID_PARAMS, "bad", math.NaN()),
	}

	var rec statusRecorder
	err := encoder.encode(&rec, &rec, http.StatusBadRequest, unencodable)
	require.ErrorIs(t, err, errNotEncoded)
	assert.Empty(t, rec.statuses, "no status for a message that was not sent")
	assert.Empty(t, rec.body.String())

	standIn, ok := answerInPlaceOf(unencodable)
	require.True(t, ok)
	require.NoError(t, encoder.encode(&rec, &rec, http.StatusOK, standIn))
	assert.Equal(t, []int{http.StatusOK}, rec.statuses)
	assert.JSONEq(t, `{"jsonrpc":"2.0","id":7,"error":{"code":-32603,"message":"internal error"}}`, rec.body.String())

	gone := statusRecorder{err: io.ErrClosedPipe}
	err = encoder.encode(&gone, nil, 0, standIn)
	require.ErrorIs(t, err, io.ErrClosedPipe)
	assert.NotErrorIs(t, err, errNotEncoded, "a failed write is the writer's")
}

// A response, a result or an error, is answered in place; a notification or
// a request to the client answers nothing.
func TestAnswerInPlaceOfOnlyAResponse(t *testing.T) {
	for name, message := range map[string]any{
		"result": mcp.NewJSONRPCResultResponse(mcp.NewRequestId("a"), map[string]any{}),
		"error":  mcp.JSONRPCError{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId("a")},
	} {
		standIn, ok := answerInPlaceOf(message)
		require.True(t, ok, name)
		assert.Equal(t, mcp.NewRequestId("a"), standIn.(mcp.JSONRPCError).ID, name)
		assert.Equal(t, mcp.INTERNAL_ERROR, standIn.(mcp.JSONRPCError).Error.Code, name)
	}
	for name, message := range map[string]any{
		"notification": mcp.JSONRPCNotification{JSONRPC: mcp.JSONRPC_VERSION},
		"request":      mcp.JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(1)},
	} {
		_, ok := answerInPlaceOf(message)
		assert.False(t, ok, name)
	}
}

// On protocol version 2026-07-28, which maps JSON-RPC errors onto HTTP
// statuses, the stand-in is an internal error, and so 200 OK.
func TestModernHTTPAnswersAResponseThatCannotBeEncoded(t *testing.T) {
	srv := NewMCPServer("p", "1.0.0", WithToolCapabilities(false))
	srv.AddTool(mcp.NewTool("echo"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{}, StructuredContent: map[string]any{"x": math.NaN()}}, nil
	})
	body, header := modernToolCall(t, map[string]any{})
	w := serveOne(NewStreamableHTTPServer(srv), body, header)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assertInternalError(t, readWireMessages(t, []string{strings.TrimSpace(w.Body.String())}), "1")
}
