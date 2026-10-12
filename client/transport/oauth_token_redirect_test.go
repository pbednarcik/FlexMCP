package transport

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// An access token goes only to the server it was issued for (MCP
// authorization, Token Audience Binding): a request that carries one follows
// a redirect only while it stays on the request's origin, its scheme, host
// and port, so neither another port nor plain http on the same host ever
// receives the token.
func TestOAuthTransports_TokenRedirectsStayOnTheOrigin(t *testing.T) {
	var leaked atomic.Int32
	collect := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}
	plain := httptest.NewServer(http.HandlerFunc(collect))
	t.Cleanup(plain.Close)
	otherPort := httptest.NewTLSServer(http.HandlerFunc(collect))
	t.Cleanup(otherPort.Close)

	oauth := func(resource string) OAuthConfig {
		store := NewMemoryTokenStore()
		require.NoError(t, store.SaveToken(t.Context(), &Token{AccessToken: "token", TokenType: "Bearer", Resource: resource}))
		return OAuthConfig{ClientID: "client", TokenStore: store}
	}
	request := JSONRPCRequest{JSONRPC: mcp.JSONRPC_VERSION, ID: mcp.NewRequestId(int64(1)), Method: "tools/list"}

	for name, target := range map[string]string{"plain http": plain.URL, "another port": otherPort.URL} {
		t.Run("streamable http to "+name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target+"/mcp", http.StatusTemporaryRedirect)
			}))
			t.Cleanup(server.Close)
			trans, err := NewStreamableHTTP(server.URL+"/mcp", WithHTTPOAuth(oauth(server.URL+"/mcp")))
			require.NoError(t, err)

			_, err = trans.SendRequest(t.Context(), request)
			require.ErrorIs(t, err, ErrRedirectLeavesOrigin)
		})
		t.Run("sse to "+name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/sse":
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "event: endpoint\ndata: %s/message\n\n", server.URL)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				default:
					http.Redirect(w, r, target+r.URL.Path, http.StatusTemporaryRedirect)
				}
			}))
			t.Cleanup(server.Close)
			newSSE := func(path string) *SSE {
				trans, err := NewSSE(server.URL+path, WithOAuth(oauth(server.URL+path)))
				require.NoError(t, err)
				t.Cleanup(func() { _ = trans.Close() })
				return trans
			}

			require.ErrorIs(t, newSSE("/redirected").Start(t.Context()), ErrRedirectLeavesOrigin)
			trans := newSSE("/sse")
			require.NoError(t, trans.Start(t.Context()))
			_, err := trans.SendRequest(t.Context(), request)
			require.ErrorIs(t, err, ErrRedirectLeavesOrigin)
			err = trans.SendNotification(t.Context(), mcp.JSONRPCNotification{JSONRPC: mcp.JSONRPC_VERSION, Method: "notifications/initialized"})
			require.ErrorIs(t, err, ErrRedirectLeavesOrigin)
		})
	}
	t.Run("an application redirect policy that rewrites the hop", func(t *testing.T) {
		var server *httptest.Server
		server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, server.URL+"/next", http.StatusTemporaryRedirect)
		}))
		t.Cleanup(server.Close)
		client := &http.Client{CheckRedirect: func(req *http.Request, _ []*http.Request) error {
			req.URL.Host = strings.TrimPrefix(otherPort.URL, "https://")
			return nil
		}}
		trans, err := NewStreamableHTTP(server.URL+"/mcp", WithHTTPBasicClient(client), WithHTTPOAuth(oauth(server.URL+"/mcp")))
		require.NoError(t, err)

		_, err = trans.SendRequest(t.Context(), request)
		require.ErrorIs(t, err, ErrRedirectLeavesOrigin)
	})
	require.Zero(t, leaked.Load(), "a redirect sent the access token to another origin")

	t.Run("a redirect on the origin is followed", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/mcp/" {
				http.Redirect(w, r, "/mcp/", http.StatusPermanentRedirect)
				return
			}
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{}})
		}))
		t.Cleanup(server.Close)
		trans, err := NewStreamableHTTP(server.URL+"/mcp", WithHTTPOAuth(oauth(server.URL+"/mcp")))
		require.NoError(t, err)

		_, err = trans.SendRequest(t.Context(), request)
		require.NoError(t, err)
	})
	t.Run("without OAuth a redirect is followed as the client says", func(t *testing.T) {
		var followed atomic.Int32
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			followed.Add(1)
			if r.URL.Path == "/sse" {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, "event: endpoint\ndata: /message\n\n")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
		}))
		t.Cleanup(other.Close)
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
		}))
		t.Cleanup(server.Close)

		streamable, err := NewStreamableHTTP(server.URL+"/mcp", WithHTTPBasicClient(server.Client()))
		require.NoError(t, err)
		_, err = streamable.SendRequest(t.Context(), request)
		require.NoError(t, err)

		sse, err := NewSSE(server.URL+"/sse", WithHTTPClient(server.Client()))
		require.NoError(t, err)
		t.Cleanup(func() { _ = sse.Close() })
		require.NoError(t, sse.Start(t.Context()))
		require.Equal(t, int32(2), followed.Load())
	})
}

// An origin is scheme, host and port, a missing port being the scheme's
// default; a downgrade to http on the same host and port is another origin.
func TestSameOrigin(t *testing.T) {
	tests := []struct {
		a, b string
		same bool
	}{
		{"https://mcp.example/mcp", "https://mcp.example/mcp/", true},
		{"https://mcp.example/mcp", "https://MCP.example:443/other", true},
		{"http://mcp.example/mcp", "http://mcp.example:80/mcp", true},
		{"https://mcp.example/mcp", "http://mcp.example/mcp", false},
		{"https://mcp.example:8443/mcp", "http://mcp.example:8443/mcp", false},
		{"https://mcp.example/mcp", "https://mcp.example:8443/mcp", false},
		{"https://mcp.example/mcp", "https://evil.example/mcp", false},
		{"https://mcp.example/mcp", "https://sub.mcp.example/mcp", false},
	}
	for _, tt := range tests {
		a, err := url.Parse(tt.a)
		require.NoError(t, err)
		b, err := url.Parse(tt.b)
		require.NoError(t, err)
		require.Equal(t, tt.same, sameOrigin(a, b), "%s and %s", tt.a, tt.b)
	}
}
