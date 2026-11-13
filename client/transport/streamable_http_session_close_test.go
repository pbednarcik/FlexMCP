package transport

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/mcp"
)

// sessionDeletes records the session ID of each DELETE a server receives.
type sessionDeletes struct {
	mu       sync.Mutex
	sessions []string
}

func (d *sessionDeletes) add(session string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sessions = append(d.sessions, session)
}

func (d *sessionDeletes) all() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.sessions...)
}

// closeFunc names Close in a goroutine dump, its closures included, in this
// package whatever the module is called.
var closeFunc = reflect.TypeFor[StreamableHTTP]().PkgPath() + ".(*StreamableHTTP).Close"

// goroutineID returns the calling goroutine's ID, as a goroutine dump heads
// its stack with it.
func goroutineID() string {
	buf := make([]byte, 64)
	buf = buf[:runtime.Stack(buf, false)]
	id, _, _ := strings.Cut(strings.TrimPrefix(string(buf), "goroutine "), " ")
	return id
}

// goroutineStack returns goroutine id's stack from a dump of every
// goroutine, or "" once it has exited.
func goroutineStack(id string) string {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for stack := range strings.SplitSeq(string(buf), "\n\n") {
		if strings.HasPrefix(stack, "goroutine "+id+" ") {
			return stack
		}
	}
	return ""
}

// lockCaller returns the function that called sync.(*Mutex).Lock in stack,
// as the dump names it, or "" when stack is not inside Lock. A dump gives
// each frame a line of its function and an indented line of its file, and
// lists inlined calls as frames of their own.
func lockCaller(stack string) string {
	var frames []string
	for _, line := range strings.Split(stack, "\n")[1:] {
		if line != "" && !strings.HasPrefix(line, "\t") {
			frames = append(frames, line)
		}
	}
	for i, frame := range frames[:max(len(frames)-1, 0)] {
		if strings.HasPrefix(frame, "sync.(*Mutex).Lock(") {
			return frames[i+1]
		}
	}
	return ""
}

// waitLockingIn waits until goroutine id is locking a sync.Mutex straight
// from a function whose name starts with caller, and reports whether it got
// there within a few seconds, with the goroutine's last stack. Inside Lock
// it has done everything its caller does before the lock, and while the
// mutex is held it does nothing more.
func waitLockingIn(id, caller string) (bool, string) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		stack := goroutineStack(id)
		if strings.HasPrefix(lockCaller(stack), caller) {
			return true, stack
		}
		if stack == "" {
			return false, "(it has returned)"
		}
		if time.Now().After(deadline) {
			return false, stack
		}
		time.Sleep(time.Millisecond)
	}
}

// legacyInitializeBody answers the initialize with ID 1 on a version that
// still has sessions.
const legacyInitializeBody = `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"s","version":"1"}}}`

// A session ID that an initialize response brings after Close is not
// published, and the session is ended on the server at once; the response
// itself is still the initialize's answer. Close and the response decide
// under one lock, so the session is ended exactly once whichever comes first.
func TestStreamableHTTP_ASessionThatArrivesAfterCloseIsEnded(t *testing.T) {
	initialize := JSONRPCRequest{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      mcp.NewRequestId(int64(1)),
		Method:  string(mcp.MethodInitialize),
	}

	// newTransport makes a transport whose initialize is answered with
	// session in its headers, after beforeAnswer has run, and which records
	// every DELETE.
	newTransport := func(t *testing.T, session string, beforeAnswer func(*StreamableHTTP)) (*StreamableHTTP, *sessionDeletes) {
		t.Helper()
		deletes := &sessionDeletes{}
		var trans *StreamableHTTP
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodDelete {
				deletes.add(r.Header.Get(HeaderKeySessionID))
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
			}
			beforeAnswer(trans)
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        http.Header{"Content-Type": {"application/json"}, HeaderKeySessionID: {session}},
				Body:          io.NopCloser(strings.NewReader(legacyInitializeBody)),
				ContentLength: -1,
				Request:       r,
			}, nil
		})}
		trans, err := NewStreamableHTTP("http://mcp.example/mcp", WithHTTPBasicClient(client))
		require.NoError(t, err)
		return trans, deletes
	}
	closeNow := func(trans *StreamableHTTP) { _ = trans.Close() }

	t.Run("Close during the initialize's round trip", func(t *testing.T) {
		trans, deletes := newTransport(t, "sess-late", closeNow)

		response, err := trans.SendRequest(t.Context(), initialize)

		if session := trans.GetSessionId(); session != "" && len(deletes.all()) == 0 {
			t.Fatalf("session %q published on a closed transport and never terminated", session)
		}
		assert.Empty(t, trans.GetSessionId(), "the session ID on a closed transport")
		assert.Equal(t, []string{"sess-late"}, deletes.all(), "the DELETEs, by session")
		require.NoError(t, err, "an initialize answered after Close still returns its answer")
		assert.NotNil(t, response.Result)
	})

	t.Run("Close after the session is published", func(t *testing.T) {
		trans, deletes := newTransport(t, "sess-early", func(*StreamableHTTP) {})

		_, err := trans.SendRequest(t.Context(), initialize)
		require.NoError(t, err)
		require.Equal(t, "sess-early", trans.GetSessionId())
		require.NoError(t, trans.Close())

		assert.Empty(t, trans.GetSessionId(), "the session ID on a closed transport")
		assert.Equal(t, []string{"sess-early"}, deletes.all(), "the DELETEs, by session")
	})

	t.Run("a modern transport has no session to end", func(t *testing.T) {
		trans, deletes := newTransport(t, "sess-late", closeNow)
		trans.SetProtocolVersion(mcp.ProtocolVersion20260728)

		_, err := trans.SendRequest(t.Context(), initialize)
		require.NoError(t, err)

		assert.Empty(t, trans.GetSessionId(), "the session ID on a closed transport")
		assert.Empty(t, deletes.all(), "the DELETEs, by session")
	})

	// Close takes the session it ends under sessionMu, the lock a publisher
	// holds: a session published while Close waits for the lock is Close's
	// to end. The test holds the lock as a publisher would, and publishes
	// once Close is waiting for it.
	t.Run("a session published while Close waits for the lock is ended", func(t *testing.T) {
		trans, deletes := newTransport(t, "", func(*StreamableHTTP) {})

		trans.sessionMu.Lock()
		closer := make(chan string, 1)
		var wg sync.WaitGroup
		wg.Go(func() {
			closer <- goroutineID()
			_ = trans.Close()
		})
		waiting, stack := waitLockingIn(<-closer, closeFunc)
		if waiting {
			trans.sessionID.Store("sess-held")
		}
		trans.sessionMu.Unlock()
		wg.Wait()
		require.Truef(t, waiting, "Close never waited for sessionMu; its goroutine:\n%s", stack)

		assert.Empty(t, trans.GetSessionId(), "the session ID on a closed transport")
		assert.Equal(t, []string{"sess-held"}, deletes.all(), "the DELETEs, by session")
	})

	// A session published before a switch to the modern protocol is no
	// session after it: Close forgets the ID, and sends no DELETE, which that
	// protocol removed.
	t.Run("a switch to modern before Close leaves no session", func(t *testing.T) {
		trans, deletes := newTransport(t, "sess-switched", func(*StreamableHTTP) {})

		_, err := trans.SendRequest(t.Context(), initialize)
		require.NoError(t, err)
		require.Equal(t, "sess-switched", trans.GetSessionId())
		trans.SetProtocolVersion(mcp.ProtocolVersion20260728)
		require.NoError(t, trans.Close())

		assert.Empty(t, trans.GetSessionId(), "the session ID on a closed transport")
		assert.Empty(t, deletes.all(), "the DELETEs, by session")
	})

	// Close and an initialize race on a real connection: whichever wins,
	// nothing stays published, a session is ended at most once, and a
	// session whose initialize was answered is ended.
	t.Run("Close and an initialize race", func(t *testing.T) {
		deletes := &sessionDeletes{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			session := "sess-" + r.URL.Query().Get("round")
			switch r.Method {
			case http.MethodPost:
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set(HeaderKeySessionID, session)
				_, _ = io.WriteString(w, legacyInitializeBody)
			case http.MethodDelete:
				deletes.add(r.Header.Get(HeaderKeySessionID))
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		}))
		t.Cleanup(server.Close)

		const rounds = 200
		for round := range rounds {
			session := fmt.Sprintf("sess-%d", round)
			trans, err := NewStreamableHTTP(fmt.Sprintf("%s/mcp?round=%d", server.URL, round))
			require.NoError(t, err)

			start := make(chan struct{})
			var answerErr error
			var wg sync.WaitGroup
			wg.Go(func() {
				<-start
				_, answerErr = trans.SendRequest(t.Context(), initialize)
			})
			wg.Go(func() {
				<-start
				_ = trans.Close()
			})
			close(start)
			wg.Wait()

			ended := 0
			for _, deleted := range deletes.all() {
				if deleted == session {
					ended++
				}
			}
			require.Empty(t, trans.GetSessionId(), "round %d: the session ID on a closed transport", round)
			require.LessOrEqual(t, ended, 1, "round %d: DELETEs of %s", round, session)
			if answerErr == nil {
				require.Equal(t, 1, ended, "round %d: DELETEs of %s, whose initialize was answered", round, session)
			}
		}
	})
}

// cutBody holds an initialize's streamed body until the request's context
// ends, as a server holding its answer does. It calls reading at each read:
// by the first, the session ID in the response's headers is published.
type cutBody struct {
	ctx     context.Context
	reading func()
}

func (b cutBody) Read([]byte) (int, error) {
	b.reading()
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (cutBody) Close() error { return nil }

// giveBackFunc names giveBackSessionID in a goroutine dump, as closeFunc
// names Close.
var giveBackFunc = reflect.TypeFor[StreamableHTTP]().PkgPath() + ".(*StreamableHTTP).giveBackSessionID"

// An initialize that Close cuts off after its response's headers published a
// session gets no answer, and gives the session back. Close has begun by
// then, so the session is Close's to end whether Close has taken it yet or
// not: it is ended exactly once, and nothing stays published.
func TestStreamableHTTP_AnInitializeCutByCloseHasItsSessionEnded(t *testing.T) {
	initialize := JSONRPCRequest{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      mcp.NewRequestId(int64(1)),
		Method:  string(mcp.MethodInitialize),
	}

	// newTransport makes a transport whose initialize response publishes
	// sess-cut and then holds its body until the request ends, and which
	// records every DELETE.
	newTransport := func(t *testing.T) (*StreamableHTTP, *sessionDeletes, <-chan struct{}) {
		t.Helper()
		deletes := &sessionDeletes{}
		reading := make(chan struct{})
		var once sync.Once
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodDelete {
				deletes.add(r.Header.Get(HeaderKeySessionID))
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
			}
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        http.Header{"Content-Type": {"text/event-stream"}, HeaderKeySessionID: {"sess-cut"}},
				Body:          cutBody{ctx: r.Context(), reading: func() { once.Do(func() { close(reading) }) }},
				ContentLength: -1,
				Request:       r,
			}, nil
		})}
		trans, err := NewStreamableHTTP("http://mcp.example/mcp", WithHTTPBasicClient(client))
		require.NoError(t, err)
		return trans, deletes, reading
	}

	// start sends the initialize under ctx and returns, once its session is
	// published, the channel its error arrives on and its goroutine's ID.
	start := func(t *testing.T, ctx context.Context, trans *StreamableHTTP, reading <-chan struct{}) (<-chan error, string) {
		t.Helper()
		done := make(chan error, 1)
		id := make(chan string, 1)
		go func() {
			id <- goroutineID()
			_, err := trans.SendRequest(ctx, initialize)
			done <- err
		}()
		select {
		case <-reading:
		case err := <-done:
			t.Fatalf("the initialize ended before its body was read: %v", err)
		}
		require.Equal(t, "sess-cut", trans.GetSessionId(), "the session the headers published")
		return done, <-id
	}

	// The test holds sessionMu as a concurrent request reading its session
	// does. Close cancels the initialize and waits for the lock; the
	// initialize gives its session back meanwhile, or waits for the lock too.
	t.Run("a session given back while Close waits for the lock is ended", func(t *testing.T) {
		trans, deletes, reading := newTransport(t)
		done, initializer := start(t, t.Context(), trans, reading)

		trans.sessionMu.Lock()
		closer := make(chan string, 1)
		var wg sync.WaitGroup
		wg.Go(func() {
			closer <- goroutineID()
			_ = trans.Close()
		})
		closing, stack := waitLockingIn(<-closer, closeFunc)
		if closing {
			// It has given the session back once it has returned.
			waitLockingIn(initializer, giveBackFunc)
		}
		trans.sessionMu.Unlock()
		wg.Wait()
		err := <-done
		require.Truef(t, closing, "Close never waited for sessionMu; its goroutine:\n%s", stack)

		require.Error(t, err, "the cut initialize has no answer")
		assert.Empty(t, trans.GetSessionId(), "the session ID on a closed transport")
		assert.Equal(t, []string{"sess-cut"}, deletes.all(), "the DELETEs, by session")
	})

	// Close's first step alone, which cancels the initialize: the transport
	// is closing, and Close has not taken the session yet when the initialize
	// gives it back.
	t.Run("a session given back before Close takes it is ended", func(t *testing.T) {
		trans, deletes, reading := newTransport(t)
		done, _ := start(t, t.Context(), trans, reading)

		trans.closeTransport()
		require.Error(t, <-done, "the cut initialize has no answer")
		assert.Equal(t, []string{"sess-cut"}, deletes.all(), "the DELETEs, by session, once the session is given back")

		require.NoError(t, trans.Close())
		assert.Empty(t, trans.GetSessionId(), "the session ID on a closed transport")
		assert.Equal(t, []string{"sess-cut"}, deletes.all(), "the DELETEs, by session, after Close")
	})

	// An initialize its caller gives up on, before any Close, gives its
	// session back and ends nothing; a later Close has no session to end.
	t.Run("a session given back before Close is not ended", func(t *testing.T) {
		trans, deletes, reading := newTransport(t)
		ctx, cancel := context.WithCancel(t.Context())
		done, _ := start(t, ctx, trans, reading)

		cancel()
		require.Error(t, <-done, "the abandoned initialize has no answer")
		assert.Empty(t, trans.GetSessionId(), "the session ID given back")
		assert.Empty(t, deletes.all(), "the DELETEs before Close")

		require.NoError(t, trans.Close())
		assert.Empty(t, deletes.all(), "the DELETEs after Close")
	})
}
