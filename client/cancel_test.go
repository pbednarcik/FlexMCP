package client

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pbednarcik/flexmcp/client/transport"
	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// connectOverPipes connects a client to s over in-memory stdio pipes, on the
// given protocol version.
func connectOverPipes(t *testing.T, s *server.MCPServer, version string) *Client {
	t.Helper()
	ctx := t.Context()
	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()
	t.Cleanup(func() {
		_ = serverWrite.Close()
		_ = serverRead.Close()
	})
	go func() { _ = server.NewStdioServer(s).Listen(ctx, serverRead, serverWrite) }()

	c := NewClient(transport.NewIO(clientRead, clientWrite, io.NopCloser(strings.NewReader(""))))
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Start(ctx))
	result, err := c.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{
		ProtocolVersion: version,
		ClientInfo:      mcp.Implementation{Name: "test-client", Version: "1.0.0"},
	}})
	require.NoError(t, err)
	require.Equal(t, version, result.ProtocolVersion)
	return c
}

// A request whose context ends is cancelled on the server too. Over stdio
// that takes a notifications/cancelled, which protocol version 2026-07-28
// requires there; without it the server kept working on a request nobody
// was waiting for.
func TestClient_CancelsAbandonedRequestOverStdio(t *testing.T) {
	for _, version := range []string{mcp.ProtocolVersion20260728, mcp.LATEST_LEGACY_PROTOCOL_VERSION} {
		t.Run(version, func(t *testing.T) {
			stopped := make(chan struct{})
			s := server.NewMCPServer("test-server", "1.0.0")
			s.AddTool(mcp.NewTool("slow"), func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				select {
				case <-ctx.Done():
					close(stopped)
				case <-time.After(10 * time.Second):
				}
				return mcp.NewToolResultText("done"), nil
			})
			c := connectOverPipes(t, s, version)

			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			_, err := c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "slow"}})
			require.ErrorIs(t, err, context.DeadlineExceeded)

			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("the server kept running the tool after the client gave up on it")
			}
		})
	}
}

// pipePeer plays the server end of a stdio connection by hand.
type pipePeer struct {
	t        *testing.T
	client   *Client
	write    *io.PipeWriter
	messages chan map[string]any
}

func newPipePeer(t *testing.T, options ...ClientOption) *pipePeer {
	t.Helper()
	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()
	t.Cleanup(func() {
		_ = serverWrite.Close()
		_ = serverRead.Close()
	})
	p := &pipePeer{t: t, write: serverWrite, messages: make(chan map[string]any, 10)}
	options = append([]ClientOption{WithLegacyProtocolOnly()}, options...)
	p.client = NewClient(transport.NewIO(clientRead, clientWrite, io.NopCloser(strings.NewReader(""))), options...)
	t.Cleanup(func() { _ = p.client.Close() })
	require.NoError(t, p.client.Start(t.Context()))
	go func() {
		scanner := bufio.NewScanner(serverRead)
		for scanner.Scan() {
			var message map[string]any
			if json.Unmarshal(scanner.Bytes(), &message) == nil {
				p.messages <- message
			}
		}
	}()
	return p
}

// next returns the next message the client sent.
func (p *pipePeer) next() map[string]any {
	p.t.Helper()
	select {
	case m := <-p.messages:
		return m
	case <-time.After(5 * time.Second):
		p.t.Fatal("no message from the client")
		return nil
	}
}

func (p *pipePeer) respond(id any, result string) {
	p.t.Helper()
	response, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": json.RawMessage(result)})
	require.NoError(p.t, err)
	_, err = p.write.Write(append(response, '\n'))
	require.NoError(p.t, err)
}

// initialize completes the handshake with the client.
func (p *pipePeer) initialize() {
	p.t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := p.client.Initialize(p.t.Context(), mcp.InitializeRequest{})
		done <- err
	}()
	initialize := p.next()
	require.Equal(p.t, "initialize", initialize["method"])
	p.respond(initialize["id"], `{"protocolVersion":"`+mcp.LATEST_LEGACY_PROTOCOL_VERSION+`","capabilities":{},"serverInfo":{"name":"s","version":"1"}}`)
	require.NoError(p.t, <-done)
	require.Equal(p.t, "notifications/initialized", p.next()["method"])
}

// expectNothingElse checks that the next message is a ping the test sends
// after waiting a little, so that nothing else was sent in the meantime.
func (p *pipePeer) expectNothingElse() {
	p.t.Helper()
	time.Sleep(100 * time.Millisecond)
	go func() { _ = p.client.Ping(p.t.Context()) }()
	assert.Equal(p.t, "ping", p.next()["method"])
}

// The cancellation names the request. Initialize, a request that was never
// sent, and a task-augmented request are not cancelled.
func TestClient_CancellationNotificationOnTheWire(t *testing.T) {
	p := newPipePeer(t)

	// An initialize that times out is not cancelled.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err := p.client.Initialize(ctx, mcp.InitializeRequest{})
	require.Error(t, err)
	assert.Equal(t, "initialize", p.next()["method"])
	p.initialize()

	// A tool call that times out is cancelled by id, with the reason.
	ctx, cancel = context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = p.client.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "slow"}})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	call := p.next()
	require.Equal(t, "tools/call", call["method"])
	cancelled := p.next()
	assert.Equal(t, "notifications/cancelled", cancelled["method"])
	params, _ := cancelled["params"].(map[string]any)
	assert.Equal(t, call["id"], params["requestId"])
	assert.Equal(t, context.DeadlineExceeded.Error(), params["reason"])

	// A call whose context had ended before it was sent isn't sent or
	// cancelled.
	done, stop := context.WithCancel(t.Context())
	stop()
	_, err = p.client.CallTool(done, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "slow"}})
	require.ErrorIs(t, err, context.Canceled)
	p.expectNothingElse()

	// A task-augmented call is cancelled with tasks/cancel, not this.
	ctx, cancel = context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = p.client.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "slow", Task: &mcp.TaskParams{}}})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, "tools/call", p.next()["method"])
	p.expectNothingElse()
}

// A peer that stops reading doesn't hold up a caller whose context ended:
// the cancellation is sent in the background.
func TestClient_CancellationDoesNotBlockTheCaller(t *testing.T) {
	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()
	t.Cleanup(func() {
		_ = serverWrite.Close()
		_ = serverRead.Close()
	})
	c := NewClient(transport.NewIO(clientRead, clientWrite, io.NopCloser(strings.NewReader(""))), WithLegacyProtocolOnly())
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Start(t.Context()))
	reader := bufio.NewReader(serverRead)
	read := func() map[string]any {
		line, err := reader.ReadBytes('\n')
		require.NoError(t, err)
		var message map[string]any
		require.NoError(t, json.Unmarshal(line, &message))
		return message
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.Initialize(t.Context(), mcp.InitializeRequest{})
		done <- err
	}()
	initialize := read()
	response, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": initialize["id"], "result": json.RawMessage(
		`{"protocolVersion":"` + mcp.LATEST_LEGACY_PROTOCOL_VERSION + `","capabilities":{},"serverInfo":{"name":"s","version":"1"}}`)})
	require.NoError(t, err)
	_, err = serverWrite.Write(append(response, '\n'))
	require.NoError(t, err)
	require.Equal(t, "notifications/initialized", read()["method"])
	require.NoError(t, <-done)

	// Read the tool call, then nothing more.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	returned := make(chan error, 1)
	go func() {
		_, err := c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "slow"}})
		returned <- err
	}()
	require.Equal(t, "tools/call", read()["method"])
	select {
	case err := <-returned:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(5 * time.Second):
		t.Fatal("the call didn't return while the cancellation couldn't be written")
	}
}

// Over Streamable HTTP on protocol version 2026-07-28 the transport ends the
// request's stream, which is the cancellation signal there, so no
// notifications/cancelled is sent.
func TestClient_NoCancellationNotificationOverHTTP(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&message)
		mu.Lock()
		methods = append(methods, message.Method)
		mu.Unlock()
		switch message.Method {
		case string(mcp.MethodServerDiscover):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(message.ID) + `,"result":{"supportedVersions":["` + mcp.ProtocolVersion20260728 + `"],"capabilities":{},"serverInfo":{"name":"s","version":"1"}}}`))
		case string(mcp.MethodToolsCall):
			<-r.Context().Done()
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer httpServer.Close()

	trans, err := transport.NewStreamableHTTP(httpServer.URL)
	require.NoError(t, err)
	c := NewClient(trans)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Start(t.Context()))
	result, err := c.Initialize(t.Context(), mcp.InitializeRequest{})
	require.NoError(t, err)
	require.Equal(t, mcp.ProtocolVersion20260728, result.ProtocolVersion)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "slow"}})
	require.Error(t, err)

	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	assert.NotContains(t, methods, string(mcp.MethodNotificationCancelled))
}

// Before protocol version 2026-07-28 a closed stream is not a cancellation on
// Streamable HTTP: the server is told not to take it as one, so the client
// sends notifications/cancelled there as it does on stdio.
func TestClient_CancellationNotificationOverLegacyHTTP(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	cancelled := make(chan struct{}, 1)
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&message)
		mu.Lock()
		methods = append(methods, message.Method)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch message.Method {
		case string(mcp.MethodServerDiscover):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(message.ID) + `,"error":{"code":-32601,"message":"method not found"}}`))
		case string(mcp.MethodInitialize):
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(message.ID) + `,"result":{"protocolVersion":"` + mcp.LATEST_LEGACY_PROTOCOL_VERSION + `","capabilities":{},"serverInfo":{"name":"s","version":"1"}}}`))
		case string(mcp.MethodToolsCall):
			<-r.Context().Done()
		case string(mcp.MethodNotificationCancelled):
			cancelled <- struct{}{}
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer httpServer.Close()

	trans, err := transport.NewStreamableHTTP(httpServer.URL)
	require.NoError(t, err)
	c := NewClient(trans)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Start(t.Context()))
	result, err := c.Initialize(t.Context(), mcp.InitializeRequest{})
	require.NoError(t, err)
	require.Equal(t, mcp.LATEST_LEGACY_PROTOCOL_VERSION, result.ProtocolVersion)

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "slow"}})
	require.ErrorIs(t, err, context.DeadlineExceeded)

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("no notifications/cancelled reached the server; it saw %v", methods)
	}
}

// A request the SSE transport's response timeout ends, while the caller's
// context is still live, is cancelled on the server as one whose context
// ended is: the server would otherwise keep working on it for no one.
func TestClient_CancelsARequestTheSSEResponseTimeoutEnds(t *testing.T) {
	srv := newLegacySSEServer(t, string(mcp.MethodToolsCall))
	c, err := NewSSEMCPClient(srv.URL+"/sse", transport.WithResponseTimeout(200*time.Millisecond))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.Start(t.Context()))
	_, err = c.Initialize(t.Context(), mcp.InitializeRequest{})
	require.NoError(t, err)

	_, err = c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "slow"}})
	require.Error(t, err)
	require.NoError(t, t.Context().Err())

	var call, cancelled *postedMessage
	require.Eventually(t, func() bool {
		for _, m := range srv.postedMessages() {
			switch m.Method {
			case string(mcp.MethodToolsCall):
				call = &m
			case string(mcp.MethodNotificationCancelled):
				cancelled = &m
			}
		}
		return call != nil && cancelled != nil
	}, 2*time.Second, 10*time.Millisecond, "no notifications/cancelled reached the server")
	var params struct {
		RequestID json.RawMessage `json:"requestId"`
		Reason    string          `json:"reason"`
	}
	require.NoError(t, json.Unmarshal(cancelled.Params, &params))
	assert.JSONEq(t, string(call.ID), string(params.RequestID))
	assert.Equal(t, context.DeadlineExceeded.Error(), params.Reason)
}

// tooLargeBound is the bound the client of tooLargeAnswerServer reads under.
const tooLargeBound = 1024

// tooLargeAnswer is how tooLargeAnswerServer answers tools/call past
// tooLargeBound.
type tooLargeAnswer int

const (
	// eventOverTheBound is an event that large on the request's stream, the
	// work still running.
	eventOverTheBound tooLargeAnswer = iota
	// jsonOverTheBound is a JSON answer that large, of no stated length.
	jsonOverTheBound
	// statedJSONOverTheBound is a JSON answer that large, its length stated.
	statedJSONOverTheBound
)

// tooLargeAnswerServer is a Streamable HTTP server that negotiates version
// and answers tools/call as answer says. It sends the ID of the call on
// callID, and each notifications/cancelled it receives on cancelled.
type tooLargeAnswerServer struct {
	*httptest.Server
	callID    chan json.RawMessage
	cancelled chan postedMessage
}

func newTooLargeAnswerServer(t *testing.T, version string, answer tooLargeAnswer) *tooLargeAnswerServer {
	t.Helper()
	s := &tooLargeAnswerServer{
		callID:    make(chan json.RawMessage, 1),
		cancelled: make(chan postedMessage, 1),
	}
	payload := strings.Repeat("x", 2*tooLargeBound)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var message postedMessage
		_ = json.NewDecoder(r.Body).Decode(&message)
		switch message.Method {
		case string(mcp.MethodServerDiscover):
			w.Header().Set("Content-Type", "application/json")
			if version != mcp.ProtocolVersion20260728 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(message.ID) + `,"error":{"code":-32601,"message":"method not found"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(message.ID) + `,"result":{"supportedVersions":["` + version + `"],"capabilities":{},"serverInfo":{"name":"s","version":"1"}}}`))
		case string(mcp.MethodInitialize):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(message.ID) + `,"result":{"protocolVersion":"` + version + `","capabilities":{},"serverInfo":{"name":"s","version":"1"}}}`))
		case string(mcp.MethodToolsCall):
			select {
			case s.callID <- message.ID:
			default:
			}
			switch answer {
			case eventOverTheBound:
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(`data: {"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info","data":"` + payload + `"}}` + "\n\n"))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			case jsonOverTheBound, statedJSONOverTheBound:
				body := `{"jsonrpc":"2.0","id":` + string(message.ID) + `,"result":{"content":[{"type":"text","text":"` + payload + `"}]}}`
				w.Header().Set("Content-Type", "application/json")
				if answer == statedJSONOverTheBound {
					w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				} else {
					// Headers flushed before the body state no length.
					w.(http.Flusher).Flush()
				}
				_, _ = w.Write([]byte(body))
			}
		case string(mcp.MethodNotificationCancelled):
			select {
			case s.cancelled <- message:
			default:
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// A request whose event stream broke off on the bound is one the client no
// longer waits for, and its work may still be running. Before protocol
// version 2026-07-28 the transport ending the stream is not a cancellation,
// so the server is sent notifications/cancelled for it; on that version the
// end of the stream is the cancellation, and nothing is sent. A JSON answer
// over the bound is the request's one response, so its work is finished, and
// nothing is sent for it in either era.
func TestClient_CancelsARequestWhoseAnswerIsTooLarge(t *testing.T) {
	for _, row := range []struct {
		name    string
		version string
		answer  tooLargeAnswer
		cancels bool
	}{
		{"legacy era event over the bound", mcp.LATEST_LEGACY_PROTOCOL_VERSION, eventOverTheBound, true},
		{"legacy era JSON answer over the bound", mcp.LATEST_LEGACY_PROTOCOL_VERSION, jsonOverTheBound, false},
		{"legacy era JSON answer of a stated length over the bound", mcp.LATEST_LEGACY_PROTOCOL_VERSION, statedJSONOverTheBound, false},
		{"modern era event over the bound", mcp.ProtocolVersion20260728, eventOverTheBound, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			srv := newTooLargeAnswerServer(t, row.version, row.answer)
			trans, err := transport.NewStreamableHTTP(srv.URL, transport.WithHTTPMaxMessageBytes(tooLargeBound))
			require.NoError(t, err)
			c := NewClient(trans)
			t.Cleanup(func() { _ = c.Close() })
			require.NoError(t, c.Start(t.Context()))
			result, err := c.Initialize(t.Context(), mcp.InitializeRequest{})
			require.NoError(t, err)
			require.Equal(t, row.version, result.ProtocolVersion)

			_, err = c.CallTool(t.Context(), mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "large"}})
			require.ErrorIs(t, err, transport.ErrMessageTooLarge)
			callID := <-srv.callID

			if !row.cancels {
				select {
				case m := <-srv.cancelled:
					t.Fatalf("notifications/cancelled sent for a request that needs none: %s", m.Params)
				case <-time.After(200 * time.Millisecond):
				}
				return
			}
			var cancelled postedMessage
			select {
			case cancelled = <-srv.cancelled:
			case <-time.After(2 * time.Second):
				t.Fatal("no notifications/cancelled reached the server")
			}
			var params struct {
				RequestID json.RawMessage `json:"requestId"`
				Reason    string          `json:"reason"`
			}
			require.NoError(t, json.Unmarshal(cancelled.Params, &params))
			assert.JSONEq(t, string(callID), string(params.RequestID))
			assert.Equal(t, transport.ErrMessageTooLarge.Error(), params.Reason)
		})
	}
}

// cancelOnClose is a RoundTripper whose first response after arm is a body
// that cancels the context arm was given when it is closed.
type cancelOnClose struct {
	base   http.RoundTripper
	cancel atomic.Pointer[context.CancelFunc]
}

func (t *cancelOnClose) arm(cancel context.CancelFunc) { t.cancel.Store(&cancel) }

func (t *cancelOnClose) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if cancel := t.cancel.Swap(nil); cancel != nil {
		resp.Body = cancellingBody{ReadCloser: resp.Body, cancel: *cancel}
	}
	return resp, nil
}

// cancellingBody cancels its context when it is closed.
type cancellingBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b cancellingBody) Close() error {
	b.cancel()
	return b.ReadCloser.Close()
}

// A JSON answer refused over the bound is the request's one response, so no
// notifications/cancelled goes out for it, even when the caller's context
// ends while the refused response is being released.
func TestClient_NoCancelForARefusedAnswerWhoseContextEndsOnRelease(t *testing.T) {
	for _, row := range []struct {
		name   string
		answer tooLargeAnswer
	}{
		{"JSON answer over the bound", jsonOverTheBound},
		{"JSON answer of a stated length over the bound", statedJSONOverTheBound},
	} {
		t.Run(row.name, func(t *testing.T) {
			srv := newTooLargeAnswerServer(t, mcp.LATEST_LEGACY_PROTOCOL_VERSION, row.answer)
			rt := &cancelOnClose{base: srv.Client().Transport}
			trans, err := transport.NewStreamableHTTP(srv.URL,
				transport.WithHTTPMaxMessageBytes(tooLargeBound),
				transport.WithHTTPBasicClient(&http.Client{Transport: rt}))
			require.NoError(t, err)
			c := NewClient(trans)
			t.Cleanup(func() { _ = c.Close() })
			require.NoError(t, c.Start(t.Context()))
			_, err = c.Initialize(t.Context(), mcp.InitializeRequest{})
			require.NoError(t, err)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			rt.arm(cancel)
			_, err = c.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: "large"}})
			require.ErrorIs(t, err, transport.ErrMessageTooLarge)
			require.ErrorIs(t, ctx.Err(), context.Canceled, "the context did not end while the response was released")
			<-srv.callID

			select {
			case m := <-srv.cancelled:
				t.Fatalf("notifications/cancelled sent for a request whose response arrived: %s", m.Params)
			case <-time.After(200 * time.Millisecond):
			}
		})
	}
}
