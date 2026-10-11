package client

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedTransport answers each request with whatever respond returns and
// records the methods it was asked, in order.
type scriptedTransport struct {
	mu            sync.Mutex
	methods       []string
	notifications []string
	respond       func(ctx context.Context, call int, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error)
	notifyErr     error
}

func (t *scriptedTransport) Start(context.Context) error { return nil }

func (t *scriptedTransport) SendRequest(ctx context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	t.mu.Lock()
	call := len(t.methods)
	t.methods = append(t.methods, request.Method)
	t.mu.Unlock()
	return t.respond(ctx, call, request)
}

func (t *scriptedTransport) SendNotification(_ context.Context, notification mcp.JSONRPCNotification) error {
	t.mu.Lock()
	t.notifications = append(t.notifications, notification.Method)
	t.mu.Unlock()
	return t.notifyErr
}

func (t *scriptedTransport) sentNotifications() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.notifications)
}

func (t *scriptedTransport) SetNotificationHandler(func(mcp.JSONRPCNotification)) {}
func (t *scriptedTransport) Close() error                                         { return nil }
func (t *scriptedTransport) GetSessionId() string                                 { return "" }

func (t *scriptedTransport) sentMethods() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.methods)
}

func resultResponse(request transport.JSONRPCRequest, result string) *transport.JSONRPCResponse {
	return &transport.JSONRPCResponse{JSONRPC: mcp.JSONRPC_VERSION, ID: request.ID, Result: json.RawMessage(result)}
}

const (
	legacyInitializeResult = `{"protocolVersion":"` + mcp.LATEST_LEGACY_PROTOCOL_VERSION + `","capabilities":{},"serverInfo":{"name":"scripted","version":"1.0.0"}}`
	discoverResult         = `{"supportedVersions":["` + mcp.ProtocolVersion20260728 + `"],"capabilities":{},"serverInfo":{"name":"scripted","version":"1.0.0"}}`
)

// modernScript answers server/discover so that Initialize settles on the
// stateless core, and hands every other request to then.
func modernScript(then func(ctx context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error)) func(context.Context, int, transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	return func(ctx context.Context, _ int, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		if request.Method == string(mcp.MethodServerDiscover) {
			return resultResponse(request, discoverResult), nil
		}
		return then(ctx, request)
	}
}

// connectScripted starts and initializes a client over tr.
func connectScripted(t *testing.T, tr *scriptedTransport, options ...ClientOption) *Client {
	t.Helper()
	c := NewClient(tr, options...)
	require.NoError(t, c.Start(t.Context()))
	_, err := c.Initialize(t.Context(), mcp.InitializeRequest{})
	require.NoError(t, err)
	return c
}

// unansweredDiscover never answers server/discover, like a server predating
// the method that ignores it, and completes the initialize handshake.
func unansweredDiscover(ctx context.Context, _ int, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	if request.Method == string(mcp.MethodServerDiscover) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return resultResponse(request, legacyInitializeResult), nil
}

func TestWithDiscoverTimeoutBoundsTheProbe(t *testing.T) {
	tests := []struct {
		name     string
		options  []ClientOption
		wantWait time.Duration
	}{
		{name: "the default bound is five seconds", wantWait: 5 * time.Second},
		{name: "WithDiscoverTimeout shortens it", options: []ClientOption{WithDiscoverTimeout(50 * time.Millisecond)}, wantWait: 50 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				tr := &scriptedTransport{respond: unansweredDiscover}
				c := NewClient(tr, tt.options...)
				require.NoError(t, c.Start(t.Context()))

				started := time.Now()
				result, err := c.Initialize(t.Context(), mcp.InitializeRequest{})
				require.NoError(t, err)

				assert.Equal(t, tt.wantWait, time.Since(started))
				assert.Equal(t, mcp.LATEST_LEGACY_PROTOCOL_VERSION, result.ProtocolVersion)
				assert.Equal(t, []string{string(mcp.MethodServerDiscover), string(mcp.MethodInitialize)}, tr.sentMethods())
			})
		})
	}
}

func TestANegativeDiscoverTimeoutWaitsOnTheCallersContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		tr := &scriptedTransport{respond: unansweredDiscover}
		c := NewClient(tr, WithDiscoverTimeout(-1))
		require.NoError(t, c.Start(t.Context()))

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			_, err := c.Initialize(ctx, mcp.InitializeRequest{})
			done <- err
		}()

		time.Sleep(time.Hour)
		assert.Equal(t, []string{string(mcp.MethodServerDiscover)}, tr.sentMethods(), "the probe is still waiting")

		cancel()
		require.ErrorIs(t, <-done, context.Canceled)
		assert.False(t, c.IsInitialized())
	})
}

func TestInitializeFailsWhenTheInitializedNotificationIsNotSent(t *testing.T) {
	notifyErr := errors.New("pipe closed")
	tr := &scriptedTransport{
		notifyErr: notifyErr,
		respond: func(_ context.Context, _ int, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
			return resultResponse(request, legacyInitializeResult), nil
		},
	}
	c := NewClient(tr, WithLegacyProtocolOnly())
	require.NoError(t, c.Start(t.Context()))

	_, err := c.Initialize(t.Context(), mcp.InitializeRequest{})
	require.ErrorIs(t, err, notifyErr)
	assert.ErrorContains(t, err, "failed to send initialized notification")
	assert.False(t, c.IsInitialized())
}

func TestRootListChangesReportsATransportFailure(t *testing.T) {
	notifyErr := errors.New("pipe closed")
	c := NewClient(&scriptedTransport{notifyErr: notifyErr}, WithSession())

	err := c.RootListChanges(t.Context())
	require.ErrorIs(t, err, notifyErr)
	assert.ErrorContains(t, err, "root list change notification")
}

// listWalkers drives every list method that follows nextCursor, keyed by the
// method it sends, with the first page each one is scripted to receive.
var listWalkers = map[string]struct {
	firstPage string
	walk      func(context.Context, *Client) error
}{
	string(mcp.MethodResourcesList): {
		firstPage: `{"resources":[{"uri":"test://a","name":"a"}],"nextCursor":"2"}`,
		walk: func(ctx context.Context, c *Client) error {
			_, err := c.ListResources(ctx, mcp.ListResourcesRequest{})
			return err
		},
	},
	string(mcp.MethodResourcesTemplatesList): {
		firstPage: `{"resourceTemplates":[{"uriTemplate":"test://{id}","name":"t"}],"nextCursor":"2"}`,
		walk: func(ctx context.Context, c *Client) error {
			_, err := c.ListResourceTemplates(ctx, mcp.ListResourceTemplatesRequest{})
			return err
		},
	},
	string(mcp.MethodPromptsList): {
		firstPage: `{"prompts":[{"name":"p"}],"nextCursor":"2"}`,
		walk: func(ctx context.Context, c *Client) error {
			_, err := c.ListPrompts(ctx, mcp.ListPromptsRequest{})
			return err
		},
	},
	string(mcp.MethodToolsList): {
		firstPage: `{"tools":[{"name":"t"}],"nextCursor":"2"}`,
		walk: func(ctx context.Context, c *Client) error {
			_, err := c.ListTools(ctx, mcp.ListToolsRequest{})
			return err
		},
	},
	string(mcp.MethodTasksList): {
		firstPage: `{"tasks":[{"taskId":"t1"}],"nextCursor":"2"}`,
		walk: func(ctx context.Context, c *Client) error {
			_, err := c.ListTasks(ctx, mcp.ListTasksRequest{})
			return err
		},
	},
}

func TestListWalkersStopOnAFailingPage(t *testing.T) {
	pageErr := errors.New("page 2 unavailable")
	for method, walker := range listWalkers {
		t.Run(method, func(t *testing.T) {
			tr := &scriptedTransport{respond: func(_ context.Context, call int, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
				if call == 0 {
					return resultResponse(request, walker.firstPage), nil
				}
				return nil, pageErr
			}}

			err := walker.walk(t.Context(), NewClient(tr, WithSession()))
			require.ErrorIs(t, err, pageErr)
			assert.Equal(t, []string{method, method}, tr.sentMethods())
		})
	}
}

func TestListWalkersStopWhenTheContextEndsBetweenPages(t *testing.T) {
	for method, walker := range listWalkers {
		t.Run(method, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			tr := &scriptedTransport{respond: func(_ context.Context, _ int, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
				cancel()
				return resultResponse(request, walker.firstPage), nil
			}}

			err := walker.walk(ctx, NewClient(tr, WithSession()))
			require.ErrorIs(t, err, context.Canceled)
			assert.Equal(t, []string{method}, tr.sentMethods(), "the next page is not requested")
		})
	}
}

// iterators collects every error each paginated iterator yields.
var iterators = map[string]func(context.Context, *Client) []error{
	"tools": func(ctx context.Context, c *Client) []error {
		var errs []error
		for _, err := range c.IterTools(ctx, mcp.ListToolsRequest{}) {
			errs = append(errs, err)
		}
		return errs
	},
	"resources": func(ctx context.Context, c *Client) []error {
		var errs []error
		for _, err := range c.IterResources(ctx, mcp.ListResourcesRequest{}) {
			errs = append(errs, err)
		}
		return errs
	},
	"resource templates": func(ctx context.Context, c *Client) []error {
		var errs []error
		for _, err := range c.IterResourceTemplates(ctx, mcp.ListResourceTemplatesRequest{}) {
			errs = append(errs, err)
		}
		return errs
	},
	"prompts": func(ctx context.Context, c *Client) []error {
		var errs []error
		for _, err := range c.IterPrompts(ctx, mcp.ListPromptsRequest{}) {
			errs = append(errs, err)
		}
		return errs
	},
}

func TestIteratorsYieldAFailingPageOnce(t *testing.T) {
	pageErr := errors.New("list unavailable")
	for name, collect := range iterators {
		t.Run(name, func(t *testing.T) {
			tr := &scriptedTransport{respond: func(context.Context, int, transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
				return nil, pageErr
			}}

			errs := collect(t.Context(), NewClient(tr, WithSession()))
			require.Len(t, errs, 1)
			assert.ErrorIs(t, errs[0], pageErr)
			assert.Len(t, tr.sentMethods(), 1)
		})
	}
}

func TestIteratorsYieldAnEndedContextWithoutListing(t *testing.T) {
	for name, collect := range iterators {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			tr := &scriptedTransport{respond: func(_ context.Context, _ int, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
				return resultResponse(request, `{}`), nil
			}}

			errs := collect(ctx, NewClient(tr, WithSession()))
			require.Len(t, errs, 1)
			assert.ErrorIs(t, errs[0], context.Canceled)
			assert.Empty(t, tr.sentMethods())
		})
	}
}

func TestMalformedResultsAreReportedAsErrors(t *testing.T) {
	nope := func(_ context.Context, _ int, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
		return resultResponse(request, `"nope"`), nil
	}
	tests := []struct {
		name string
		call func(t *testing.T) error
	}{
		{name: "tools/list", call: func(t *testing.T) error {
			_, err := NewClient(&scriptedTransport{respond: nope}, WithSession()).ListTools(t.Context(), mcp.ListToolsRequest{})
			return err
		}},
		{name: "completion/complete", call: func(t *testing.T) error {
			_, err := NewClient(&scriptedTransport{respond: nope}, WithSession()).Complete(t.Context(), mcp.CompleteRequest{})
			return err
		}},
		{name: "initialize", call: func(t *testing.T) error {
			_, err := NewClient(&scriptedTransport{respond: nope}, WithLegacyProtocolOnly()).Initialize(t.Context(), mcp.InitializeRequest{})
			return err
		}},
		{name: "server/discover", call: func(t *testing.T) error {
			_, err := NewClient(&scriptedTransport{respond: nope}).Discover(t.Context(), mcp.DiscoverRequest{})
			return err
		}},
		{name: "subscriptions/listen", call: func(t *testing.T) error {
			tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
				return resultResponse(request, `"nope"`), nil
			})}
			return connectScripted(t, tr).Listen(t.Context(), mcp.SubscriptionFilter{ToolsListChanged: true})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call(t)
			require.Error(t, err)
			assert.ErrorContains(t, err, "unmarshal")
		})
	}
}
