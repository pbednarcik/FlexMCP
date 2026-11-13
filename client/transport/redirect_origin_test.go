package transport

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/stretchr/testify/require"
)

// offOriginRedirect starts an origin that answers every request with status
// and a Location off that origin, and counts the requests that arrive
// anywhere else. Another host is the same listener reached as localhost
// instead of its address, so only the host differs; another port is a
// second listener, so only the port does.
func offOriginRedirect(t *testing.T, status int, to string) (string, *atomic.Int32) {
	t.Helper()
	reached := new(atomic.Int32)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(other.Close)
	origin := httptest.NewUnstartedServer(nil)
	addr := origin.Listener.Addr().String()
	location := other.URL + "/elsewhere"
	if to == "another host" {
		originHost, port, err := net.SplitHostPort(addr)
		require.NoError(t, err)
		otherHost := "localhost"
		if originHost == otherHost {
			t.Fatalf("the origin and the other host are both %q: the redirect would stay on the origin", originHost)
		}
		location = "http://" + net.JoinHostPort(otherHost, port) + "/elsewhere"
	}
	origin.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != addr {
			reached.Add(1)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		http.Redirect(w, r, location, status)
	})
	origin.Start()
	t.Cleanup(origin.Close)
	return origin.URL, reached
}

// A transport on its default client sends its headers, and on a 307 or 308
// its body, with every hop it follows, so it follows a redirect only while
// the request's scheme, host and port stay the same.
func TestTransports_ADefaultClientKeepsRedirectsOnTheOrigin(t *testing.T) {
	headers := map[string]string{"X-Api-Key": "secret"}
	request := JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Method: "tools/list"}
	destinations := []string{"another host", "another port"}

	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, to := range destinations {
			t.Run(fmt.Sprintf("streamable http %d to %s", status, to), func(t *testing.T) {
				originURL, reached := offOriginRedirect(t, status, to)
				trans, err := NewStreamableHTTP(originURL+"/mcp", WithHTTPHeaders(headers))
				require.NoError(t, err)
				t.Cleanup(func() { _ = trans.Close() })

				_, err = trans.SendRequest(t.Context(), request)
				n := reached.Load()
				require.Zero(t, n, "the redirect target got %d request(s)", n)
				require.ErrorIs(t, err, ErrRedirectLeavesOrigin)
			})
		}
	}
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
		for _, to := range destinations {
			t.Run(fmt.Sprintf("sse start %d to %s", status, to), func(t *testing.T) {
				originURL, reached := offOriginRedirect(t, status, to)
				trans, err := NewSSE(originURL+"/sse", WithHeaders(headers))
				require.NoError(t, err)
				t.Cleanup(func() { _ = trans.Close() })

				err = trans.Start(t.Context())
				n := reached.Load()
				require.Zero(t, n, "the redirect target got %d request(s)", n)
				require.ErrorIs(t, err, ErrRedirectLeavesOrigin)
			})
		}
	}
	t.Run("sse post 307 to another port", func(t *testing.T) {
		var reached atomic.Int32
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached.Add(1)
			w.WriteHeader(http.StatusAccepted)
		}))
		t.Cleanup(other.Close)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/sse" {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "event: endpoint\ndata: /message\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				return
			}
			http.Redirect(w, r, other.URL+"/elsewhere", http.StatusTemporaryRedirect)
		}))
		t.Cleanup(server.Close)
		trans, err := NewSSE(server.URL+"/sse", WithHeaders(headers))
		require.NoError(t, err)
		t.Cleanup(func() { _ = trans.Close() })
		require.NoError(t, trans.Start(t.Context()))

		// A followed POST is answered on the stream, which never answers it.
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		_, err = trans.SendRequest(ctx, request)
		n := reached.Load()
		require.Zero(t, n, "the redirect target got %d request(s)", n)
		require.ErrorIs(t, err, ErrRedirectLeavesOrigin)
	})
	t.Run("a redirect on the origin is followed", func(t *testing.T) {
		var answered atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/mcp/" {
				http.Redirect(w, r, "/mcp/", http.StatusPermanentRedirect)
				return
			}
			answered.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
		}))
		t.Cleanup(server.Close)
		trans, err := NewStreamableHTTP(server.URL+"/mcp", WithHTTPHeaders(headers))
		require.NoError(t, err)
		t.Cleanup(func() { _ = trans.Close() })

		_, err = trans.SendRequest(t.Context(), request)
		require.NoError(t, err)
		require.Equal(t, int32(1), answered.Load())
	})
	t.Run("a timeout on the default client keeps it bound", func(t *testing.T) {
		originURL, reached := offOriginRedirect(t, http.StatusTemporaryRedirect, "another port")
		trans, err := NewStreamableHTTP(originURL+"/mcp", WithHTTPTimeout(5*time.Second))
		require.NoError(t, err)
		t.Cleanup(func() { _ = trans.Close() })

		_, err = trans.SendRequest(t.Context(), request)
		n := reached.Load()
		require.Zero(t, n, "the redirect target got %d request(s)", n)
		require.ErrorIs(t, err, ErrRedirectLeavesOrigin)
	})
}

// The GET stream is bound to the origin too. A server that redirects it
// elsewhere does so on every retry, each one refused, so the listener stops
// after the first and says why once.
func TestStreamableHTTP_AListenerRedirectedOffTheOriginStops(t *testing.T) {
	orig := retryInterval
	t.Cleanup(func() { retryInterval = orig })
	retryInterval = 5 * time.Millisecond

	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(other.Close)
	var gets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		gets.Add(1)
		http.Redirect(w, r, other.URL+"/elsewhere", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)

	var logged lockedBuffer
	trans, err := NewStreamableHTTP(server.URL+"/mcp", WithContinuousListening(),
		WithHTTPLogger(slog.New(slog.NewTextHandler(&logged, nil))))
	require.NoError(t, err)
	t.Cleanup(func() { _ = trans.Close() })
	require.NoError(t, trans.Start(t.Context()))
	trans.initializedOnce.Do(func() { close(trans.initialized) })

	require.Eventually(t, func() bool { return gets.Load() >= 1 }, 5*time.Second, time.Millisecond,
		"the listener never sent its GET")
	time.Sleep(20 * retryInterval)
	n := gets.Load()
	require.Equal(t, int32(1), n, "the listener sent %d GETs to a server that redirects it off the origin", n)
	require.Zero(t, reached.Load(), "the redirect target was reached")
	logs := logged.String()
	require.Equal(t, 1, strings.Count(logs, ErrRedirectLeavesOrigin.Error()), "the refusal is logged once:\n%s", logs)
}
