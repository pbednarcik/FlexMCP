package client

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/mark3labs/mcp-go/mcp"
)

// Protocol version 2026-07-28 replaced the standalone notification stream and
// the resources/subscribe and resources/unsubscribe RPCs with a single
// subscriptions/listen request (SEP-2575).
//
// The client opts in to specific notification types; the server acknowledges
// what it established, tags every notification it delivers with the
// subscription ID, and holds the response stream open until the client
// cancels the request.

// subscriptionState tracks the client's pending and active subscription.
//
// generation identifies the Listen call that currently owns the stream. A
// later call supersedes an earlier one, and only the owner may clear the
// state, so a returning call cannot tear down a stream that replaced it.
type subscriptionState struct {
	mu         sync.Mutex
	filter     mcp.SubscriptionFilter
	cancel     context.CancelFunc
	generation uint64
}

// Listen opens a subscriptions/listen stream for the given notification
// types and blocks until the context is cancelled or the server closes the
// stream.
//
// Notifications are delivered to the handlers registered with
// [Client.OnNotification], exactly as they were on earlier protocol versions.
//
// The resource URIs recorded by [Client.Subscribe] are sent along with the
// filter's own, and stay recorded until [Client.Unsubscribe]; the filter's
// own URIs are this call's alone, so a later call with a narrower filter
// narrows. One stream is open at a time: a Listen call ends the stream of an
// earlier one.
//
// Listen is only available on connections using protocol version 2026-07-28 or
// later. On a legacy connection it returns an error: notifications arrive on
// the transport's own stream there, with no opt-in required.
func (c *Client) Listen(ctx context.Context, filter mcp.SubscriptionFilter) error {
	if !c.isModern() {
		return fmt.Errorf(
			"subscriptions/listen requires protocol version %s or later; "+
				"on this connection notifications are delivered without opting in",
			mcp.ProtocolVersion20260728)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The filter sent is the caller's plus the URIs Subscribe recorded, in
	// a slice of this call's own: the caller keeps its slice, nothing of the
	// caller's is recorded, and a Subscribe or Unsubscribe during the call
	// writes into neither.
	c.subscriptions.mu.Lock()
	filter.ResourceSubscriptions = unionURIs(filter.ResourceSubscriptions, c.subscriptions.filter.ResourceSubscriptions)
	if previous := c.subscriptions.cancel; previous != nil {
		previous()
	}
	c.subscriptions.cancel = cancel
	c.subscriptions.generation++
	generation := c.subscriptions.generation
	c.subscriptions.mu.Unlock()

	defer func() {
		c.subscriptions.mu.Lock()
		// Clear the stream only if this call still owns it. A concurrent
		// Listen may have superseded us, and its stream must survive our
		// return.
		if c.subscriptions.generation == generation {
			c.subscriptions.cancel = nil
		}
		c.subscriptions.mu.Unlock()
	}()

	params := mcp.SubscriptionsListenParams{Notifications: filter}
	response, err := c.sendRequest(ctx, string(mcp.MethodSubscriptionsListen), params, nil)
	if err != nil {
		return err
	}

	var result mcp.SubscriptionsListenResult
	if err := json.Unmarshal(*response, &result); err != nil {
		return fmt.Errorf("failed to unmarshal subscriptions/listen result: %w", err)
	}
	return c.checkResultType(result.ResultType)
}

// ListenAsync opens a subscriptions/listen stream in the background and
// returns a function that closes it.
//
// It is the convenient form of [Client.Listen] for callers that only want the
// notifications to start flowing. Errors from the stream are reported to
// onError, which may be nil.
func (c *Client) ListenAsync(
	ctx context.Context,
	filter mcp.SubscriptionFilter,
	onError func(error),
) (stop func(), err error) {
	if !c.isModern() {
		return nil, fmt.Errorf(
			"subscriptions/listen requires protocol version %s or later",
			mcp.ProtocolVersion20260728)
	}

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		err := c.Listen(ctx, filter)
		// The stream has stopped before its error is reported: done is closed
		// first, so the error handler may call the returned stop (upstream
		// #1070). stop does not wait for a running onError.
		close(done)
		if err != nil && ctx.Err() == nil && onError != nil {
			onError(err)
		}
	}()

	return func() {
		cancel()
		<-done
	}, nil
}

// Subscribe subscribes to updates for a resource.
//
// Protocol version 2026-07-28 removed the resources/subscribe RPC: resource
// subscriptions are declared in the filter of a subscriptions/listen request
// (SEP-2575). On a modern connection this method records the URI so that the
// next [Client.Listen] call includes it; it does not open the stream itself.
//
// Deprecated: use [Client.Listen] with a filter naming the resources of
// interest.
func (c *Client) Subscribe(
	ctx context.Context,
	request mcp.SubscribeRequest,
) error {
	if c.isModern() {
		c.subscriptions.mu.Lock()
		defer c.subscriptions.mu.Unlock()
		if !slices.Contains(c.subscriptions.filter.ResourceSubscriptions, request.Params.URI) {
			c.subscriptions.filter.ResourceSubscriptions = append(
				c.subscriptions.filter.ResourceSubscriptions, request.Params.URI)
		}
		return nil
	}
	_, err := c.sendRequest(ctx, string(mcp.MethodResourcesSubscribe), request.Params, outboundHeader(request.Header, request.Method))
	return err
}

// Unsubscribe removes a resource update subscription.
//
// Protocol version 2026-07-28 removed the resources/unsubscribe RPC. On a
// modern connection this method drops the URI from the filter used by the next
// [Client.Listen] call.
//
// Deprecated: close the [Client.Listen] stream, or reopen it with a narrower
// filter.
func (c *Client) Unsubscribe(
	ctx context.Context,
	request mcp.UnsubscribeRequest,
) error {
	if c.isModern() {
		c.subscriptions.mu.Lock()
		defer c.subscriptions.mu.Unlock()
		c.subscriptions.filter.ResourceSubscriptions = slices.DeleteFunc(
			c.subscriptions.filter.ResourceSubscriptions,
			func(uri string) bool { return uri == request.Params.URI },
		)
		return nil
	}
	_, err := c.sendRequest(ctx, string(mcp.MethodResourcesUnsubscribe), request.Params, outboundHeader(request.Header, request.Method))
	return err
}

// unionURIs is own followed by the recorded URIs not among them, each once,
// in a new slice.
func unionURIs(own, recorded []string) []string {
	union := make([]string, 0, len(own)+len(recorded))
	for _, uri := range own {
		if !slices.Contains(union, uri) {
			union = append(union, uri)
		}
	}
	for _, uri := range recorded {
		if !slices.Contains(union, uri) {
			union = append(union, uri)
		}
	}
	return union
}

// PendingSubscriptionFilter returns the resource URIs [Client.Subscribe]
// recorded, which the next [Client.Listen] call adds to its filter.
func (c *Client) PendingSubscriptionFilter() mcp.SubscriptionFilter {
	c.subscriptions.mu.Lock()
	defer c.subscriptions.mu.Unlock()
	filter := c.subscriptions.filter
	filter.ResourceSubscriptions = slices.Clone(filter.ResourceSubscriptions)
	return filter
}
