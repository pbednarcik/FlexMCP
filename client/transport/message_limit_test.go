package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// testBound is the message bound these tests set, so that none streams a
// message the size of the default.
const testBound = 1 << 10

const (
	answerHead = `{"jsonrpc":"2.0","id":7,"result":{"text":"`
	answerTail = `"}}`
)

// answerOfSize is the answer to request 7, n bytes long: its result's text
// is padded to fit.
func answerOfSize(n int) string {
	return answerHead + strings.Repeat("x", n-len(answerHead)-len(answerTail)) + answerTail
}

// answerLinesOfSize is an answer to request 7 split into lines that, joined
// by newlines as SSE joins an event's data lines, are n bytes long.
func answerLinesOfSize(n int) []string {
	return strings.SplitAfterN(answerOfSize(n-2), ",", 3)
}

// messageEvent is an SSE message event with one data line per line.
func messageEvent(lines ...string) string {
	var b strings.Builder
	b.WriteString("event: message\n")
	for _, line := range lines {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}

// crlf is event with its lines ended by CRLF.
func crlf(event string) string {
	return strings.ReplaceAll(event, "\n", "\r\n")
}

// answerText is the text of an answer answerOfSize made.
func answerText(t *testing.T, response *JSONRPCResponse) string {
	t.Helper()
	require.NotNil(t, response)
	var result struct{ Text string }
	require.NoError(t, json.Unmarshal(response.Result, &result))
	return result.Text
}

var request7 = JSONRPCRequest{JSONRPC: "2.0", Method: "tools/call", ID: mcp.NewRequestId(int64(7))}

// countedAnswer answers every request with body as JSON of the given
// ContentLength, counting the bytes the client reads of it.
type countedAnswer struct {
	body   string
	length int64
	read   *atomic.Int64
}

func (a countedAnswer) RoundTrip(r *http.Request) (*http.Response, error) {
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(&countingReader{r: strings.NewReader(a.body), n: a.read}),
		ContentLength: a.length,
		Request:       r,
	}, nil
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// Both constructors bound a message at DefaultMaxMessageBytes, and the
// options set the bound.
func TestTheMessageBoundIsDefaultMaxMessageBytes(t *testing.T) {
	assert.Equal(t, int64(32<<20), DefaultMaxMessageBytes)

	h, err := NewStreamableHTTP("http://127.0.0.1:1/mcp")
	require.NoError(t, err)
	assert.Equal(t, DefaultMaxMessageBytes, h.maxMessageBytes, "NewStreamableHTTP")
	h, err = NewStreamableHTTP("http://127.0.0.1:1/mcp", WithHTTPMaxMessageBytes(5))
	require.NoError(t, err)
	assert.Equal(t, int64(5), h.maxMessageBytes, "WithHTTPMaxMessageBytes")

	s, err := NewSSE("http://127.0.0.1:1/sse")
	require.NoError(t, err)
	assert.Equal(t, DefaultMaxMessageBytes, s.maxMessageBytes, "NewSSE")
	s, err = NewSSE("http://127.0.0.1:1/sse", WithMaxMessageBytes(5))
	require.NoError(t, err)
	assert.Equal(t, int64(5), s.maxMessageBytes, "WithMaxMessageBytes")
}

// A JSON answer over the bound fails with ErrMessageTooLarge, whatever its
// stated length says, and one whose stated length is over the bound is not
// read at all. One that fills the bound exactly is the answer, for every
// request in turn, and a bound of zero is none.
func TestStreamableHTTP_JSONAnswerOverTheBoundFails(t *testing.T) {
	// Stated longer than releaseResponse drains, so nothing reads it.
	stated := answerOfSize(maxDrainBytes + 1)
	cases := []struct {
		name     string
		body     string
		length   int64
		bound    int64
		requests int
		tooLarge bool
	}{
		{"chunked, over the bound", answerOfSize(2 * testBound), -1, testBound, 1, true},
		{"stated length over the bound", stated, int64(len(stated)), testBound, 1, true},
		{"exactly the bound", answerOfSize(testBound), testBound, testBound, 1, false},
		{"the bound and one byte", answerOfSize(testBound + 1), -1, testBound, 1, true},
		// The decoder reads in parts of 512 bytes and more: a bound that is
		// no multiple of them ends inside one.
		{"an unaligned bound and one byte", answerOfSize(1001), -1, 1000, 1, true},
		{"two requests at the bound", answerOfSize(testBound), -1, testBound, 2, false},
		{"no bound", answerOfSize(4 * testBound), -1, 0, 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var read atomic.Int64
			c, err := NewStreamableHTTP("http://127.0.0.1:1/mcp",
				WithHTTPBasicClient(&http.Client{Transport: countedAnswer{body: tc.body, length: tc.length, read: &read}}),
				WithHTTPMaxMessageBytes(tc.bound))
			require.NoError(t, err)
			t.Cleanup(func() { _ = c.Close() })
			for i := range tc.requests {
				response, err := c.SendRequest(t.Context(), request7)
				if tc.tooLarge {
					require.ErrorIs(t, err, ErrMessageTooLarge, "a %d-byte answer (a response returned: %v)", len(tc.body), response != nil)
					assert.ErrorContains(t, err, "failed to decode response")
					continue
				}
				require.NoError(t, err, "request %d", i+1)
				assert.Len(t, answerText(t, response), len(tc.body)-len(answerHead)-len(answerTail))
			}
			if tc.length > tc.bound && tc.bound > 0 {
				assert.Zero(t, read.Load(), "an answer stated over the bound was read")
			}
		})
	}
}

// The data of one SSE event over the bound fails the request with
// ErrMessageTooLarge, whether it is one line or several, and so does any
// line over the bound, a comment too. Data that fills the bound exactly, in
// one line or several with the newlines that join them, is the answer.
func TestStreamableHTTP_SSEEventOverTheBoundFails(t *testing.T) {
	cases := []struct {
		name     string
		stream   string
		tooLarge bool
		text     int
	}{
		{"one data line over the bound", messageEvent(answerOfSize(testBound + 1)), true, 0},
		{"data lines over the bound", messageEvent(answerLinesOfSize(testBound + 1)...), true, 0},
		{"a comment line over the bound", ": " + strings.Repeat("x", testBound+16) + "\n" + messageEvent(answerOfSize(64)), true, 0},
		{"exactly the bound in one line", messageEvent(answerOfSize(testBound)), false, testBound - len(answerHead) - len(answerTail)},
		{"exactly the bound in one line ending in CRLF", crlf(messageEvent(answerOfSize(testBound))), false, testBound - len(answerHead) - len(answerTail)},
		{"exactly the bound in lines", messageEvent(answerLinesOfSize(testBound)...), false, testBound - 2 - len(answerHead) - len(answerTail)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewStreamableHTTP("http://127.0.0.1:1/mcp",
				WithHTTPBasicClient(&http.Client{Transport: eventStreamRoundTripper{body: []byte(tc.stream)}}),
				WithHTTPMaxMessageBytes(testBound))
			require.NoError(t, err)
			t.Cleanup(func() { _ = c.Close() })
			response, err := c.SendRequest(t.Context(), request7)
			if tc.tooLarge {
				require.ErrorIs(t, err, ErrMessageTooLarge, "an event over the bound (a response returned: %v)", response != nil)
				assert.ErrorContains(t, err, "failed to read the SSE stream")
				return
			}
			require.NoError(t, err)
			assert.Len(t, answerText(t, response), tc.text)
		})
	}
}

// A stream that an event over the bound ended is not resumed, even before
// protocol version 2026-07-28, where the server has sent an event ID: the
// server would replay the same event.
func TestStreamableHTTP_AnOversizedEventIsNotResumed(t *testing.T) {
	server := &pollingServer{postEvent: "id: 1\nretry: 10\ndata:\n\n" + messageEvent(strings.Repeat("x", 2*testBound)), polls: 1}
	_, err := sendThroughPollingServer(t, server, mcp.ProtocolVersion20251125, WithHTTPMaxMessageBytes(testBound))
	require.ErrorIs(t, err, ErrMessageTooLarge)
	server.mu.Lock()
	defer server.mu.Unlock()
	assert.Empty(t, server.lastIDs, "the stream was resumed after an event over the bound")
}

// An event over the bound ends the GET stream, which the listener opens
// again after its retry interval. Why the stream ended is logged once for
// each end, and not as a response's failure: the stream answers none.
func TestStreamableHTTP_ListenStreamEndsOnAnOversizedEvent(t *testing.T) {
	orig := retryInterval
	t.Cleanup(func() { retryInterval = orig })
	retryInterval = 10 * time.Millisecond

	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		gets.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, messageEvent(strings.Repeat("x", 2*testBound)))
		w.(http.Flusher).Flush()
		// The server keeps the stream open: only the client ends it.
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	var logged lockedBuffer
	trans, err := NewStreamableHTTP(srv.URL, WithContinuousListening(), WithHTTPMaxMessageBytes(testBound),
		WithHTTPLogger(slog.New(slog.NewTextHandler(&logged, nil))))
	require.NoError(t, err)
	t.Cleanup(func() { _ = trans.Close() })
	require.NoError(t, trans.Start(t.Context()))
	trans.initializedOnce.Do(func() { close(trans.initialized) })

	require.Eventually(t, func() bool { return gets.Load() >= 2 }, 2*time.Second, 5*time.Millisecond,
		"the GET stream outlived an event over the bound")
	logs := logged.String()
	assert.Contains(t, logs, `msg="SSE stream error" err="`+ErrMessageTooLarge.Error()+`"`)
	assert.NotContains(t, logs, "for the response")
	assert.Equal(t, strings.Count(logs, `msg="SSE stream error"`), strings.Count(logs, "level=ERROR"),
		"an end logged more than once:\n%s", logs)
}

// An event over the bound, in one data line or several, or any line over
// it, ends the legacy SSE stream: the connection-lost handler gets
// ErrMessageTooLarge, and the request waiting on the stream fails with an
// error that is both ErrMessageTooLarge and ErrTransportClosed, whether the
// stream ended after its POST was answered or while the POST waited for its
// answer, which the end cuts short. Data that fills the bound exactly is the
// answer, and a bound of zero is none. A line over the bound before the
// endpoint event fails Start with ErrMessageTooLarge.
func TestSSE_EventOverTheBoundEndsTheStream(t *testing.T) {
	cases := []struct {
		name     string
		event    string
		bound    int64
		inPost   bool // the event comes while the POST waits for its answer, not after it
		tooLarge bool
		text     int
	}{
		{"one data line over the bound", messageEvent(answerOfSize(testBound + 1)), testBound, false, true, 0},
		{"one data line over the bound, in the POST", messageEvent(answerOfSize(testBound + 1)), testBound, true, true, 0},
		{"data lines over the bound", messageEvent(answerLinesOfSize(testBound + 1)...), testBound, false, true, 0},
		{"a comment line over the bound", ": " + strings.Repeat("x", testBound+16) + "\n" + messageEvent(answerOfSize(64)), testBound, false, true, 0},
		{"exactly the bound in one line", messageEvent(answerOfSize(testBound)), testBound, false, false, testBound - len(answerHead) - len(answerTail)},
		{"exactly the bound in one line ending in CRLF", crlf(messageEvent(answerOfSize(testBound))), testBound, false, false, testBound - len(answerHead) - len(answerTail)},
		{"exactly the bound in lines", messageEvent(answerLinesOfSize(testBound)...), testBound, false, false, testBound - 2 - len(answerHead) - len(answerTail)},
		{"no bound", messageEvent(answerOfSize(4 * testBound)), 0, false, false, 4*testBound - len(answerHead) - len(answerTail)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			answered := make(chan struct{}, 1)
			release := make(chan struct{})
			server := newScriptedSSEServer()
			server.onPost = func(_ json.RawMessage, s *scriptedSSEServer) {
				if !tc.inPost {
					go func() {
						<-answered
						s.events <- tc.event
					}()
					return
				}
				// The POST is not answered until the test is done, so it can
				// only end by the event ending the stream, and with it the
				// POST: an answer that raced the end would leave the request
				// waiting on the stream instead.
				s.events <- tc.event
				select {
				case <-release:
				case <-time.After(5 * time.Second):
				}
			}
			client := &http.Client{Transport: postAnswered{rt: &http.Transport{}, answered: answered}}
			t.Cleanup(client.CloseIdleConnections)
			c := startedSSE(t, server, WithHTTPClient(client), WithMaxMessageBytes(tc.bound), WithResponseTimeout(5*time.Second))
			// Registered after startedSSE's cleanups, so it runs before them:
			// the POST is let go before the server is closed.
			t.Cleanup(func() { close(release) })
			lost := make(chan error, 1)
			c.SetConnectionLostHandler(func(err error) { lost <- err })

			response, err := c.SendRequest(t.Context(), request7)
			if !tc.tooLarge {
				require.NoError(t, err)
				assert.Len(t, answerText(t, response), tc.text)
				return
			}
			require.ErrorIs(t, err, ErrMessageTooLarge, "an event over the bound (a response returned: %v)", response != nil)
			assert.ErrorIs(t, err, ErrTransportClosed)
			if tc.inPost {
				assert.ErrorIs(t, err, errStreamEnded, "the end did not cut the POST short")
			} else {
				assert.ErrorIs(t, err, errConnectionClosed, "the request did not wait on the stream")
			}
			select {
			case err := <-lost:
				assert.ErrorIs(t, err, ErrMessageTooLarge)
			case <-time.After(2 * time.Second):
				t.Fatal("the connection-lost handler was not called")
			}
		})
	}

	t.Run("a line over the bound before the endpoint", func(t *testing.T) {
		server := newScriptedSSEServer()
		server.noEndpoint = true
		server.events <- messageEvent(answerOfSize(testBound + 1))
		httpServer := httptest.NewServer(server)
		t.Cleanup(httpServer.Close)
		c, err := NewSSE(httpServer.URL, WithMaxMessageBytes(testBound))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		err = c.Start(ctx)
		require.ErrorIs(t, err, ErrMessageTooLarge)
		assert.ErrorIs(t, err, ErrTransportClosed)
	})

	t.Run("the stream ended over the bound after the endpoint, before Start published", func(t *testing.T) {
		// The round trip runs in Start once its attempt has begun, before its
		// reader does. It ends the stream there as the reader ends it on a
		// line over the bound, under lifecycleMu, but leaves the stream's
		// context alone, as the reader has not cancelled it yet when it lets
		// lifecycleMu go: so Start receives the endpoint, and then finds the
		// stream ended.
		body, write := io.Pipe()
		t.Cleanup(func() { _ = write.Close() })
		var c *SSE
		rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			c.lifecycleMu.Lock()
			c.ended.Store(true)
			c.failPending(ErrMessageTooLarge)
			c.lifecycleMu.Unlock()
			go func() { _, _ = io.WriteString(write, "event: endpoint\ndata: /message\n\n") }()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"text/event-stream"}},
				Body:       body,
				Request:    r,
			}, nil
		})
		var err error
		c, err = NewSSE("http://mcp.invalid/sse", WithHTTPClient(&http.Client{Transport: rt}), WithMaxMessageBytes(testBound))
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		err = c.Start(ctx)
		require.ErrorIs(t, err, ErrMessageTooLarge)
		assert.ErrorIs(t, err, ErrTransportClosed)
	})
}

// postAnswered is rt sending on answered, without waiting, once the round
// trip of a POST has returned its answer.
type postAnswered struct {
	rt       http.RoundTripper
	answered chan struct{}
}

func (p postAnswered) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := p.rt.RoundTrip(r)
	if err == nil && r.Method == http.MethodPost {
		select {
		case p.answered <- struct{}{}:
		default:
		}
	}
	return resp, err
}
