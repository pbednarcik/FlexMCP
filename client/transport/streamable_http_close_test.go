package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// Watching for Close costs a request no goroutine of its own.
func TestContextAwareOfClientCloseStartsNoGoroutine(t *testing.T) {
	c, err := NewStreamableHTTP("http://127.0.0.1:1/mcp")
	require.NoError(t, err)

	const requests = 100
	before := runtime.NumGoroutine()
	cancels := make([]context.CancelFunc, 0, requests)
	for range requests {
		_, cancel := c.contextAwareOfClientClose(t.Context())
		cancels = append(cancels, cancel)
	}
	assert.Less(t, runtime.NumGoroutine()-before, requests/10, "goroutines for %d requests in flight", requests)
	for _, cancel := range cancels {
		cancel()
	}
}

// A request that ends lets go of its watch on Close: a transport that serves
// many requests does not keep one for each.
func TestContextAwareOfClientCloseLetsGoWhenTheRequestEnds(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	c, err := NewStreamableHTTP("http://127.0.0.1:1/mcp")
	require.NoError(t, err)

	cycle := func(n int) {
		for range n {
			_, cancel := c.contextAwareOfClientClose(t.Context())
			cancel()
		}
	}
	cycle(1000)
	before := heapInUse()
	cycle(50000)
	assert.Less(t, heapInUse()-before, int64(1<<20), "heap kept by 50000 ended requests")
	// The transport is live until here: a collected one would take what it
	// kept with it.
	runtime.KeepAlive(c)
}

// heapInUse is the live heap after a collection.
func heapInUse() int64 {
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return int64(stats.HeapInuse)
}

// closeTestPing and closeTestNote are what the close tests send; no server
// has to understand them.
var (
	closeTestPing = JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(1)), Method: "ping"}
	closeTestNote = mcp.JSONRPCNotification{JSONRPC: "2.0", Method: "notifications/initialized"}
)

// closeTestInitialize is an initialize answer on a protocol version that
// still opens the continuous GET listener.
const closeTestInitialize = `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"s","version":"1"}}}`

// countingRoundTripper counts the requests that reach it. With block it holds
// each until its context ends; without, it answers an empty result.
type countingRoundTripper struct {
	hits  atomic.Int64
	block bool
}

func (rt *countingRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.hits.Add(1)
	if rt.block {
		<-r.Context().Done()
		return nil, r.Context().Err()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":1,"result":{}}`)),
		Request:    r,
	}, nil
}

// throughRoundTripper is a transport whose every request goes to rt.
func throughRoundTripper(t *testing.T, rt http.RoundTripper) *StreamableHTTP {
	t.Helper()
	tr, err := NewStreamableHTTP("http://127.0.0.1:1/mcp", WithHTTPBasicClient(&http.Client{Transport: rt}))
	require.NoError(t, err)
	return tr
}

// Close is final: Start after it fails and starts nothing, whether or not the
// transport was started.
func TestStreamableHTTP_StartAfterCloseIsRefused(t *testing.T) {
	for _, row := range []struct {
		name   string
		listen bool
	}{
		{"never started", false},
		{"never started, listening", true},
	} {
		t.Run(row.name, func(t *testing.T) {
			var opts []StreamableHTTPCOption
			if row.listen {
				opts = append(opts, WithContinuousListening())
			}
			tr, err := NewStreamableHTTP("http://127.0.0.1:1/mcp", opts...)
			require.NoError(t, err)
			require.NoError(t, tr.Close())
			assert.ErrorIs(t, tr.Start(t.Context()), ErrTransportClosed, "Start after Close")
		})
	}
	// A transport whose initialize completed takes Start's fast path.
	t.Run("after an initialize", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, closeTestInitialize)
		}))
		defer srv.Close()
		tr, err := NewStreamableHTTP(srv.URL)
		require.NoError(t, err)
		require.NoError(t, tr.Start(t.Context()))
		_, err = tr.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(1)), Method: "initialize"})
		require.NoError(t, err)
		select {
		case <-tr.initialized:
		default:
			require.FailNow(t, "the initialize did not mark the transport initialized")
		}
		require.NoError(t, tr.Close())
		assert.ErrorIs(t, tr.Start(t.Context()), ErrTransportClosed, "Start after an initialize and Close")
	})
}

// After Close a request or a notification is refused before it reaches the
// HTTP client.
func TestStreamableHTTP_NothingIsSentAfterClose(t *testing.T) {
	t.Run("fifty rounds", func(t *testing.T) {
		rt := &countingRoundTripper{}
		for range 50 {
			tr := throughRoundTripper(t, rt)
			require.NoError(t, tr.Close())
			_, err := tr.SendRequest(t.Context(), closeTestPing)
			require.ErrorIs(t, err, ErrTransportClosed, "SendRequest after Close")
			require.ErrorIs(t, tr.SendNotification(t.Context(), closeTestNote), ErrTransportClosed, "SendNotification after Close")
		}
		assert.Zero(t, rt.hits.Load(), "sends after Close that reached the round tripper")
	})
	t.Run("a request alone", func(t *testing.T) {
		rt := &countingRoundTripper{}
		tr := throughRoundTripper(t, rt)
		require.NoError(t, tr.Close())
		_, _ = tr.SendRequest(t.Context(), closeTestPing)
		assert.Zero(t, rt.hits.Load(), "times SendRequest after Close reached the round tripper")
	})
	t.Run("a notification alone", func(t *testing.T) {
		rt := &countingRoundTripper{}
		tr := throughRoundTripper(t, rt)
		require.NoError(t, tr.Close())
		_ = tr.SendNotification(t.Context(), closeTestNote)
		assert.Zero(t, rt.hits.Load(), "times SendNotification after Close reached the round tripper")
	})
}

// cutByClose runs send until held reports the server or the round tripper
// holds it, closes the transport, and returns send's error.
func cutByClose(t *testing.T, tr *StreamableHTTP, held func() bool, send func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- send() }()
	require.Eventually(t, held, 2*time.Second, time.Millisecond, "the send never reached the server")
	require.NoError(t, tr.Close())
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		require.FailNow(t, "the send did not end on Close")
		return nil
	}
}

// A send Close cuts short fails with an error that is ErrTransportClosed and
// keeps the failure Close caused, wherever Close found it.
func TestStreamableHTTP_CloseCutsASendShort(t *testing.T) {
	t.Run("request in the round trip", func(t *testing.T) {
		rt := &countingRoundTripper{block: true}
		tr := throughRoundTripper(t, rt)
		err := cutByClose(t, tr, func() bool { return rt.hits.Load() > 0 }, func() error {
			_, err := tr.SendRequest(t.Context(), closeTestPing)
			return err
		})
		assert.ErrorIs(t, err, ErrTransportClosed)
		assert.ErrorIs(t, err, context.Canceled)
	})
	t.Run("notification in the round trip", func(t *testing.T) {
		rt := &countingRoundTripper{block: true}
		tr := throughRoundTripper(t, rt)
		err := cutByClose(t, tr, func() bool { return rt.hits.Load() > 0 }, func() error {
			return tr.SendNotification(t.Context(), closeTestNote)
		})
		assert.ErrorIs(t, err, ErrTransportClosed)
		assert.ErrorIs(t, err, context.Canceled)
	})
	for _, body := range []struct{ name, contentType, first string }{
		{"request reading an event stream", "text/event-stream", ": hello\n\n"},
		{"request reading a JSON body", "application/json", `{"jsonrpc":"2.0",`},
	} {
		t.Run(body.name, func(t *testing.T) {
			var held atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", body.contentType)
				if body.contentType == "application/json" {
					// A stated length the body never reaches keeps the
					// client reading.
					w.Header().Set("Content-Length", "200")
				}
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, body.first)
				w.(http.Flusher).Flush()
				held.Store(true)
				<-r.Context().Done()
			}))
			defer srv.Close()
			tr, err := NewStreamableHTTP(srv.URL)
			require.NoError(t, err)
			err = cutByClose(t, tr, held.Load, func() error {
				_, err := tr.SendRequest(t.Context(), closeTestPing)
				return err
			})
			assert.ErrorIs(t, err, ErrTransportClosed)
		})
	}
}

// A send that fails on its own while the transport is open is not reported
// as closed.
func TestStreamableHTTP_AFailureWhileOpenIsNotClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	tr, err := NewStreamableHTTP(srv.URL)
	require.NoError(t, err)
	defer func() { _ = tr.Close() }()

	_, err = tr.SendRequest(t.Context(), closeTestPing)
	require.Error(t, err, "a request answered 500")
	assert.False(t, errors.Is(err, ErrTransportClosed), "a request answered 500 failed as closed: %v", err)

	err = tr.SendNotification(t.Context(), closeTestNote)
	require.Error(t, err, "a notification answered 500")
	assert.False(t, errors.Is(err, ErrTransportClosed), "a notification answered 500 failed as closed: %v", err)
}

// The continuous GET listener ends on Close and does not connect again.
func TestStreamableHTTP_TheListenerEndsOnClose(t *testing.T) {
	var gets atomic.Int64
	getEnded := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch r.Method {
		case http.MethodGet:
			gets.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			getEnded <- struct{}{}
		case http.MethodDelete:
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set(HeaderKeySessionID, "s1")
			_, _ = io.WriteString(w, closeTestInitialize)
		}
	}))
	defer srv.Close()
	tr, err := NewStreamableHTTP(srv.URL, WithContinuousListening())
	require.NoError(t, err)
	tr.retryInterval = 10 * time.Millisecond
	require.NoError(t, tr.Start(t.Context()))
	_, err = tr.SendRequest(t.Context(), JSONRPCRequest{JSONRPC: "2.0", ID: mcp.NewRequestId(int64(1)), Method: "initialize"})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return gets.Load() > 0 }, 2*time.Second, time.Millisecond, "the listener never connected")

	require.NoError(t, tr.Close())
	select {
	case <-getEnded:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "the GET stream did not end on Close")
	}
	// An absence has no event to poll for: ten retry intervals pass, and no
	// GET may come in them.
	before := gets.Load()
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, before, gets.Load(), "GETs after Close")
}
