package client

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pbednarcik/flexmcp/client/transport"
	"github.com/pbednarcik/flexmcp/mcp"
	"github.com/pbednarcik/flexmcp/server"
)

// TestListenWithTaskIDsNeedsTheDeclaredExtension pins that Listen asking for
// task pushes refuses, before anything is sent, on a client that does not
// declare the Tasks extension, as the extension's task methods do; a filter
// without taskIds, or a declared client, is sent.
func TestListenWithTaskIDsNeedsTheDeclaredExtension(t *testing.T) {
	for _, c := range []struct {
		name    string
		options []ClientOption
		filter  mcp.SubscriptionFilter
		refused bool
	}{
		{name: "taskIds, not declared", filter: mcp.SubscriptionFilter{TaskIDs: []string{"t1"}}, refused: true},
		{name: "taskIds, declared", options: []ClientOption{withTasksExtension()}, filter: mcp.SubscriptionFilter{TaskIDs: []string{"t1"}}},
		{name: "no taskIds, not declared", filter: mcp.SubscriptionFilter{ToolsListChanged: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tr := &scriptedTransport{respond: modernScript(func(_ context.Context, request transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
				return resultResponse(request, `{"resultType":"complete"}`), nil
			})}
			client := connectScripted(t, tr, c.options...)

			err := client.Listen(t.Context(), c.filter)

			listened := slices.Contains(tr.sentMethods(), string(mcp.MethodSubscriptionsListen))
			if c.refused {
				require.ErrorIs(t, err, ErrExtensionTasksNotDeclared)
				assert.False(t, listened, "subscriptions/listen was sent")
				return
			}
			require.NoError(t, err)
			assert.True(t, listened, "subscriptions/listen was not sent")
		})
	}
}

// listenTasks is the smallest server.TaskHandler: it knows no task.
type listenTasks struct{}

func (listenTasks) TaskState(context.Context, server.TaskRef) (server.TaskState, error) {
	return server.TaskState{}, server.ErrTaskGone
}

func (listenTasks) CancelTask(context.Context, server.TaskRef) error { return server.ErrTaskGone }

// TestInProcessListenTaskIDs pins the in-process path of a listen asking for
// task pushes from a server serving Tasks: a request that does not declare the
// extension is refused with -32021, and a declared client's Listen is
// acknowledged without taskIds and returns once the stream closes.
func TestInProcessListenTaskIDs(t *testing.T) {
	newServer := func() *server.MCPServer {
		return server.NewMCPServer("test", "1.0.0", server.WithToolCapabilities(true), server.WithTaskHandler(listenTasks{}))
	}

	t.Run("not declared", func(t *testing.T) {
		tr := transport.NewInProcessTransport(newServer())
		require.NoError(t, tr.Start(t.Context()))
		t.Cleanup(func() { _ = tr.Close() })
		response, err := tr.SendRequest(t.Context(), transport.JSONRPCRequest{
			JSONRPC: mcp.JSONRPC_VERSION,
			ID:      mcp.NewRequestId(int64(1)),
			Method:  string(mcp.MethodSubscriptionsListen),
			Params: map[string]any{
				"notifications": map[string]any{"taskIds": []string{"t1"}},
				"_meta": map[string]any{
					mcp.MetaKeyProtocolVersion:    mcp.ProtocolVersion20260728,
					mcp.MetaKeyClientInfo:         map[string]any{"name": "c", "version": "1"},
					mcp.MetaKeyClientCapabilities: map[string]any{},
				},
			},
		})
		require.NoError(t, err)
		require.NotNil(t, response.Error, "the listen was answered %s", response.Result)
		assert.Equal(t, mcp.MISSING_REQUIRED_CLIENT_CAPABILITY, response.Error.Code)
	})

	t.Run("declared", func(t *testing.T) {
		c := connectInProcess(t, newServer(), mcp.ProtocolVersion20260728, withTasksExtension())
		acknowledged := make(chan mcp.JSONRPCNotification, 1)
		c.OnNotification(func(notification mcp.JSONRPCNotification) {
			if notification.Method == mcp.MethodNotificationSubscriptionsAcknowledged {
				acknowledged <- notification
			}
		})
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		require.NoError(t, c.Listen(ctx, mcp.SubscriptionFilter{TaskIDs: []string{"t1"}}))
		require.NoError(t, ctx.Err(), "Listen returned only at its deadline")
		select {
		case ack := <-acknowledged:
			granted, err := json.Marshal(ack.Params.AdditionalFields["notifications"])
			require.NoError(t, err)
			assert.JSONEq(t, `{}`, string(granted), "the acknowledgement grants a task push")
		case <-time.After(time.Second):
			t.Fatal("no acknowledgement")
		}
	})
}
