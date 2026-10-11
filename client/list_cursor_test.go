package client

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedPageTransport returns list pages in order, ignoring the requested
// cursor; the params of every request are kept as they were sent.
type scriptedPageTransport struct {
	mu     sync.Mutex
	method string
	pages  []json.RawMessage
	calls  int
	sent   []json.RawMessage
}

func (t *scriptedPageTransport) Start(context.Context) error { return nil }
func (t *scriptedPageTransport) SendNotification(context.Context, mcp.JSONRPCNotification) error {
	return nil
}
func (t *scriptedPageTransport) SetNotificationHandler(func(mcp.JSONRPCNotification)) {}
func (t *scriptedPageTransport) Close() error                                         { return nil }
func (t *scriptedPageTransport) GetSessionId() string                                 { return "session" }

func (t *scriptedPageTransport) SendRequest(
	_ context.Context,
	request transport.JSONRPCRequest,
) (*transport.JSONRPCResponse, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if request.Method != t.method {
		return nil, errors.New("unexpected method " + request.Method)
	}
	if t.calls >= len(t.pages) {
		return nil, errors.New("no more pages")
	}
	page := t.pages[t.calls]
	t.calls++
	params, err := json.Marshal(request.Params)
	if err != nil {
		return nil, err
	}
	t.sent = append(t.sent, params)
	return &transport.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      request.ID,
		Result:  page,
	}, nil
}

func (t *scriptedPageTransport) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

// params returns the params of the nth request (0-based) as they were sent.
func (t *scriptedPageTransport) params(n int) json.RawMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sent[n]
}

// cursor is a cursor a test puts in a request or a result.
func cursor(s string) *mcp.Cursor {
	c := mcp.Cursor(s)
	return &c
}

// An empty-string nextCursor is a cursor, not the end: the walk asks for the
// next page with that empty cursor, and only a missing nextCursor ends it.
func TestClient_ListFollowsAnEmptyCursor(t *testing.T) {
	t.Parallel()

	toolPages := []json.RawMessage{
		json.RawMessage(`{"tools":[{"name":"a"}],"nextCursor":""}`),
		json.RawMessage(`{"tools":[{"name":"b"}]}`),
	}
	taskPages := []json.RawMessage{
		json.RawMessage(`{"tasks":[{"taskId":"t1"}],"nextCursor":""}`),
		json.RawMessage(`{"tasks":[{"taskId":"t2"}]}`),
	}
	tests := []struct {
		name   string
		method string
		pages  []json.RawMessage
		count  func(*testing.T, context.Context, *Client) (int, error)
	}{
		{
			name: "tools", method: "tools/list", pages: toolPages,
			count: func(t *testing.T, ctx context.Context, c *Client) (int, error) {
				result, err := c.ListTools(ctx, mcp.ListToolsRequest{})
				if err != nil {
					return 0, err
				}
				assert.Nil(t, result.NextCursor, "a missing nextCursor is the end")
				return len(result.Tools), nil
			},
		},
		{
			name: "tools iterator", method: "tools/list", pages: toolPages,
			count: func(_ *testing.T, ctx context.Context, c *Client) (int, error) {
				n := 0
				for _, err := range c.IterTools(ctx, mcp.ListToolsRequest{}) {
					if err != nil {
						return 0, err
					}
					n++
				}
				return n, nil
			},
		},
		{
			name: "tasks", method: "tasks/list", pages: taskPages,
			count: func(_ *testing.T, ctx context.Context, c *Client) (int, error) {
				result, err := c.ListTasks(ctx, mcp.ListTasksRequest{})
				if err != nil {
					return 0, err
				}
				return len(result.Tasks), nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tr := &scriptedPageTransport{method: tt.method, pages: append([]json.RawMessage(nil), tt.pages...)}
			c := NewClient(tr, WithSession())

			got, err := tt.count(t, t.Context(), c)
			require.NoError(t, err)
			assert.Equal(t, 2, got, "both pages were walked")
			require.Equal(t, 2, tr.callCount())
			var params map[string]any
			require.NoError(t, json.Unmarshal(tr.params(1), &params))
			cursor, sent := params["cursor"]
			assert.True(t, sent, "the empty cursor is sent, not dropped: %s", tr.params(1))
			assert.Equal(t, "", cursor)
		})
	}
}

func TestClient_ListFollowsAdvancingCursor(t *testing.T) {
	tr := &scriptedPageTransport{
		method: "tools/list",
		pages: []json.RawMessage{
			json.RawMessage(`{"tools":[{"name":"a"}],"nextCursor":"page-2"}`),
			json.RawMessage(`{"tools":[{"name":"b"}]}`),
		},
	}
	c := NewClient(tr, WithSession())

	result, err := c.ListTools(t.Context(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	require.Len(t, result.Tools, 2)
	assert.Empty(t, result.NextCursor)
	assert.Equal(t, 2, tr.callCount())
}

func TestClient_ListRejectsRepeatedCursor(t *testing.T) {
	t.Parallel()

	repeated := []json.RawMessage{
		json.RawMessage(`{"nextCursor":"page-2"}`),
		json.RawMessage(`{"nextCursor":"page-2"}`),
	}
	cycled := []json.RawMessage{
		json.RawMessage(`{"nextCursor":"page-2"}`),
		json.RawMessage(`{"nextCursor":"page-3"}`),
		json.RawMessage(`{"nextCursor":"page-2"}`),
	}

	tests := []struct {
		name   string
		method string
		pages  []json.RawMessage
		calls  int
		list   func(context.Context, *Client) error
	}{
		{
			name:   "tools same cursor",
			method: "tools/list",
			pages:  repeated,
			calls:  2,
			list: func(ctx context.Context, c *Client) error {
				_, err := c.ListTools(ctx, mcp.ListToolsRequest{})
				return err
			},
		},
		{
			name:   "tools cursor cycles",
			method: "tools/list",
			pages:  cycled,
			calls:  3,
			list: func(ctx context.Context, c *Client) error {
				_, err := c.ListTools(ctx, mcp.ListToolsRequest{})
				return err
			},
		},
		{
			name:   "resources same cursor",
			method: "resources/list",
			pages:  repeated,
			calls:  2,
			list: func(ctx context.Context, c *Client) error {
				_, err := c.ListResources(ctx, mcp.ListResourcesRequest{})
				return err
			},
		},
		{
			name:   "templates same cursor",
			method: "resources/templates/list",
			pages:  repeated,
			calls:  2,
			list: func(ctx context.Context, c *Client) error {
				_, err := c.ListResourceTemplates(ctx, mcp.ListResourceTemplatesRequest{})
				return err
			},
		},
		{
			name:   "prompts same cursor",
			method: "prompts/list",
			pages:  repeated,
			calls:  2,
			list: func(ctx context.Context, c *Client) error {
				_, err := c.ListPrompts(ctx, mcp.ListPromptsRequest{})
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			pages := append([]json.RawMessage(nil), tt.pages...)
			tr := &scriptedPageTransport{method: tt.method, pages: pages}
			c := NewClient(tr, WithSession())

			err := tt.list(t.Context(), c)
			require.ErrorIs(t, err, ErrRepeatedListCursor)
			assert.Equal(t, tt.calls, tr.callCount())
		})
	}
}

func TestIter_RejectsRepeatedCursor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		walk   func(context.Context, *Client) error
	}{
		{
			name:   "tools",
			method: "tools/list",
			walk: func(ctx context.Context, c *Client) error {
				for _, err := range c.IterTools(ctx, mcp.ListToolsRequest{}) {
					if err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			name:   "resources",
			method: "resources/list",
			walk: func(ctx context.Context, c *Client) error {
				for _, err := range c.IterResources(ctx, mcp.ListResourcesRequest{}) {
					if err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			name:   "templates",
			method: "resources/templates/list",
			walk: func(ctx context.Context, c *Client) error {
				for _, err := range c.IterResourceTemplates(ctx, mcp.ListResourceTemplatesRequest{}) {
					if err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			name:   "prompts",
			method: "prompts/list",
			walk: func(ctx context.Context, c *Client) error {
				for _, err := range c.IterPrompts(ctx, mcp.ListPromptsRequest{}) {
					if err != nil {
						return err
					}
				}
				return nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tr := &scriptedPageTransport{
				method: tt.method,
				pages: []json.RawMessage{
					json.RawMessage(`{"nextCursor":"page-2"}`),
					json.RawMessage(`{"nextCursor":"page-2"}`),
				},
			}
			c := NewClient(tr, WithSession())

			err := tt.walk(t.Context(), c)
			require.ErrorIs(t, err, ErrRepeatedListCursor)
			assert.Equal(t, 2, tr.callCount())
		})
	}
}
